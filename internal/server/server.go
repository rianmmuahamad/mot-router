// Package server wires HTTP routing, authentication, translation, and the
// dashboard API for Mot Router.
package server

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/mihomo"
	"github.com/rian/mot-router/internal/store"
	"github.com/rian/mot-router/internal/translate"
	"github.com/rian/mot-router/internal/zen"
)

//go:embed dashboard.html
var dashboardFS embed.FS

// Provider identifiers.
const (
	ProviderZen         = "opencode-zen"
	ProviderTokenHarbor = "tokenharbor"
	ProviderCline       = "cline"
	ProviderCodeBuddy   = "codebuddy"
)

// Config holds runtime configuration.
type Config struct {
	Host         string
	Port         int
	DataDir      string
	DefaultModel string
	UpstreamKey  string
	// DefaultProvider selects the upstream when a request does not name one.
	DefaultProvider string
}

// Server holds the wired dependencies.
type Server struct {
	cfg     Config
	store   *store.Store
	zen     *zen.Client
	dataDir string
	// gate caches the probed upstream request shape. It is refreshed in the
	// background so a change upstream is absorbed without a restart.
	gate *gate.Cache

	// mihomoCached holds the long-lived proxy client. It must not be rebuilt per
	// request: it owns the pooled transport, so a fresh client each time leaks
	// connections until the proxy starts refusing them.
	mihomoMu     sync.Mutex
	mihomoCached *mihomo.Client
	mihomoKey    string

	// nodeMu guards nodePenalty, which records nodes that recently failed in a
	// way that follows the exit IP.
	nodeMu sync.Mutex
	// nodePenalty maps "group|node" to the time it was last rejected. A node
	// that Zen rate limits is healthy by mihomo's measure (the tunnel works) yet
	// useless to us, so mihomo's own health check cannot filter it. Recording
	// the rejection here lets rotation skip it for a while.
	nodePenalty map[string]time.Time

	// exitIPMu guards the node→exit-IP map read from the ipfarm state file.
	exitIPMu    sync.Mutex
	exitIPCache map[string]string
	exitIPAt    time.Time
}

// exitIPCacheTTL bounds how stale the node→exit-IP map may be.
const exitIPCacheTTL = 30 * time.Second

// defaultIpFarmState is where ipfarm writes the node table when no override is
// configured.
const defaultIpFarmState = "/var/lib/ipfarm/state.tsv"

// settingIpFarmState overrides the path of the ipfarm state file.
const settingIpFarmState = "ipfarm_state"

// nodePenaltyTTL is how long a rejected node is avoided.
//
// Zen's free tier rate limit follows the exit IP and lasts for hours, not
// minutes: an IP that has exhausted its budget stays refused for the rest of
// the window. The earlier 10-minute value was thirty times too short, so
// rotation walked straight back onto a still-limited IP, collected another
// rejection, and repeated — burning the client's time on known-losing requests
// while adding load upstream.
//
// Five hours matches the observed window. It is configurable through
// `node_penalty_minutes` so an operator can tune it without a rebuild when the
// upstream window changes.
const nodePenaltyTTL = 5 * time.Hour

// settingNodePenaltyMinutes overrides nodePenaltyTTL when set.
const settingNodePenaltyMinutes = "node_penalty_minutes"

// penalizeNode records that a node failed in an IP-bound way.
func (s *Server) penalizeNode(group, node string) {
	if node == "" {
		return
	}
	s.nodeMu.Lock()
	defer s.nodeMu.Unlock()
	if s.nodePenalty == nil {
		s.nodePenalty = map[string]time.Time{}
	}
	s.nodePenalty[group+"|"+node] = time.Now()
}

// penaltyTTL returns how long a rejected node is avoided, honouring the
// `node_penalty_minutes` setting when present.
func (s *Server) penaltyTTL() time.Duration {
	if s.store != nil {
		if v := s.store.GetSetting(settingNodePenaltyMinutes); v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return time.Duration(n) * time.Minute
			}
		}
	}
	return nodePenaltyTTL
}

// penalizedNodes returns the nodes of a group currently being avoided.
func (s *Server) penalizedNodes(group string) map[string]bool {
	s.nodeMu.Lock()
	defer s.nodeMu.Unlock()
	out := map[string]bool{}
	now := time.Now()
	ttl := s.penaltyTTL()
	for k, at := range s.nodePenalty {
		// Expire lazily so the map does not grow without bound.
		if now.Sub(at) > ttl {
			delete(s.nodePenalty, k)
			continue
		}
		if strings.HasPrefix(k, group+"|") {
			out[strings.TrimPrefix(k, group+"|")] = true
		}
	}
	return out
}

// nodeIPPenalized reports whether a node is backed by an exit IP that is
// currently penalised under a different node name.
//
// Zen rate limits the exit IP, not the node label. Two nodes behind one IP are
// therefore equally unusable, and skipping only the exact name would let
// rotation walk onto the sibling and collect the same rejection. The node→IP
// mapping comes from the ipfarm state file, which records each node's observed
// egress address; with no state available the check degrades to per-node
// filtering, which is still correct but weaker.
func (s *Server) nodeIPPenalized(group, node string, avoid map[string]bool) bool {
	ips := s.nodeExitIPs()
	if len(ips) == 0 {
		return false
	}
	ip := ips[node]
	if ip == "" {
		return false
	}
	for name := range avoid {
		if ips[name] == ip {
			return true
		}
	}
	return false
}

// nodeExitIPs maps emitted node names to their observed exit IP, read from the
// ipfarm state file. The result is cached briefly because rotation runs inline
// with a client request.
func (s *Server) nodeExitIPs() map[string]string {
	s.exitIPMu.Lock()
	defer s.exitIPMu.Unlock()
	if s.exitIPCache != nil && time.Since(s.exitIPAt) < exitIPCacheTTL {
		return s.exitIPCache
	}

	path := ""
	if s.store != nil {
		path = s.store.GetSetting(settingIpFarmState)
	}
	if path == "" {
		path = defaultIpFarmState
	}
	out := map[string]string{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		first := true
		for sc.Scan() {
			line := sc.Text()
			if first {
				first = false
				if strings.HasPrefix(line, "name\t") {
					continue
				}
			}
			fields := strings.Split(line, "\t")
			// name proto host port exit_ip zen_ms ok fail penalized_until
			if len(fields) < 5 {
				continue
			}
			if fields[4] != "" {
				out[fields[0]] = fields[4]
			}
		}
		f.Close()
	}
	s.exitIPCache = out
	s.exitIPAt = time.Now()
	return out
}

// New builds a server.
func New(cfg Config, st *store.Store, zc *zen.Client, gc *gate.Cache) *Server {
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = ProviderZen
	}
	s := &Server{cfg: cfg, store: st, zen: zc, gate: gc, dataDir: cfg.DataDir}
	if gc != nil {
		var saved gate.Profile
		if raw := st.GetSetting("gate.verified_profile.v1"); len(raw) > 0 && len(raw) <= 65536 && json.Unmarshal([]byte(raw), &saved) == nil {
			gc.Restore(&saved)
		}
		// Read immutable snapshots instead of racing a profile pointer update
		// against requests and transport clones.
		zc.GateProvider = func() *zen.GateProfile { return zen.ProfileFromGate(gc.Snapshot()) }
		gc.OnUpdate = func(p *gate.Profile) {
			if p.Source == "probe" && p.Verdict == "ok" {
				if raw, err := json.Marshal(p); err == nil && len(raw) <= 65536 {
					_ = st.SetSetting("gate.verified_profile.v1", string(raw))
				}
			}
		}
	}
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		s.handleProxy(w, r, "anthropic")
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.handleProxy(w, r, "openai")
	})
	mux.HandleFunc("POST /v1/systemone", s.handleSystemOne)
	mux.HandleFunc("POST /zen/v1/systemone", s.handleSystemOne)
	mux.HandleFunc("GET /v1/models", s.handleModels)

	mux.HandleFunc("GET /api/keys", s.handleListKeys)
	mux.HandleFunc("POST /api/keys", s.handleCreateKey)
	mux.HandleFunc("DELETE /api/keys/{id}", s.handleDeleteKey)
	mux.HandleFunc("POST /api/keys/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		s.handleSetRevoked(w, r, true)
	})
	mux.HandleFunc("POST /api/keys/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		s.handleSetRevoked(w, r, false)
	})

	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/quota", s.handleQuotaStats)
	mux.HandleFunc("GET /api/requests", s.handleRequests)
	mux.HandleFunc("DELETE /api/requests", s.handleClearRequests)
	mux.HandleFunc("GET /api/models", s.handleModelCatalog)
	mux.HandleFunc("GET /api/models/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.zen.CatalogStatus()) })
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/playground", s.handlePlayground)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)

	mux.HandleFunc("GET /api/providers", s.handleListProviders)
	mux.HandleFunc("DELETE /api/providers/{id}", s.handleDeleteProvider)
	mux.HandleFunc("PUT /api/providers/default", s.handleSetDefaultProvider)
	mux.HandleFunc("PUT /api/providers/tokenharbor", s.handleSetTokenHarbor)
	mux.HandleFunc("PUT /api/providers/cline", s.handleSetCline)
	mux.HandleFunc("POST /api/providers/codebuddy/login", s.handleCodeBuddyLogin)
	mux.HandleFunc("POST /api/providers/codebuddy/poll", s.handleCodeBuddyPoll)
	mux.HandleFunc("GET /api/providers/codebuddy/accounts", s.handleCodeBuddyAccounts)
	mux.HandleFunc("POST /api/providers/codebuddy/accounts/{id}/select", s.handleSelectCodeBuddyAccount)
	mux.HandleFunc("DELETE /api/providers/codebuddy/accounts/{id}", s.handleDeleteCodeBuddyAccount)
	mux.HandleFunc("GET /api/providers/codebuddy/quota", s.handleCodeBuddyQuota)
	mux.HandleFunc("GET /api/providers/codebuddy/models", s.handleCodeBuddyModels)
	mux.HandleFunc("PUT /api/providers/codebuddy", s.handleSetCodeBuddy)

	mux.HandleFunc("GET /api/proxies", s.handleListProxies)
	mux.HandleFunc("POST /api/proxies", s.handleCreateProxy)
	mux.HandleFunc("POST /api/proxies/cloudflare", s.handleDeployCloudflare)
	mux.HandleFunc("POST /api/proxies/rotate", s.handleRotateNow)
	mux.HandleFunc("PUT /api/proxies/rotation", s.handleSetRotation)

	mux.HandleFunc("GET /api/mihomo", s.handleMihomoStatus)
	mux.HandleFunc("PUT /api/mihomo", s.handleMihomoSettings)
	mux.HandleFunc("POST /api/mihomo/rotate", s.handleMihomoRotate)
	mux.HandleFunc("POST /api/mihomo/select", s.handleMihomoSelect)
	mux.HandleFunc("POST /api/mihomo/test", s.handleMihomoTest)

	mux.HandleFunc("GET /api/gate", s.handleGateStatus)
	mux.HandleFunc("POST /api/gate/refresh", s.handleGateRefresh)
	mux.HandleFunc("POST /api/gate/test", s.handleGateTest)
	mux.HandleFunc("POST /api/proxies/{id}/test", s.handleTestProxy)
	mux.HandleFunc("POST /api/proxies/{id}/toggle", s.handleToggleProxy)
	mux.HandleFunc("DELETE /api/proxies/{id}", s.handleDeleteProxy)
	mux.HandleFunc("GET /", s.handleDashboard)

	return withCORS(mux)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, message, errType string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

// extractKey accepts `Authorization: Bearer`, `x-api-key`, or `?key=`.
func extractKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return r.URL.Query().Get("key")
}

func describeClient(r *http.Request) string {
	ua := r.Header.Get("User-Agent")
	switch {
	case strings.HasPrefix(ua, "claude-cli"):
		return "claude-code"
	case strings.HasPrefix(ua, "opencode"):
		return "opencode"
	case strings.HasPrefix(ua, "curl"):
		return "curl"
	case strings.Contains(ua, "python"):
		return "python"
	case strings.Contains(ua, "node"):
		return "node"
	}
	if i := strings.Index(ua, "/"); i > 0 {
		ua = ua[:i]
	}
	if len(ua) > 24 {
		ua = ua[:24]
	}
	if ua == "" {
		return "unknown"
	}
	return ua
}

// resolveModel honors explicit aliases and keeps an explicitly selected model.
// A missing credential or a failed catalog lookup must never switch a request
// to the default model. The default is only for an omitted model.
func (s *Server) resolveModel(ctx context.Context, requested string) string {
	if requested == "" {
		return s.DefaultModel()
	}
	if alias := s.store.GetSetting("alias:" + requested); alias != "" {
		return alias
	}
	// OpenCode publishes separate paid and free DeepSeek IDs. Existing clients
	// selecting the bare ID without a paid upstream key mean the free offering.
	if requested == "deepseek-v4.1-flash" && s.zen.UpstreamKey == "" {
		return "deepseek-v4.1-flash-free"
	}
	return requested
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request, surface string) {
	keyStr := extractKey(r)
	if keyStr == "" {
		writeAPIError(w, http.StatusUnauthorized, "missing API key", "authentication_error")
		return
	}
	key, err := s.store.FindValidKey(keyStr)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if key == nil {
		writeAPIError(w, http.StatusUnauthorized, "invalid API key", "authentication_error")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "could not read body", "invalid_request_error")
		return
	}
	defer r.Body.Close()

	client := describeClient(r)

	// The model the client asked for, before any routing or alias resolution.
	// Logged verbatim so a surprising upstream model can always be traced back.
	requestedModel := requestedModelFrom(raw)
	if requestedModel == "" {
		requestedModel = s.DefaultModel()
	}

	// A `provider/` prefix selects the upstream; otherwise the default applies.
	provider, bareModel := s.providerFor(r.Context(), requestedModel)
	if provider == ProviderTokenHarbor {
		s.handleTokenHarborProxy(w, r, surface, raw, bareModel, requestedModel, key, client)
		return
	}
	if provider == ProviderCline {
		s.handleClineProxy(w, r, surface, raw, bareModel, requestedModel, key, client)
		return
	}
	if provider == ProviderCodeBuddy {
		s.handleCodeBuddyProxy(w, r, surface, raw, bareModel, requestedModel, key, client)
		return
	}

	started := time.Now()
	var body map[string]any
	var toolMap *zen.ToolInjection

	resolvedModel := s.resolveModel(r.Context(), bareModel)
	profile := s.zen.CurrentGate()
	r = r.WithContext(zen.WithGateSnapshot(r.Context(), profile))
	body, toolMap, err = s.translateZenBodyWithProfile(raw, surface, resolvedModel, profile)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	// A recovered profile must reshape the original request and replace the
	// response tool mapping together, so client tool calls retain their names.
	r = r.WithContext(context.WithValue(r.Context(), gateRebuildKey{}, func(profile *zen.GateProfile) (map[string]any, error) {
		var rebuildErr error
		body, toolMap, rebuildErr = s.translateZenBodyWithProfile(raw, surface, resolvedModel, profile)
		return body, rebuildErr
	}))

	// The translator writes the resolved id into the body; read it back so the
	// log records exactly what went upstream.
	model, _ := body["model"].(string)
	if model == "" {
		model = bareModel
	}

	base := requestRecord{
		Key:            key,
		Surface:        surface,
		Provider:       ProviderZen,
		Model:          model,
		RequestedModel: requestedModel,
		Started:        started,
		Client:         client,
	}

	_ = s.store.TouchKey(key.ID)

	sessionSeed := fmt.Sprintf("%d:%s:%s", key.ID, client, model)
	// Prefer mihomo when configured: it rotates the egress IP on a rate limit,
	// which is the only mechanism that actually resets Zen's per-IP quota.
	events, status, err, relay := s.mihomoStreamZen(r.Context(), model, sessionSeed, body, base)
	base.Relay = relay

	// A spent per-model allowance cannot be fixed by another exit IP: the
	// limit is scoped to (egress, model). When the monitor published a model
	// that still has allowance, serve the request with it instead of failing.
	if err != nil && status == http.StatusTooManyRequests {
		if fallback := s.fallbackModelForLimit(r.Context(), model); fallback != "" {
			fallbackBody, fallbackMap, buildErr := s.translateZenBodyWithProfile(raw, surface, fallback, profile)
			if buildErr == nil {
				if fallbackModel, ok := fallbackBody["model"].(string); ok {
					body = fallbackBody
					toolMap = fallbackMap
					model = fallbackModel
					base.Model = fallbackModel
					base.Relay = relay + " -> fallback:" + fallback
					events, status, err, relay = s.mihomoStreamZen(r.Context(), model, sessionSeed, body, base)
				}
			}
		}
	}

	if err != nil {
		base.Status = status
		if base.Status < 400 {
			base.Status = http.StatusBadGateway
		}
		base.Err = err.Error()
		s.recordRequest(base)
		// Surface the upstream status so a client can back off correctly; only
		// fall back to 502 when the upstream never answered.
		httpStatus := http.StatusBadGateway
		if status >= 400 && status < 600 {
			httpStatus = status
		}
		var upstream *zen.UpstreamError
		if errors.As(err, &upstream) && upstream.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64((upstream.RetryAfter+time.Second-1)/time.Second), 10))
		}
		writeAPIError(w, httpStatus, err.Error(), "upstream_error")
		return
	}

	events, outcome := zen.ObserveStream(r.Context(), events)
	finishRecord := func() {
		base.Status = http.StatusOK
		if outcome.Failure != nil {
			base.Status = outcome.Failure.HTTPStatus()
			base.Err = outcome.Failure.Message
		}
		s.recordRequest(base)
	}

	if surface == "anthropic" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		writer := translate.NewAnthropicStreamWriter(w, model)
		translate.StreamAnthropic(events, writer, toolMap)
		base.In, base.Out = writer.Usage()
		finishRecord()
		return
	}

	// OpenAI surface: honour the client's stream preference.
	streamRequested := true
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.Stream != nil {
		streamRequested = *probe.Stream
	}

	if !streamRequested {
		completion := translate.CollectOpenAI(events, model, toolMap)
		base.In, base.Out = usageFrom(completion)
		finishRecord()
		if outcome.Failure != nil {
			writeJSON(w, outcome.Failure.HTTPStatus(), zen.ErrorBody(outcome.Failure))
			return
		}
		writeJSON(w, http.StatusOK, completion)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	usage := translate.StreamOpenAI(w, events, model, toolMap)
	if usage != nil {
		base.In, base.Out = usage.PromptTokens, usage.CompletionTokens
	}
	finishRecord()
}

func usageFrom(completion map[string]any) (int64, int64) {
	u, ok := completion["usage"].(*translate.OpenAIUsage)
	if !ok || u == nil {
		return 0, 0
	}
	return u.PromptTokens, u.CompletionTokens
}

// requestRecord is the log entry for one proxied request.
type requestRecord struct {
	Key            *store.APIKey
	Surface        string
	Provider       string
	Model          string // resolved model actually sent upstream
	RequestedModel string // model the client asked for
	Status         int
	Started        time.Time
	In             int64
	Out            int64
	Err            string
	Client         string
	// Relay is the proxy pool used for this request, or "direct".
	Relay string
}

func (s *Server) recordRequest(rec requestRecord) {
	keyID := rec.Key.ID
	keyName := rec.Key.Name
	if err := s.store.LogRequest(store.LogInput{
		KeyID:          &keyID,
		KeyName:        &keyName,
		Surface:        rec.Surface,
		Model:          rec.Model,
		RequestedModel: rec.RequestedModel,
		Provider:       rec.Provider,
		Status:         rec.Status,
		LatencyMs:      time.Since(rec.Started).Milliseconds(),
		InputTokens:    rec.In,
		OutputTokens:   rec.Out,
		Error:          rec.Err,
		Client:         rec.Client,
		Relay:          rec.Relay,
	}); err != nil {
		log.Printf("log request: %v", err)
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.zen.ListModels(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error(), "api_error")
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m.ID, "object": "model", "created": m.Created, "owned_by": "opencode",
		})
	}
	data = append(data,
		map[string]any{"id": "cline/glm-5.2", "object": "model", "owned_by": "cline"},
		map[string]any{"id": "zai/glm-5.2", "object": "model", "owned_by": "cline"},
		map[string]any{"id": "cline-free/gemini-3.8-flash", "object": "model", "owned_by": "cline"},
		map[string]any{"id": "cline-free/deepseek-v4.1-flash", "object": "model", "owned_by": "cline"},
		map[string]any{"id": "stealth/space-bunny-alpha", "object": "model", "owned_by": "cline"},
		map[string]any{"id": "cline-free/mimo-v2.6-flash", "object": "model", "owned_by": "cline"},
		map[string]any{"id": "qwen3.8-flash:free", "object": "model", "owned_by": "tokenharbor"},
		map[string]any{"id": "deepseek-v4.1-flash:free", "object": "model", "owned_by": "tokenharbor"},
		map[string]any{"id": "mimo-v2.6-flash:free", "object": "model", "owned_by": "tokenharbor"},
	)
	for _, model := range codeBuddyModels {
		data = append(data, map[string]any{
			"id": "codebuddy/" + model.ID, "object": "model", "owned_by": ProviderCodeBuddy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListKeys()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Name == "" {
		body.Name = "untitled"
	}
	key, err := s.store.CreateKey(body.Name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusCreated, key)
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid key id", "invalid_request_error")
		return
	}
	ok, err := s.store.DeleteKey(id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if !ok {
		writeAPIError(w, http.StatusNotFound, "key not found", "not_found_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSetRevoked(w http.ResponseWriter, r *http.Request, revoked bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid key id", "invalid_request_error")
		return
	}
	ok, err := s.store.SetKeyRevoked(id, revoked)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if !ok {
		writeAPIError(w, http.StatusNotFound, "key not found", "not_found_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	rows, err := s.store.RecentRequests(limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleClearRequests(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearLogs(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleModelCatalog merges every provider's catalog. `?all=1` returns a
// uniform shape including the provider tag, used by the dashboard.
func (s *Server) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") == "1" {
		out := []map[string]any{}
		if models, err := s.zen.ListModels(r.Context()); err == nil {
			for _, m := range models {
				out = append(out, map[string]any{
					"id":       ProviderZen + "/" + m.ID,
					"provider": ProviderZen,
					"free":     strings.HasSuffix(m.ID, "-free"),
				})
			}
		}
		out = append(out, s.gatewayModels()...)
		writeJSON(w, http.StatusOK, out)
		return
	}

	models, err := s.zen.ListModels(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusOK, models)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	base := "http://" + net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoint":                base + "/v1",
		"anthropic_endpoint":      base,
		"default_model":           s.DefaultModel(),
		"upstream":                "https://opencode.ai/zen/v1/responses",
		"user_agent":              s.zen.UserAgent,
		"upstream_key_configured": s.zen.UpstreamKey != "",
	})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListKeys()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	aliases, err := s.store.ListSettings("alias:")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default_model":    s.DefaultModel(),
		"default_provider": s.effectiveProvider(),
		"upstream":         s.zen.EffectiveBaseURL(s.DefaultModel()) + "/responses",
		"key_count":        len(keys),
		"aliases":          aliases,
	})
}

// handlePutSettings persists the default model and model aliases. Aliases let a
// client model name (e.g. `claude-sonnet-4-5`) be pinned to any Zen model.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Alias        string `json:"alias"`
		AliasModel   string `json:"alias_model"`
		DefaultModel string `json:"default_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	if body.Alias != "" && body.AliasModel != "" {
		if err := s.store.SetSetting("alias:"+body.Alias, body.AliasModel); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
			return
		}
	}
	if body.DefaultModel != "" {
		if err := s.store.SetSetting("default_model", body.DefaultModel); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePlayground runs a single completion straight from the dashboard. It
// routes through whichever provider the model name selects, so the dashboard
// exercises the same path a real client would.
func (s *Server) handlePlayground(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	requested := orDefault(body.Model, s.DefaultModel())
	prompt := orDefault(body.Prompt, "hi")

	// A dashboard test must never hang: the user is staring at a spinner, and an
	// upstream that accepts the connection then goes silent would leave it there
	// indefinitely.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	// providerFor strips any `provider/` prefix; `model` is therefore already
	// bare and safe to hand to either upstream.
	provider, model := s.providerFor(ctx, requested)

	started := time.Now()

	if provider == ProviderTokenHarbor {
		text, in, out, err := s.runTokenHarborPlayground(ctx, model, prompt)
		if err != nil {
			s.logPlayground(provider, model, requested, http.StatusBadGateway, started, err.Error())
			writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
			return
		}
		s.logPlayground(provider, model, requested, http.StatusOK, started, "")
		writeJSON(w, http.StatusOK, map[string]any{
			"text":  text,
			"usage": map[string]int64{"input_tokens": in, "output_tokens": out},
		})
		return
	}
	if provider == ProviderCline {
		text, in, out, err := s.runClinePlayground(ctx, model, prompt)
		if err != nil {
			s.logPlayground(provider, model, requested, http.StatusBadGateway, started, err.Error())
			writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
			return
		}
		s.logPlayground(provider, model, requested, http.StatusOK, started, "")
		writeJSON(w, http.StatusOK, map[string]any{
			"text":  text,
			"usage": map[string]int64{"input_tokens": in, "output_tokens": out},
		})
		return
	}
	if provider == ProviderCodeBuddy {
		text, in, out, err := s.runCodeBuddyPlayground(ctx, model, prompt)
		if err != nil {
			s.logPlayground(provider, model, requested, http.StatusBadGateway, started, err.Error())
			writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
			return
		}
		s.logPlayground(provider, model, requested, http.StatusOK, started, "")
		writeJSON(w, http.StatusOK, map[string]any{
			"text":  text,
			"usage": map[string]int64{"input_tokens": in, "output_tokens": out},
		})
		return
	}

	// Apply explicit aliases and the DeepSeek free-variant compatibility alias.
	// Never substitute the default for a model the caller already selected.
	model = s.resolveModel(r.Context(), model)

	tools := playgroundTools()
	profile := s.zen.CurrentGate()
	ctx = zen.WithGateSnapshot(ctx, profile)
	if zen.IsFreeModel(model) {
		tools, _ = zen.PrepareTools(tools, profile)
	}
	zenBody := map[string]any{
		"model": model,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": prompt}},
		}},
		"max_output_tokens": 2000,
		"store":             false,
		"stream":            true,
		"tools":             tools,
		"tool_choice":       "auto",
	}

	events, status, err, _ := s.mihomoStreamZen(ctx, model, "playground", zenBody, requestRecord{})
	if err != nil {
		s.logPlayground(provider, model, requested, status, started, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		// Without this return the handler would fall through and range over a nil
		// channel, hanging the request forever.
		return
	}

	events, outcome := zen.ObserveStream(ctx, events)
	text := ""
	var in, out int64
	for ev := range events {
		switch ev.Type {
		case "response.output_text.delta":
			text += ev.Delta
		case "response.completed":
			if ev.Response != nil && ev.Response.Usage != nil {
				in, out = ev.Response.Usage.InputTokens, ev.Response.Usage.OutputTokens
			}
		}
	}

	if outcome.Failure != nil {
		s.logPlayground(provider, model, requested, outcome.Failure.HTTPStatus(), started, outcome.Failure.Message)
		writeJSON(w, http.StatusOK, map[string]any{"error": outcome.Failure.Message})
		return
	}
	s.logPlayground(provider, model, requested, http.StatusOK, started, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"text":  text,
		"usage": map[string]int64{"input_tokens": in, "output_tokens": out},
	})
}

// logPlayground records a dashboard test request, including the provider that
// served it and the model the user actually selected.
func (s *Server) logPlayground(provider, model, requestedModel string, status int, started time.Time, errMsg string) {
	name := "playground"
	client := "dashboard"
	_ = s.store.LogRequest(store.LogInput{
		KeyName:        &name,
		Surface:        "playground",
		Provider:       provider,
		Model:          model,
		RequestedModel: requestedModel,
		Status:         status,
		LatencyMs:      time.Since(started).Milliseconds(),
		Error:          errMsg,
		Client:         client,
	})
}

func playgroundTools() []zen.Tool {
	tools := make([]zen.Tool, 0, len(zen.RequiredTools))
	for _, name := range zen.RequiredTools {
		tools = append(tools, zen.Tool{
			Type:        "function",
			Name:        name,
			Description: "d",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	return tools
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// DefaultModel returns the effective default model, honouring a value persisted
// from the dashboard over the one supplied at startup.
func (s *Server) DefaultModel() string {
	if v := s.store.GetSetting("default_model"); v != "" {
		return v
	}
	return s.cfg.DefaultModel
}

// requestedModelFrom extracts the client-requested model from a raw body.
// Both protocols use a top-level `model` field, so one probe covers both.
func requestedModelFrom(raw []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.Model != "" {
		return probe.Model
	}
	return ""
}
