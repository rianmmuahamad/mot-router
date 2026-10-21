package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/zen"
)

// gateStatus is the dashboard view of the probed upstream request shape.
type gateStatus struct {
	UpstreamURL     string               `json:"upstream_url"`
	ProbeModel      string               `json:"probe_model"`
	ClientFormat    string               `json:"client_format"`
	VerifiedVersion string               `json:"verified_version,omitempty"`
	Automatic       gate.AutomaticStatus `json:"automatic"`
	Identity        gate.IdentityPolicy  `json:"identity"`
	Verdict         string               `json:"verdict"`
	Source          string               `json:"source"`
	CheckedAt       int64                `json:"checked_at"`
	LatencyMs       int64                `json:"latency_ms"`
	// AgeSeconds is how long ago the profile was verified.
	AgeSeconds int64 `json:"age_seconds"`
	// Refreshing is true while a probe is running.
	Refreshing bool `json:"refreshing"`

	UserAgent      string   `json:"user_agent"`
	MinVersion     string   `json:"min_version"`
	SessionPattern string   `json:"session_pattern"`
	RequiresStream bool     `json:"requires_stream"`
	RequiredTools  []string `json:"required_tools"`
	AcceptsSuper   bool     `json:"accepts_tool_superset"`

	Conditions []gate.Condition `json:"conditions"`

	// Adaptations summarises what the router is doing differently from the
	// client's raw request, so the operator can see the adaptation at a glance.
	Adaptations []string `json:"adaptations"`
}

// handleGateStatus reports the current gate profile and probe history.
func (s *Server) handleGateStatus(w http.ResponseWriter, r *http.Request) {
	if s.gate == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"reason":  "gate probing is disabled",
		})
		return
	}
	s.prepareGate()
	prof := s.gate.Snapshot()
	writeJSON(w, http.StatusOK, s.buildGateStatus(prof))
}

func (s *Server) buildGateStatus(prof *gate.Profile) gateStatus {
	if prof == nil {
		return gateStatus{Verdict: "unknown", Source: "none"}
	}

	st := gateStatus{
		UpstreamURL:     s.zen.EffectiveBaseURL(s.gate.ProbeModel()),
		ProbeModel:      s.gate.ProbeModel(),
		ClientFormat:    prof.ClientFormat,
		VerifiedVersion: prof.VerifiedVersion,
		Automatic:       s.gate.Automatic(),
		Identity:        prof.Identity.Normalized(),
		Verdict:         prof.Verdict,
		Source:          prof.Source,
		CheckedAt:       prof.CheckedAt.UnixMilli(),
		LatencyMs:       prof.LatencyMs,
		AgeSeconds:      int64(time.Since(prof.CheckedAt).Seconds()),
		UserAgent:       prof.UserAgent,
		MinVersion:      prof.MinVersion,
		SessionPattern:  sessionPattern(prof),
		RequiresStream:  prof.RequiresStream,
		RequiredTools:   prof.RequiredTools,
		AcceptsSuper:    prof.AcceptsToolSuperset,
		Conditions:      prof.Conditions,
	}
	st.Refreshing = st.Automatic.Refreshing

	// Describe what the router enforces on every free-tier request.
	ad := []string{}
	if prof.UserAgent != "" {
		ad = append(ad, "User-Agent diset ke "+prof.UserAgent)
	}
	if prof.SessionPrefix != "" {
		ad = append(ad, "session id dibentuk ulang sesuai pola "+sessionPattern(prof))
	}
	if prof.RequiresStream {
		ad = append(ad, `body dipaksa "stream": true`)
	}
	if len(prof.RequiredTools) > 0 {
		ad = append(ad, "tool wajib disuntik bila klien tidak punya: "+strings.Join(prof.RequiredTools, ", "))
	}
	if len(prof.ToolDescriptions) > 0 {
		ad = append(ad, "deskripsi tool wajib mengikuti profil yang terverifikasi; schema tool klien dipertahankan")
	}
	if prof.AcceptsToolSuperset {
		ad = append(ad, "tool tambahan ditoleransi, jadi tool asli klien tetap diteruskan")
	}
	st.Adaptations = ad
	return st
}

func sessionPattern(p *gate.Profile) string {
	prefix := p.SessionPrefix
	if prefix == "" {
		prefix = "ses_"
	}
	hexLen, alnumLen := p.SessionHex, p.SessionAlnum
	if hexLen == 0 {
		hexLen = 12
	}
	if alnumLen == 0 {
		alnumLen = 14
	}
	return prefix + strings.Repeat("h", hexLen) + strings.Repeat("a", alnumLen)
}

// ProbeGate runs a probe on the proxied path. Exposed so startup uses the same
// egress as real traffic instead of probing the rate-limited direct IP.
func (s *Server) ProbeGate(ctx context.Context) *gate.Profile {
	if s.gate == nil {
		return nil
	}
	s.prepareGate()
	return s.gate.Refresh(ctx)
}

// handleGateRefresh forces a probe now.
func (s *Server) handleGateRefresh(w http.ResponseWriter, r *http.Request) {
	if s.gate == nil {
		writeAPIError(w, http.StatusPreconditionFailed, "gate probing is disabled", "gate_disabled")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	model := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("model")), "opencode-zen/")
	if model == "" {
		model = s.gate.ProbeModel()
	}
	if len(model) > 256 || strings.ContainsAny(model, "\r\n\t/") {
		writeAPIError(w, http.StatusBadRequest, "invalid Zen probe model", "invalid_request_error")
		return
	}
	s.prepareGate()
	prof := s.gate.RefreshModel(ctx, model)
	writeJSON(w, http.StatusOK, s.buildGateStatus(prof))
}

// handleGateTest sends a real inference request with the current profile and
// reports whether the gate accepted it. This is the end-to-end proof that the
// adaptation works, not just that the profile looks right.
func (s *Server) handleGateTest(w http.ResponseWriter, r *http.Request) {
	if s.gate == nil {
		writeAPIError(w, http.StatusPreconditionFailed, "gate probing is disabled", "gate_disabled")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Build a deliberately bare body — no tools, no stream flag — then let the
	// adaptation layer fix it. This exercises the same ApplyToBody call the real
	// proxy path uses, so a pass here is genuine end-to-end proof.
	s.prepareGate()
	prof := s.gate.Current(ctx)
	profile := zen.ProfileFromGate(prof)
	if profile == nil {
		profile = zen.ProfileFromGate(gate.Fallback())
	}
	ctx = zen.WithGateSnapshot(ctx, profile)

	model := s.gate.ProbeModel()
	body := map[string]any{
		"model": model,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": "Reply with exactly: GATE_OK"}},
		}},
		// Reasoning models spend tokens before emitting text, so this must be
		// large enough that the reply is not truncated into `incomplete`.
		"max_output_tokens": 2048,
		"store":             false,
	}
	// No client tools at all: every required tool must be injected. This is the
	// same call the real proxy path makes.
	tools, _ := zen.PrepareTools(nil, profile)
	injectedCount := len(tools)
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	if profile.RequiresStream {
		body["stream"] = true
	}
	started := time.Now()
	// Use the same transport the proxy uses. Testing directly would report a
	// false failure whenever the direct egress IP is rate limited, even though
	// real requests succeed through mihomo.
	events, status, err, viaMihomo := s.streamZenForProbe(ctx, model, "gate-test", body)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             false,
			"model":          model,
			"status":         status,
			"error":          err.Error(),
			"elapsed_ms":     time.Since(started).Milliseconds(),
			"tools_injected": injectedCount,
			"required_tools": prof.RequiredTools,
			"profile_source": prof.Source,
			"stream_forced":  body["stream"],
			"transport":      viaMihomo,
		})
		return
	}

	text := ""
	for ev := range events {
		if ev.Type == "response.output_text.delta" {
			text += ev.Delta
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             strings.Contains(text, "GATE_OK"),
		"model":          model,
		"text":           text,
		"status":         status,
		"elapsed_ms":     time.Since(started).Milliseconds(),
		"tools_injected": injectedCount,
		"required_tools": prof.RequiredTools,
		"profile_source": prof.Source,
		"stream_forced":  body["stream"],
		"transport":      viaMihomo,
	})
}

// streamZenForProbe sends a probe request down the exact path a real client
// request takes, including node rotation and retry.
//
// Testing with a single bare attempt reports a false failure for any condition a
// retry would have absorbed — a dropped connection, a rate-limited exit, an
// overloaded backend. Those are the common case on the free tier, so the test
// would fail far more often than real traffic does. It reports whether mihomo
// carried the request so the dashboard can say which egress was used.
func (s *Server) streamZenForProbe(
	ctx context.Context,
	model, sessionSeed string,
	body map[string]any,
) (<-chan zen.Event, int, error, bool) {
	if !s.mihomoEnabled() {
		events, status, err := s.zen.Stream(ctx, model, sessionSeed, body)
		return events, status, err, false
	}
	if c := s.mihomoClient(); c != nil {
		if _, err := s.mihomoGroup(ctx, c); err == nil {
			events, status, err, relay := s.mihomoStreamZen(ctx, model, sessionSeed, body, requestRecord{})
			return events, status, err, strings.HasPrefix(relay, "mihomo:")
		}
	}
	// No usable proxy group: fall back to direct rather than failing the test.
	events, status, err := s.zen.Stream(ctx, model, sessionSeed, body)
	return events, status, err, false
}

// prepareGate points the prober at the same egress the proxy path uses.
//
// Without this the prober sends its requests directly, where the free tier's
// per-IP quota is normally exhausted, so every probe reports
// "baseline rejected with 429". That reads as the gate refusing a known-good
// request when in fact the request never reached the gate.
func (s *Server) prepareGate() {
	if s.gate == nil {
		return
	}
	if !s.mihomoEnabled() {
		s.gate.Configure(nil, nil)
		return
	}
	c := s.mihomoClient()
	if c == nil {
		s.gate.Configure(nil, nil)
		return
	}
	// Honour the operator's rotation switch here too.
	//
	// Without this check the prober moved nodes even when rotation was off,
	// which silently changed the exit the real requests used: measured as the
	// node switching from the working DIRECT to a rate-limited WARP during a
	// probe. A probe must observe the configured egress, not alter it.
	if !s.mihomoRotateOnLimit() {
		s.gate.Configure(c.ProxyClient(), nil)
		return
	}

	s.gate.Configure(c.ProxyClient(), func(ctx context.Context) bool {
		group, err := s.mihomoGroup(ctx, c)
		if err != nil {
			return false
		}
		_, _, err = s.rotateMihomoNode(ctx, c, group)
		return err == nil
	})
}
