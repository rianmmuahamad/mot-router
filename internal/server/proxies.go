package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rian/mot-router/internal/relay"
	"github.com/rian/mot-router/internal/store"
	"github.com/rian/mot-router/internal/zen"
)

// rotateMu serializes relay selection so concurrent requests do not all pick the
// same relay and burn the same budget.
var rotateMu sync.Mutex

// relayIndex tracks which relay a rotation cycle last handed out.
var relayIndex int

// deployMu serializes Worker deploys, which are slow and rate-limited upstream.
var deployMu sync.Mutex

// relayFor picks the next relay to use.
//
// With one active relay the behaviour is "sticky": the same relay is reused, so
// prompt caching and session affinity are preserved. With several, selection
// round-robins so load and per-IP budgets spread across them.
func (s *Server) relayFor(ctx context.Context) *store.ProxyPool {
	pools, err := s.store.ActiveProxyPools()
	if err != nil || len(pools) == 0 {
		return nil
	}
	if len(pools) == 1 {
		return &pools[0]
	}

	rotateMu.Lock()
	defer rotateMu.Unlock()
	chosen := pools[relayIndex%len(pools)]
	relayIndex++
	return &chosen
}

// nextRelayAfter returns the relay following `current`, used when a relay just
// failed or the upstream reported a rate limit.
func (s *Server) nextRelayAfter(current *store.ProxyPool) *store.ProxyPool {
	pools, err := s.store.ActiveProxyPools()
	if err != nil || len(pools) == 0 {
		return nil
	}
	if current == nil {
		return &pools[0]
	}
	for i := range pools {
		if pools[i].ID == current.ID {
			return &pools[(i+1)%len(pools)]
		}
	}
	return &pools[0]
}

// relayHTTPClient builds an HTTP client that routes through a relay.
func relayHTTPClient(pool *store.ProxyPool) *http.Client {
	return &http.Client{
		Transport: relay.NewTransport(pool.URL),
		// No timeout: generations stream for as long as the model needs.
	}
}

// isRateLimited reports whether an upstream failure looks like a per-IP limit,
// which is the signal to rotate the egress IP.
func isRateLimited(status int, detail string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	lower := strings.ToLower(detail)
	for _, needle := range []string{
		"rate limit", "rate_limit", "ratelimit", "too many requests",
		"429", "quota", "capacity", "resource_exhausted", "resource has been exhausted",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// ---- dashboard API ----

// handleListProxies returns the configured relays plus rotation status.
func (s *Server) handleListProxies(w http.ResponseWriter, r *http.Request) {
	pools, err := s.store.ListProxyPools()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	active := 0
	for _, p := range pools {
		if p.Active {
			active++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pools":         pools,
		"active_count":  active,
		"rotate_on_429": s.rotateOnLimit(),
	})
}

// handleCreateProxy manually registers a relay URL.
func (s *Server) handleCreateProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Type    string `json:"type"`
		NoProxy string `json:"no_proxy"`
		Active  *bool  `json:"active"`
		Strict  bool   `json:"strict"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	if body.URL == "" {
		writeAPIError(w, http.StatusBadRequest, "url is required", "invalid_request_error")
		return
	}
	if !strings.HasPrefix(body.URL, "http://") && !strings.HasPrefix(body.URL, "https://") {
		body.URL = "https://" + body.URL
	}
	if body.Name == "" {
		body.Name = body.URL
	}
	if body.Type == "" {
		body.Type = "custom"
	}
	active := true
	if body.Active != nil {
		active = *body.Active
	}

	pool, err := s.store.CreateProxyPool(&store.ProxyPool{
		Name: body.Name, URL: body.URL, Type: body.Type,
		NoProxy: body.NoProxy, Active: active, Strict: body.Strict,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusCreated, pool)
}

// handleDeployCloudflare deploys a Cloudflare Worker relay.
func (s *Server) handleDeployCloudflare(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID    string `json:"account_id"`
		APIToken     string `json:"api_token"`
		WorkerName   string `json:"worker_name"`
		Subdomain    string `json:"subdomain"`
		AutoActivate *bool  `json:"auto_activate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.AccountID) == "" || strings.TrimSpace(body.APIToken) == "" {
		writeAPIError(w, http.StatusBadRequest,
			"Cloudflare Account ID and API Token are required", "invalid_request_error")
		return
	}

	// Deploys are slow and rate-limited; keep them serial.
	deployMu.Lock()
	defer deployMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	result, err := relay.Deploy(ctx, relay.DeployConfig{
		AccountID:  body.AccountID,
		APIToken:   body.APIToken,
		WorkerName: body.WorkerName,
		Subdomain:  body.Subdomain,
	})
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error(), "deploy_error")
		return
	}

	activate := true
	if body.AutoActivate != nil {
		activate = *body.AutoActivate
	}

	pool, err := s.store.CreateProxyPool(&store.ProxyPool{
		Name:   result.Name,
		URL:    result.URL,
		Type:   "cloudflare",
		Active: activate,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			"worker deployed at "+result.URL+" but saving it failed: "+err.Error(), "api_error")
		return
	}

	// Record whether it actually forwards traffic, so a broken deploy is visible
	// immediately instead of on the first real request.
	status, elapsed, testErr := relay.Test(ctx, result.URL, 20*time.Second)
	_ = s.store.SetProxyPoolTest(pool.ID, testErr == nil)

	response := map[string]any{
		"pool":       pool,
		"deploy_url": result.URL,
		"test": map[string]any{
			"ok":         testErr == nil,
			"status":     status,
			"elapsed_ms": elapsed.Milliseconds(),
		},
	}
	if testErr != nil {
		response["test_error"] = testErr.Error()
	}
	writeJSON(w, http.StatusCreated, response)
}

// handleTestProxy checks one relay's connectivity and egress IP.
func (s *Server) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid pool id", "invalid_request_error")
		return
	}
	pool, err := s.store.GetProxyPool(id)
	if err != nil || pool == nil {
		writeAPIError(w, http.StatusNotFound, "proxy pool not found", "not_found_error")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	status, elapsed, testErr := relay.Test(ctx, pool.URL, 20*time.Second)
	_ = s.store.SetProxyPoolTest(pool.ID, testErr == nil)

	out := map[string]any{
		"ok":         testErr == nil,
		"status":     status,
		"elapsed_ms": elapsed.Milliseconds(),
	}
	if ip, ipErr := relay.EgressIP(ctx, pool.URL, 20*time.Second); ipErr == nil {
		out["egress_ip"] = ip
	}
	if testErr != nil {
		out["error"] = testErr.Error()
		_ = s.store.SetProxyPoolError(pool.ID, testErr.Error())
	}
	writeJSON(w, http.StatusOK, out)
}

// handleToggleProxy enables or disables a relay.
func (s *Server) handleToggleProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid pool id", "invalid_request_error")
		return
	}
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	ok, err := s.store.SetProxyPoolActive(id, body.Active)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if !ok {
		writeAPIError(w, http.StatusNotFound, "proxy pool not found", "not_found_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteProxy removes a relay.
func (s *Server) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid pool id", "invalid_request_error")
		return
	}
	ok, err := s.store.DeleteProxyPool(id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if !ok {
		writeAPIError(w, http.StatusNotFound, "proxy pool not found", "not_found_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRotateNow forces the rotation pointer to advance, for manual control.
func (s *Server) handleRotateNow(w http.ResponseWriter, r *http.Request) {
	pools, err := s.store.ActiveProxyPools()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if len(pools) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "reason": "no active relays configured"})
		return
	}
	rotateMu.Lock()
	relayIndex++
	current := pools[relayIndex%len(pools)]
	rotateMu.Unlock()

	// Report the relay's egress IP so the rotation is observable, but do not fail
	// the rotation if the lookup times out.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	ip := ""
	if v, err := relay.EgressIP(ctx, current.URL, 15*time.Second); err == nil {
		ip = v
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "now_using": current.Name, "url": current.URL, "egress_ip": ip,
	})
}

// rotateOnLimit reports whether automatic rotation on a rate limit is enabled.
func (s *Server) rotateOnLimit() bool {
	return s.store.GetSetting("rotate_on_limit") != "0"
}

// handleSetRotation toggles automatic rotation.
func (s *Server) handleSetRotation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	value := "0"
	if body.Enabled {
		value = "1"
	}
	if err := s.store.SetSetting("rotate_on_limit", value); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": body.Enabled})
}

// describeRelay is a helper for error messages.
func describeRelay(p *store.ProxyPool) string {
	if p == nil {
		return "direct"
	}
	return fmt.Sprintf("%s (%s)", p.Name, p.URL)
}

// streamZenWithRotation sends a request through a relay, and on a per-IP rate
// limit retries once through the next relay.
//
// Returns the event channel, the upstream status, an error, and the relay used.
func (s *Server) streamZenWithRotation(
	ctx context.Context,
	model, sessionSeed string,
	body map[string]any,
	rec requestRecord,
) (<-chan zen.Event, int, error, *store.ProxyPool) {
	pool := s.relayFor(ctx)

	stream := func(p *store.ProxyPool) (<-chan zen.Event, int, error) {
		client := s.zen
		if p != nil {
			client = s.zen.WithHTTP(relayHTTPClient(p))
		}
		events, status, err := client.Stream(ctx, model, sessionSeed, body)
		if p != nil {
			if err != nil && isRateLimited(status, err.Error()) {
				_ = s.store.SetProxyPoolError(p.ID, err.Error())
			} else if err == nil {
				_ = s.store.TouchProxyPool(p.ID)
			}
		}
		return events, status, err
	}

	events, status, err := stream(pool)
	if err == nil {
		return events, status, nil, pool
	}

	// Rotate only for a rate limit, and only when rotation is enabled.
	if pool == nil || !s.rotateOnLimit() || !isRateLimited(status, err.Error()) {
		return events, status, err, pool
	}

	next := s.nextRelayAfter(pool)
	if next == nil || next.ID == pool.ID {
		return events, status, err, pool
	}

	events, status, err = stream(next)
	return events, status, err, next
}
