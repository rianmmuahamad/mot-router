// Package gate probes the upstream free-tier gate and derives the request shape
// it currently demands.
//
// Why probing instead of reading an error: every gate failure returns the exact
// same response — HTTP 403 with `FreeTierError` — whether the User-Agent,
// session id, streaming flag, or tool roster is at fault. There is no
// self-describing endpoint. So the only reliable way to learn what the gate
// wants is to send a request that is known-good and see whether it is accepted.
//
// Probes use bounded streaming requests. A successful verdict requires actual
// output and confirmed termination, rather than an HTTP status alone.
package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rian/mot-router/internal/streamwire"
)

// Condition is one requirement the gate enforces.
type Condition struct {
	Name         string `json:"name"`
	OK           bool   `json:"ok"`
	Detail       string `json:"detail"`
	Inconclusive bool   `json:"inconclusive,omitempty"`
	// Status and Body are the raw upstream verdict, kept for the dashboard.
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
}

// Profile is the complete request shape the gate currently accepts.
type Profile struct {
	ProbeModel       string            `json:"probe_model,omitempty"`
	ToolDescriptions map[string]string `json:"tool_descriptions,omitempty"`
	BaseURL          string            `json:"base_url,omitempty"`
	ClientFormat     string            `json:"client_format,omitempty"`
	VerifiedVersion  string            `json:"verified_version,omitempty"`
	Identity         IdentityPolicy    `json:"identity"`
	RepairSource     string            `json:"repair_source,omitempty"`
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
	DefaultUserAgent    = "opencode/1.19.0"
	DefaultMinVersion   = "1.19.0"
	DefaultSessionHex   = 12
	DefaultSessionAlnum = 14
)

// Fallback returns the last known-good profile.
func Fallback() *Profile {
	return &Profile{
		ClientFormat:        ClientV1,
		Identity:            DefaultIdentity(),
		UserAgent:           DefaultUserAgent,
		MinVersion:          DefaultMinVersion,
		SessionPrefix:       "ses_",
		SessionHex:          DefaultSessionHex,
		SessionAlnum:        DefaultSessionAlnum,
		RequiresStream:      true,
		RequiredTools:       []string{"read"},
		AcceptsToolSuperset: true,
		CheckedAt:           time.Now(),
		Verdict:             "fallback",
		Source:              "fallback",
	}
}

// Prober runs the checks.
type Prober struct {
	Resolve          func(context.Context) (*OfficialHints, error)
	ReferenceVersion string
	seed             *Profile
	policy           IdentityPolicy
	format           string
	BaseURL          string
	endpoint         string
	Model            string
	ToolPool         []string
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
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = DefaultProbeModel
	}
	return &Prober{
		BaseURL:  strings.TrimSuffix(baseURL, "/"),
		Model:    model,
		ToolPool: nativeToolNames(ClientV1),
		ExtraUA:  "ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14",
	}
}

func (p *Prober) forModel(model string) *Prober {
	p.mu.RLock()
	defer p.mu.RUnlock()
	copy := NewProber(p.BaseURL, model)
	copy.ToolPool = append([]string(nil), p.ToolPool...)
	copy.ExtraUA, copy.Resolve = p.ExtraUA, p.Resolve
	copy.ReferenceVersion = p.ReferenceVersion
	copy.HTTP, copy.Rotate = p.HTTP, p.Rotate
	return copy
}

// probeBody builds the minimal request used for gating checks. It is tiny so a
// rejected probe costs nothing and an accepted one costs almost nothing.
func (p *Prober) probeBody(tools []string, stream bool) map[string]any {
	p.mu.RLock()
	format := p.format
	p.mu.RUnlock()
	ts := make([]map[string]any, 0, len(tools))
	for _, n := range tools {
		ts = append(ts, ProbeTool(n, format))
	}
	b := map[string]any{
		"model": p.Model,
		"input": []map[string]any{{
			"role":    "developer",
			"content": []map[string]any{{"type": "input_text", "text": "This is a connectivity check. Reply OK without calling tools."}},
		}, {
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
	if UsesChatCompletions(p.Model) {
		chat := map[string]any{
			"model": p.Model, "stream": stream,
			"messages": []map[string]any{
				{"role": "system", "content": "This is a connectivity check. Reply OK without calling tools."},
				{"role": "user", "content": "hi"},
			},
			"max_tokens": 16,
		}
		if format == ClientV2 {
			delete(chat, "max_tokens")
			chat["max_completion_tokens"] = 16
		}
		if len(ts) > 0 {
			chatTools := make([]map[string]any, 0, len(ts))
			for _, tool := range ts {
				chatTools = append(chatTools, map[string]any{"type": "function", "function": map[string]any{
					"name": tool["name"], "description": tool["description"], "parameters": tool["parameters"],
				}})
			}
			chat["tools"] = chatTools
			chat["tool_choice"] = "auto"
		}
		return chat
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

	// Only a failed connection may try another node. Quota/access denials
	// are returned as received and never trigger an IP switch.
	ipTied := v.err != nil && v.status == 0
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
	p.mu.RLock()
	endpoint := p.endpoint
	p.mu.RUnlock()
	if endpoint == "" {
		endpoint = p.BaseURL
	}
	path := "/responses"
	if UsesChatCompletions(p.Model) {
		path = "/chat/completions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint+path, strings.NewReader(string(payload)))
	if err != nil {
		return verdict{err: err}
	}
	p.mu.RLock()
	policy := p.policy
	format := p.format
	p.mu.RUnlock()
	for k, v := range ClientHeaders(ua, session, RequestIDWithPolicy(session, policy), format) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer public")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return verdict{err: err}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if len(body) > 1<<20 {
		return verdict{status: resp.StatusCode, err: fmt.Errorf("probe response exceeds 1 MiB")}
	}
	if readErr == nil && resp.StatusCode == http.StatusOK && !streamwire.ValidCompletion(body) {
		readErr = fmt.Errorf("probe returned HTTP 200 without completed output")
	}
	return verdict{status: resp.StatusCode, body: strings.TrimSpace(string(body)), err: readErr}

}

// accepted reports whether a verdict proves the request satisfied the gate.
//
// Only HTTP 200 counts. A 429 must NOT be treated as acceptance: the upstream
// evaluates its quota before the gate conditions, so a rate-limited IP returns
// 429 for every request — valid or not. Treating that as a pass makes the probe
// report whatever it tested first as working, which is how a stale version floor
// gets recorded. Use inconclusive for that case.
func accepted(v verdict) bool {
	return v.err == nil && v.status == http.StatusOK && streamwire.ValidCompletion([]byte(v.body))
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
	return v.status == http.StatusTooManyRequests || v.status == http.StatusRequestTimeout || v.status >= 500
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

	p.mu.RLock()
	seed := p.seed
	p.mu.RUnlock()
	if seed != nil {
		*prof = *seed
		prof.Conditions = nil
		prof.Source = "probe"
		prof.CheckedAt = started
		prof.RepairSource = ""
	}
	if prof.BaseURL == "" && IsOfficialEndpoint(p.BaseURL) {
		prof.BaseURL = p.BaseURL
	}
	if prof.ClientFormat == "" {
		prof.ClientFormat = ClientV1
	}
	prof.ProbeModel = p.Model
	// A successful V1 baseline does not establish compatibility with the
	// installed V2 client. Prefer the recognized current official contract
	// before checking inference, including when a legacy profile is cached.
	resolver := p.Resolve
	var resolvedHints []*OfficialHints
	if resolver == nil && IsOfficialEndpoint(p.BaseURL) {
		resolver = func(ctx context.Context) (*OfficialHints, error) { return officialHints(ctx, p.ReferenceVersion) }
	}
	if resolver != nil {
		hint, err := resolver(ctx)
		resolvedHints = append(resolvedHints, hint)
		if err != nil {
			prof.Conditions = append(prof.Conditions, Condition{Name: "official_metadata", Inconclusive: true, Detail: err.Error()})
		} else if hint != nil && hint.ClientFormat == ClientV2 && hint.Identity.Valid() && versionPattern.MatchString(hint.Version) && versionFromUA(hint.UserAgent) == hint.Version {
			prof.ClientFormat, prof.Identity, prof.UserAgent = hint.ClientFormat, hint.Identity, hint.UserAgent
			if IsOfficialEndpoint(p.BaseURL) {
				prof.BaseURL = DefaultBaseURL
			}
			prof.RepairSource = "official @opencode/cli@" + hint.Version
		}
	}
	p.setProbeProfile(prof)
	goodSession := SessionIDWithPolicy("gate-probe", prof.Identity)
	baselineUA := prof.UserAgent
	if prof.ClientFormat != ClientV2 && !strings.Contains(baselineUA, " ") {
		baselineUA += " " + p.ExtraUA
	}

	// A historical request shape is only a candidate baseline. It becomes
	// verified when upstream actually accepts it; a tool count proves nothing.
	base := p.send(ctx, baselineUA, goodSession, p.probeTools(), true)
	if !accepted(base) && repairable(base.status, base.body) && prof.ClientFormat != ClientV2 {
		if repaired, session, v := p.recoverBaseline(ctx, prof, resolvedHints...); repaired != nil {
			prof = repaired
			goodSession = session
			baselineUA = prof.UserAgent
			base = v
		}
	}
	prof.Conditions = append(prof.Conditions, Condition{
		Name: "baseline", OK: accepted(base), Status: base.status, Body: base.body,
		Detail:       fmt.Sprintf("request uji %s dengan %d tools (%s)", p.Model, len(p.probeTools()), prof.ClientFormat),
		Inconclusive: inconclusive(base),
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

	// Keep the complete accepted contract. A reduced roster measured on one
	// request/model must not become a universal minimum for other models.
	required := p.probeTools()
	prof.RequiredTools = append([]string(nil), required...)
	prof.AcceptsToolSuperset = false // extra caller tools are preserved, not presumed verified
	prof.MinVersion = ""
	prof.Conditions = append(prof.Conditions,
		Condition{Name: "required_tools", OK: true, Detail: "roster lengkap; tanpa reduksi: " + strings.Join(required, ", ")},
		Condition{Name: "client_version", OK: true, Detail: versionFromUA(baselineUA) + "; versi minimum tidak diuji"},
	)

	// Keep the accepted client version; never search for older versions.
	prof.UserAgent = baselineUA
	prof.VerifiedVersion = versionFromUA(baselineUA)
	prof.SessionPrefix = "ses_"
	prof.SessionHex = prof.Identity.Normalized().TimestampBytes * 2
	prof.SessionAlnum = prof.Identity.Normalized().SuffixLength
	prof.RequiresStream = true
	prof.ToolDescriptions = make(map[string]string)
	// The full baseline verified these descriptions together. Keep all of
	// them: extra known tools also need their verified description when a
	// client declares them, even though only a subset is mandatory.
	for _, name := range p.probeTools() {
		tool := ProbeTool(name, prof.ClientFormat)
		prof.ToolDescriptions[name], _ = tool["description"].(string)
	}
	verified := p.send(ctx, baselineUA, goodSession, required, true)
	prof.Conditions = append(prof.Conditions, Condition{Name: "profile_verified", OK: accepted(verified), Status: verified.status, Body: verified.body, Inconclusive: inconclusive(verified), Detail: "profil akhir diuji sebelum dipakai"})
	if !accepted(verified) {
		prof.Source = "fallback"
		prof.Verdict = "final profile verification failed"
		prof.LatencyMs = time.Since(started).Milliseconds()
		return prof
	}
	prof.Verdict = "ok"
	if prof.RepairSource == "" && seed != nil && !slices.Equal(seed.RequiredTools, required) {
		prof.RepairSource = "verified complete tool roster for " + p.Model
	}
	prof.LatencyMs = time.Since(started).Milliseconds()
	return prof
}

// discoverRequiredTools finds the smallest tool set the gate accepts.
//
// It starts from the known pool and removes names one at a time, keeping any
// removal that still passes. That yields a minimal set without assuming the
// current five names are still correct.
func (p *Prober) discoverRequiredTools(ctx context.Context, ua, session string) []string {
	candidates := p.probeTools()

	// Fast path: if the full pool is not accepted, the gate wants something we
	// do not have, or the IP is rate limited. Either way, report the known set so
	// the caller still has a usable default.
	if !accepted(p.send(ctx, ua, session, candidates, true)) {
		p.mu.RLock()
		v2 := p.format == ClientV2
		p.mu.RUnlock()
		if v2 {
			return []string{"shell", "edit", "glob", "grep", "read"}
		}
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
func (p *Prober) discoverMinVersion(ctx context.Context, session string, extra ...string) string {
	// Oldest first, so the first definitive acceptance is the floor.
	candidates := []string{
		"1.16.0", "1.17.0", "1.17.9", "1.18.0", "1.18.1", "1.18.31", "1.19.0",
	}
	for _, version := range extra {
		if versionPattern.MatchString(version) {
			candidates = append(candidates, version)
		}
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
	recovering       bool
	recoveryDone     chan struct{}
	done             chan struct{}
	recoveryAt       time.Time
	recoveryAttempts int
	repairs          int
	lastRepair       time.Time
	lastRepairSource string
	mu               sync.RWMutex
	current          *Profile
	lastGood         *Profile
	lastAttempt      time.Time
	prober           *Prober
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
	lastAttempt := c.lastAttempt
	c.mu.RUnlock()

	if cur != nil && cur.Source == "probe" && time.Since(cur.CheckedAt) < c.TTL {
		return cur
	}
	// A failed probe must not turn every test into another discovery run.
	// Explicit Refresh still retries immediately when requested by the user.
	if cur != nil && cur.Source == "fallback" && !lastAttempt.IsZero() && time.Since(lastAttempt) < time.Minute {
		return cur
	}
	return c.Refresh(ctx)
}

// Refresh runs a probe and stores the result. It never returns nil.
func (c *Cache) Refresh(ctx context.Context) *Profile {
	return c.RefreshModel(ctx, c.ProbeModel())
}

// RefreshModel verifies the model under repair, using an isolated prober.
func (c *Cache) RefreshModel(ctx context.Context, model string) *Profile {
	if model == "" || len(model) > 256 || strings.ContainsAny(model, "\r\n\t") {
		p := Fallback()
		p.Verdict = "invalid probe model"
		return p
	}
	c.mu.Lock()
	if c.inflight {
		cur := c.current
		c.mu.Unlock()
		return cur
	}
	c.inflight = true
	c.done = make(chan struct{})
	seed := c.current
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.inflight = false
		close(c.done)
		c.mu.Unlock()
	}()

	prober := c.prober.forModel(model)
	prober.setSeed(seed)
	prof := prober.Probe(ctx)
	if prof == nil {
		prof = Fallback()
	}

	c.mu.Lock()
	c.lastAttempt = time.Now()
	if prof.Source == "probe" && prof.Verdict == "ok" {
		c.lastGood = prof
		if prof.RepairSource != "" {
			c.repairs++
			c.lastRepair = time.Now()
			c.lastRepairSource = prof.RepairSource
		}
	} else if c.lastGood != nil {
		// Keep all verified adaptation settings across successive failures,
		// while reporting the latest failed attempt's actual diagnostics.
		working := *c.lastGood
		working.Source = prof.Source
		working.Verdict = prof.Verdict
		working.CheckedAt = prof.CheckedAt
		working.LatencyMs = prof.LatencyMs
		working.Conditions = prof.Conditions
		working.ProbeModel = prof.ProbeModel
		prof = &working
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
