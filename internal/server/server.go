// Package server wires HTTP routing, authentication, translation, and the
// dashboard API for Mot Router.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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
)

// Config holds runtime configuration.
type Config struct {
	Host         string
	Port         int
	DefaultModel string
	UpstreamKey  string
	// DefaultProvider selects the upstream when a request does not name one.
	DefaultProvider string
}

// Server holds the wired dependencies.
type Server struct {
	cfg         Config
	store       *store.Store
	zen         *zen.Client
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
}

// nodePenaltyTTL is how long a rejected node is avoided.
//
// Long enough to move past a rate-limited exit, short enough that a node which
// recovers is usable again without a restart.
const nodePenaltyTTL = 10 * time.Minute

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

// penalizedNodes returns the nodes of a group currently being avoided.
func (s *Server) penalizedNodes(group string) map[string]bool {
	s.nodeMu.Lock()
	defer s.nodeMu.Unlock()
	out := map[string]bool{}
	now := time.Now()
	for k, at := range s.nodePenalty {
		// Expire lazily so the map does not grow without bound.
		if now.Sub(at) > nodePenaltyTTL {
			delete(s.nodePenalty, k)
			continue
		}
		if strings.HasPrefix(k, group+"|") {
			out[strings.TrimPrefix(k, group+"|")] = true
		}
	}
	return out
}

// clearNodePenalty forgets a node's rejection, called after it succeeds.
func (s *Server) clearNodePenalty(group, node string) {
	if node == "" {
		return
	}
	s.nodeMu.Lock()
	defer s.nodeMu.Unlock()
	delete(s.nodePenalty, group+"|"+node)
}

// New builds a server.
func New(cfg Config, st *store.Store, zc *zen.Client, gc *gate.Cache) *Server {
	if cfg.DefaultProvider == "" {
		cfg.DefaultProvider = ProviderZen
	}
	s := &Server{cfg: cfg, store: st, zen: zc, gate: gc}
	if gc != nil {
		// Push each new profile into the Zen client so header and tool shaping
		// follow the probe.
		gc.OnUpdate = func(p *gate.Profile) {
			zc.Gate = zen.ProfileFromGate(p)
		}
		if snap := gc.Snapshot(); snap != nil {
			zc.Gate = zen.ProfileFromGate(snap)
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
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/playground", s.handlePlayground)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)

	mux.HandleFunc("GET /api/providers", s.handleListProviders)
	mux.HandleFunc("DELETE /api/providers/{id}", s.handleDeleteProvider)
	mux.HandleFunc("PUT /api/providers/default", s.handleSetDefaultProvider)
	mux.HandleFunc("PUT /api/providers/tokenharbor", s.handleSetTokenHarbor)
	mux.HandleFunc("PUT /api/providers/cline", s.handleSetCline)

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

func (s *Server) resolveModel(ctx context.Context, requested string) string {
	if alias := s.store.GetSetting("alias:" + requested); alias != "" {
		return alias
	}
	low := strings.ToLower(requested)
	if strings.Contains(low, "glm") {
		return requested
	}
	if strings.Contains(low, "qwen3.8-flash") || strings.Contains(low, "deepseek-v4.1-flash") || strings.Contains(low, "mimo-v2.6") {
		return requested
	}
	if zen.IsFreeModel(requested) {
		return requested
	}
	if s.zen.UpstreamKey == "" {
		return s.DefaultModel()
	}
	models, err := s.zen.ListModels(ctx)
	if err != nil {
		return s.DefaultModel()
	}
	for _, m := range models {
		if m.ID == requested {
			return requested
		}
	}
	return s.DefaultModel()
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


	started := time.Now()
	var body map[string]any
	var toolMap *zen.ToolInjection

	if surface == "anthropic" {
		var req translate.AnthropicRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
			return
		}
		result, err := translate.AnthropicToZen(&req, s.resolveModel(r.Context(), bareModel), s.zen.Gate)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "translation failed: "+err.Error(), "invalid_request_error")
			return
		}
		body, toolMap = result.Body, result.ToolMap
	} else {
		var req translate.OpenAIChatRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
			return
		}
		result, err := translate.OpenAIToZen(&req, s.resolveModel(r.Context(), bareModel), s.zen.Gate)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "translation failed: "+err.Error(), "invalid_request_error")
			return
		}
		body, toolMap = result.Body, result.ToolMap
	}

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
	events, status, err, mihomoGroupName := s.mihomoStreamZen(r.Context(), model, sessionSeed, body, base)
	if mihomoGroupName != "" {
		base.Relay = "mihomo:" + mihomoGroupName
	} else {
		var pool *store.ProxyPool
		events, status, err, pool = s.streamZenWithRotation(r.Context(), model, sessionSeed, body, base)
		base.Relay = describeRelay(pool)
	}
	if err != nil {
		base.Status = status
		base.Err = err.Error()
		s.recordRequest(base)
		// Surface the upstream status so a client can back off correctly; only
		// fall back to 502 when the upstream never answered.
		httpStatus := http.StatusBadGateway
		if status >= 400 && status < 600 {
			httpStatus = status
		}
		writeAPIError(w, httpStatus, err.Error(), "upstream_error")
		return
	}

	if surface == "anthropic" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		writer := translate.NewAnthropicStreamWriter(w, model)
		translate.StreamAnthropic(events, writer, toolMap)
		base.In, base.Out = writer.Usage()
		base.Status = http.StatusOK
		s.recordRequest(base)
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
		base.Status = http.StatusOK
		s.recordRequest(base)
		writeJSON(w, http.StatusOK, completion)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	chunks := translate.StreamOpenAI(events, model, toolMap)
	usage := translate.WriteOpenAIStream(w, chunks)
	if usage != nil {
		base.In, base.Out = usage.PromptTokens, usage.CompletionTokens
	}
	base.Status = http.StatusOK
	s.recordRequest(base)
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
		map[string]any{"id": "cline-free/muse-spark-1.3-contributor", "object": "model", "owned_by": "cline"},
		map[string]any{"id": "qwen3.8-flash:free", "object": "model", "owned_by": "tokenharbor"},
		map[string]any{"id": "deepseek-v4.1-flash:free", "object": "model", "owned_by": "tokenharbor"},
		map[string]any{"id": "mimo-v2.6-flash-free", "object": "model", "owned_by": "opencode"},
		map[string]any{"id": "space-bunny-free", "object": "model", "owned_by": "opencode"},
		map[string]any{"id": "jev-1.13-free", "object": "model", "owned_by": "opencode"},
	)
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
		"upstream":         s.zen.BaseURL + "/responses",
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

	// Resolve against the Zen catalog: a paid model with no upstream credential
	// falls back to the configured free default instead of failing.
	model = s.resolveModel(r.Context(), model)

	zenBody := map[string]any{
		"model": model,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": prompt}},
		}},
		"max_output_tokens": 2000,
		"store":             false,
		"stream":            true,
		"tools":             playgroundTools(),
		"tool_choice":       "auto",
	}

	events, status, err := s.zen.Stream(ctx, model, "playground", zenBody)
	if err != nil {
		s.logPlayground(provider, model, requested, status, started, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		// Without this return the handler would fall through and range over a nil
		// channel, hanging the request forever.
		return
	}

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
