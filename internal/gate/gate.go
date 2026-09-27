// Package gate probes the upstream free-tier gate and derives the request shape
// it currently demands.
//
// Why probing instead of reading an error: every gate failure returns the exact
// same response — HTTP 403 with `FreeTierError` — whether the User-Agent,
// session id, streaming flag, or tool roster is at fault. There is no
// self-describing endpoint. So the only reliable way to learn what the gate
// wants is to send a request that is known-good and see whether it is accepted.
//
// The probe is deliberately cheap: a tiny non-streaming call that the gate
// rejects before any generation happens, so it costs no output tokens.
package gate

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Condition is one requirement the gate enforces.
type Condition struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	// Status and Body are the raw upstream verdict, kept for the dashboard.
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
}

// Profile is the complete request shape the gate currently accepts.
type Profile struct {
	// UserAgent must keep `opencode/<version>` as its leading token.
	UserAgent string `json:"user_agent"`
	// MinVersion is the lowest `opencode/<x.y.z>` accepted.
	MinVersion string `json:"min_version"`
	// SessionPrefix/Hex/Alnum describe the `ses_` id shape.
	SessionPrefix string `json:"session_prefix"`
	SessionHex    int    `json:"session_hex"`
	SessionAlnum  int    `json:"session_alnum"`
	// RequiresStream means the body must carry "stream": true.
	RequiresStream bool `json:"requires_stream"`
	// RequiredTools are tool names the body must contain.
	RequiredTools []string `json:"required_tools"`
	// AcceptsToolSuperset records that extra tool names are tolerated, which is
	// what lets the router inject missing names safely.
	AcceptsToolSuperset bool `json:"accepts_tool_superset"`

	// Probe metadata.
	CheckedAt  time.Time   `json:"checked_at"`
	LatencyMs  int64       `json:"latency_ms"`
	Conditions []Condition `json:"conditions"`
	// Verdict is "ok" when a known-good request was accepted.
	Verdict string `json:"verdict"`
	// Source is "probe" or "fallback".
	Source string `json:"source"`
}

// KnownToolNames is a broad pool of names real agent clients use. The gate
// accepts a superset (verified up to 60 names), so sending the whole pool is
// safe and means the router keeps working when a client renames or drops a tool.
var KnownToolNames = []string{
	"bash", "edit", "glob", "grep", "read", "write",
	"skill", "task", "todowrite", "webfetch", "websearch",
	"list", "multiedit", "patch", "apply_patch", "notebookedit",
	"ls", "view", "search", "todo", "codesearch",
}

// Defaults captured from the official CLI. Used when a probe cannot run.
const (
	DefaultUserAgent    = "opencode/1.18.31"
	DefaultMinVersion   = "1.18.0"
	DefaultSessionHex   = 12
	DefaultSessionAlnum = 14
)

// Fallback returns the last known-good profile.
func Fallback() *Profile {
	return &Profile{
		UserAgent:           DefaultUserAgent,
		MinVersion:          DefaultMinVersion,
		SessionPrefix:       "ses_",
		SessionHex:          DefaultSessionHex,
		SessionAlnum:        DefaultSessionAlnum,
		RequiresStream:      true,
		RequiredTools:       []string{"bash", "edit", "glob", "grep", "read"},
		AcceptsToolSuperset: true,
		CheckedAt:           time.Now(),
		Verdict:             "fallback",
		Source:              "fallback",
	}
}

// SessionID mints an id in the shape the gate checks. Deterministic per seed so
// prompt caching and upstream routing stay pinned to one conversation.
func SessionID(seed string, prefix string, hexLen, alnumLen int) string {
	if prefix == "" {
		prefix = "ses_"
	}
	if hexLen <= 0 {
		hexLen = DefaultSessionHex
	}
	if alnumLen <= 0 {
		alnumLen = DefaultSessionAlnum
	}
	sum := md5.Sum([]byte(seed))
	h := hex.EncodeToString(sum[:])
	for len(h) < hexLen {
		h += h
	}

	sum2 := md5.Sum([]byte("alnum:" + seed))
	a := hex.EncodeToString(sum2[:])
	for len(a) < alnumLen {
		a += a
	}
	return prefix + h[:hexLen] + a[:alnumLen]
}

// Prober runs the checks.
type Prober struct {
	BaseURL  string
	Model    string
	ToolPool []string
	// ExtraUA is appended after the version token, mimicking the real CLI.
	ExtraUA string

	// mu guards HTTP and Rotate, which are swapped at runtime by the server as
	// the proxy settings change while a background probe may be running.
	mu     sync.RWMutex
	HTTP   *http.Client
	Rotate func(ctx context.Context) bool
}

// Configure swaps the transport and the rotation hook.
//
// A nil client restores the process default. The server calls this so probes
// leave through the same egress as real requests: probing directly reports a
// false "baseline rejected with 429" whenever the direct IP is rate limited,
// which is the normal state on the free tier.
func (p *Prober) Configure(h *http.Client, rotate func(ctx context.Context) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.HTTP = h
	p.Rotate = rotate
}

func (p *Prober) httpClient() *http.Client {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

func (p *Prober) rotate(ctx context.Context) bool {
	p.mu.RLock()
	fn := p.Rotate
	p.mu.RUnlock()
	if fn == nil {
		return false
	}
	return fn(ctx)
}

// NewProber builds a prober with production defaults.
func NewProber(baseURL, model string) *Prober {
	if baseURL == "" {
		baseURL = "https://opencode.ai/zen/v1"
	}
	if model == "" {
		model = "muse-spark-1.3-contributor-free"
	}
	return &Prober{
		BaseURL:  strings.TrimSuffix(baseURL, "/"),
		Model:    model,
		ToolPool: KnownToolNames,
		ExtraUA:  "ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14",
	}
}

// probeBody builds the minimal request used for gating checks. It is tiny so a
// rejected probe costs nothing and an accepted one costs almost nothing.
func (p *Prober) probeBody(tools []string, stream bool) map[string]any {
	ts := make([]map[string]any, 0, len(tools))
	for _, n := range tools {
		ts = append(ts, map[string]any{
			"type": "function", "name": n, "description": "probe",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	b := map[string]any{
		"model": p.Model,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": "hi"}},
		}},
		"max_output_tokens": 16,
		"store":             false,
		"stream":            stream,
	}
	if len(ts) > 0 {
		b["tools"] = ts
		b["tool_choice"] = "auto"
	}
	return b
}

type verdict struct {
	status int
	body   string
	err    error
}

// send performs one probe request, rotating the exit node once if the failure
// is one a different node would fix.
//
// Retrying matters here because a probe is a single request: without it, one
// flaky exit node makes the whole profile look rejected and the dashboard
// reports a failure that says nothing about the gate itself.
func (p *Prober) send(ctx context.Context, ua, session string, tools []string, stream bool) verdict {
	v := p.sendOnce(ctx, ua, session, tools, stream)

	// A rate limit (429) or a dropped connection (no HTTP response) is tied to
	// the exit IP, not to the request. Try one other node before believing it.
	ipTied := v.status == http.StatusTooManyRequests ||
		(v.err != nil && v.status == 0)
	if !ipTied {
		return v
	}
	if !p.rotate(ctx) {
		return v
	}
	return p.sendOnce(ctx, ua, session, tools, stream)
}

// sendOnce performs exactly one probe request with no retry.
func (p *Prober) sendOnce(ctx context.Context, ua, session string, tools []string, stream bool) verdict {
	payload, err := json.Marshal(p.probeBody(tools, stream))
	if err != nil {
		return verdict{err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.BaseURL+"/responses", strings.NewReader(string(payload)))
	if err != nil {
		return verdict{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("x-opencode-session", session)
	req.Header.Set("Authorization", "Bearer public")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return verdict{err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// Drain any streamed content so the connection can be reused.
	if resp.StatusCode == http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	return verdict{status: resp.StatusCode, body: strings.TrimSpace(string(body))}
}

// accepted reports whether a verdict proves the request satisfied the gate.
//
// Only HTTP 200 counts. A 429 must NOT be treated as acceptance: the upstream
// evaluates its quota before the gate conditions, so a rate-limited IP returns
// 429 for every request — valid or not. Treating that as a pass makes the probe
// report whatever it tested first as working, which is how a stale version floor
// gets recorded. Use inconclusive for that case.
func accepted(v verdict) bool {
	return v.err == nil && v.status == http.StatusOK
}

// inconclusive reports whether the upstream refused to answer on grounds that
// say nothing about the request's validity.
//
// A 429 means the IP's quota is exhausted. Whether the gate conditions were
// checked before that is an implementation detail, so the result must not be
// read as evidence either way.
func inconclusive(v verdict) bool {
	// A transport failure means the request never got a verdict. Measured on a
	// VPS with a flaky route: treating these as rejections inflated the required
	// tool list from 5 names to 8, because a dropped connection during the
	// greedy removal looked like "the gate refuses without this tool".
	if v.err != nil {
		return true
	}
	return v.status == http.StatusTooManyRequests
}

// Probe runs the full check sequence and returns the discovered profile.
//
// Each condition is verified independently against a known-good baseline, so a
// single failure does not mask the others.
func (p *Prober) Probe(ctx context.Context) *Profile {
	started := time.Now()
	prof := Fallback()
	prof.Source = "probe"
	prof.CheckedAt = started

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	goodSession := SessionID("gate-probe", "ses_", DefaultSessionHex, DefaultSessionAlnum)
	baselineUA := DefaultUserAgent + " " + p.ExtraUA

	// 1. Baseline: known-good shape with the full tool pool. If this fails, the
	//    gate changed in a way this prober cannot describe, so fall back.
	base := p.send(ctx, baselineUA, goodSession, p.ToolPool, true)
	prof.Conditions = append(prof.Conditions, Condition{
		Name: "baseline", OK: accepted(base), Status: base.status, Body: base.body,
		Detail: fmt.Sprintf("known-good request with %d tools", len(p.ToolPool)),
	})
	if !accepted(base) {
		if base.err != nil {
			prof.Verdict = "error: " + base.err.Error()
		} else {
			prof.Verdict = fmt.Sprintf("baseline rejected with %d", base.status)
		}
		prof.Source = "fallback"
		prof.LatencyMs = time.Since(started).Milliseconds()
		return prof
	}

	// 2. User-Agent: the leading token must be `opencode/<version>`.
	uaOK := p.send(ctx, "mot-router/1.0", goodSession, p.ToolPool, true)
	prof.Conditions = append(prof.Conditions, Condition{
		Name: "user_agent_prefix", OK: !accepted(uaOK), Status: uaOK.status, Body: uaOK.body,
		Detail: "a non-opencode User-Agent must be refused",
	})

	// 3. Session shape: confirm a malformed id is refused, so we know the check
	//    is still live and our own minted ids are the reason we pass.
	sessBad := p.send(ctx, baselineUA, "sess_not-the-right-shape", p.ToolPool, true)
	prof.Conditions = append(prof.Conditions, Condition{
		Name: "session_shape", OK: !accepted(sessBad), Status: sessBad.status, Body: sessBad.body,
		Detail: "a malformed session id must be refused",
	})

	// 4. Streaming: confirm it is still mandatory.
	noStream := p.send(ctx, baselineUA, goodSession, p.ToolPool, false)
	prof.Conditions = append(prof.Conditions, Condition{
		Name: "stream_required", OK: !accepted(noStream), Status: noStream.status, Body: noStream.body,
		Detail: "a non-streaming body must be refused",
	})

	// 5. Tools: find the smallest set the gate accepts, then confirm extra names
	//    are tolerated (that is what makes injection safe).
	required := p.discoverRequiredTools(ctx, baselineUA, goodSession)
	prof.RequiredTools = required
	prof.Conditions = append(prof.Conditions, Condition{
		Name: "required_tools", OK: len(required) > 0,
		Detail: strings.Join(required, ", "),
	})

	supersetOK := p.send(ctx, baselineUA, goodSession, append(append([]string{}, required...), "mot_router_probe_tool"), true)
	prof.AcceptsToolSuperset = accepted(supersetOK)
	prof.Conditions = append(prof.Conditions, Condition{
		Name: "tool_superset", OK: prof.AcceptsToolSuperset, Status: supersetOK.status,
		Detail: "extra tool names beyond the required set must be tolerated",
	})

	// 6. Version floor: binary-search the lowest accepted minor version.
	prof.MinVersion = p.discoverMinVersion(ctx, goodSession)
	prof.Conditions = append(prof.Conditions, Condition{
		Name: "min_version", OK: prof.MinVersion != "", Detail: prof.MinVersion,
	})

	prof.UserAgent = "opencode/" + prof.MinVersion + " " + p.ExtraUA
	prof.SessionPrefix = "ses_"
	prof.SessionHex = DefaultSessionHex
	prof.SessionAlnum = DefaultSessionAlnum
	prof.RequiresStream = true
	prof.Verdict = "ok"
	prof.LatencyMs = time.Since(started).Milliseconds()
	return prof
}

// discoverRequiredTools finds the smallest tool set the gate accepts.
//
// It starts from the known pool and removes names one at a time, keeping any
// removal that still passes. That yields a minimal set without assuming the
// current five names are still correct.
func (p *Prober) discoverRequiredTools(ctx context.Context, ua, session string) []string {
	candidates := append([]string{}, p.ToolPool...)

	// Fast path: if the full pool is not accepted, the gate wants something we
	// do not have, or the IP is rate limited. Either way, report the known set so
	// the caller still has a usable default.
	if !accepted(p.send(ctx, ua, session, candidates, true)) {
		return []string{"bash", "edit", "glob", "grep", "read"}
	}

	// Greedy removal. A removal is kept only when the smaller set is accepted
	// outright.
	//
	// An inconclusive verdict must not be read as "this tool is required": it
	// says nothing about the tool. Those are retried a bounded number of times,
	// and only a definite rejection keeps the name. Measured before this: a
	// dropped connection mid-scan inflated the required list from 5 to 8 names.
	for i := 0; i < len(candidates); {
		trial := append(append([]string{}, candidates[:i]...), candidates[i+1:]...)
		if len(trial) == 0 {
			i++
			continue
		}

		drop := false
		for attempt := 0; attempt < 3; attempt++ {
			v := p.send(ctx, ua, session, trial, true)
			if accepted(v) {
				drop = true
				break
			}
			if !inconclusive(v) {
				// A definite rejection: the tool really is required.
				break
			}
			// Inconclusive — retry rather than assume.
		}
		if drop {
			candidates = trial
			continue
		}
		i++
	}

	// Verify the empty set is genuinely rejected, so we know the reduction was
	// meaningful rather than the gate having stopped checking tools. Only a
	// definite rejection proves that; an inconclusive result is retried.
	emptyAccepted := false
	emptyRejected := false
	for attempt := 0; attempt < 3; attempt++ {
		v := p.send(ctx, ua, session, nil, true)
		if accepted(v) {
			emptyAccepted = true
			break
		}
		if !inconclusive(v) {
			emptyRejected = true
			break
		}
	}
	if emptyAccepted {
		return []string{}
	}
	if !emptyRejected {
		// Every attempt was inconclusive, so the reduction is unproven. Report
		// the known-good set rather than a minimal list that was never verified.
		return []string{"bash", "edit", "glob", "grep", "read"}
	}

	sort.Strings(candidates)
	return candidates
}

// discoverMinVersion finds the lowest `opencode/<x.y.z>` the gate accepts.
// A rate-limited IP answers every version with 429, which proves nothing. Those
// results are skipped rather than read as acceptance; if the whole scan is
// inconclusive the caller keeps the known-good default instead of recording a
// version that was never actually validated.
func (p *Prober) discoverMinVersion(ctx context.Context, session string) string {
	// Oldest first, so the first definitive acceptance is the floor.
	candidates := []string{
		"1.16.0", "1.17.0", "1.17.9", "1.18.0", "1.18.1", "1.18.31",
	}

	sawRejection := false
	for _, v := range candidates {
		verdict := p.send(ctx, "opencode/"+v+" "+p.ExtraUA, session, p.ToolPool, true)

		if accepted(verdict) {
			return v
		}
		if inconclusive(verdict) {
			// Rate limited: this version tells us nothing, try the next one.
			continue
		}
		// A definite rejection (403, 426, …) proves this version is too old.
		sawRejection = true
	}

	// Nothing was accepted. If at least one version was rejected outright, the
	// floor is above the candidates; fall back to the known-good default. If the
	// entire scan was rate limited, the default is also the safest answer.
	_ = sawRejection
	return DefaultMinVersion
}

// Cache keeps the last good profile and refreshes it in the background.
type Cache struct {
	mu      sync.RWMutex
	current *Profile
	prober  *Prober
	// TTL is how long a profile is trusted before a refresh.
	TTL time.Duration
	// OnUpdate is called after each successful refresh, for dashboard wiring.
	OnUpdate func(*Profile)
	// inflight guards against overlapping refreshes.
	inflight bool
}

// NewCache builds a cache seeded with the fallback profile.
func NewCache(p *Prober, ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	return &Cache{current: Fallback(), prober: p, TTL: ttl}
}

// Current returns the profile in force, refreshing it if stale.
func (c *Cache) Current(ctx context.Context) *Profile {
	c.mu.RLock()
	cur := c.current
	c.mu.RUnlock()

	if cur != nil && cur.Source == "probe" && time.Since(cur.CheckedAt) < c.TTL {
		return cur
	}
	return c.Refresh(ctx)
}

// Refresh runs a probe and stores the result. It never returns nil.
func (c *Cache) Refresh(ctx context.Context) *Profile {
	c.mu.Lock()
	if c.inflight {
		cur := c.current
		c.mu.Unlock()
		return cur
	}
	c.inflight = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.inflight = false
		c.mu.Unlock()
	}()

	prof := c.prober.Probe(ctx)
	if prof == nil {
		prof = Fallback()
	}

	c.mu.Lock()
	// Keep the previous good profile's timestamps if this probe failed, so the
	// dashboard can show that the last check was unhealthy without losing the
	// working configuration.
	if prof.Source == "fallback" && c.current != nil && c.current.Source == "probe" {
		prof.RequiredTools = c.current.RequiredTools
		prof.UserAgent = c.current.UserAgent
		prof.MinVersion = c.current.MinVersion
	}
	c.current = prof
	c.mu.Unlock()

	if c.OnUpdate != nil {
		c.OnUpdate(prof)
	}
	return prof
}

// Configure re-points the prober's transport and rotation hook.
//
// The server calls this before each probe so the probe leaves through the same
// egress as real traffic. Without it a probe hits the direct IP, which on the
// free tier is normally rate limited, and the resulting 429 is misreported as
// the gate rejecting a known-good request.
func (c *Cache) Configure(h *http.Client, rotate func(ctx context.Context) bool) {
	if c.prober != nil {
		c.prober.Configure(h, rotate)
	}
}

// Snapshot returns the current profile without triggering a refresh.
func (c *Cache) Snapshot() *Profile {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}
