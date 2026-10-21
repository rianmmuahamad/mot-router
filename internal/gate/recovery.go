package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type OfficialHints struct {
	Version      string
	Identity     IdentityPolicy
	ClientFormat string
	UserAgent    string
}

var versionPattern = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,5}$`)

func repairable(status int, body string) bool {
	if status != 403 && status != 426 {
		return false
	}
	var data struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(body), &data) != nil {
		return false
	}
	t := data.Error.Type
	if t == "" {
		t = data.Type
	}
	return t == "FreeTierError" || t == "UpgradeRequired" || t == "UpgradeRequiredError"
}
func IsIdentityRejection(status int, detail string) bool { return repairable(status, detail) }

func (p *Prober) setIdentity(policy IdentityPolicy) {
	p.mu.Lock()
	p.policy = policy.Normalized()
	p.mu.Unlock()
}
func (p *Prober) setProbeProfile(profile *Profile) {
	p.mu.Lock()
	p.policy = profile.Identity.Normalized()
	p.format = profile.ClientFormat
	p.endpoint = ""
	if IsOfficialEndpoint(p.BaseURL) && IsOfficialEndpoint(profile.BaseURL) {
		p.endpoint = profile.BaseURL
	}
	p.mu.Unlock()
}
func (p *Prober) setSeed(profile *Profile) { p.mu.Lock(); p.seed = profile; p.mu.Unlock() }

func (p *Prober) recoverBaseline(ctx context.Context, current *Profile, resolvedHints ...*OfficialHints) (*Profile, string, verdict) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	candidates := []Profile{*current, *current}
	candidates[0].RepairSource = "fresh session timestamp"
	candidates[1].Identity = DefaultIdentity()
	candidates[1].ClientFormat = ClientV1
	candidates[1].UserAgent = DefaultUserAgent + " " + p.ExtraUA
	candidates[1].RepairSource = "built-in canonical identity"
	if IsOfficialEndpoint(p.BaseURL) {
		candidate := *current
		candidate.BaseURL = DefaultBaseURL
		if current.BaseURL == DefaultBaseURL {
			candidate.BaseURL = LegacyBaseURL
		}
		candidate.RepairSource = "verified official endpoint " + candidate.BaseURL
		candidates = append(candidates, candidate)
	}
	resolver := p.Resolve
	if resolver == nil && strings.HasPrefix(p.BaseURL, "https://opencode.ai/") {
		resolver = func(ctx context.Context) (*OfficialHints, error) { return officialHints(ctx, p.ReferenceVersion) }
	}
	// Read the current official client before trying historical endpoints.
	// An obsolete endpoint's 429/5xx must not prevent V2 from being considered.
	if resolver != nil {
		var hint *OfficialHints
		var err error
		if len(resolvedHints) > 0 {
			hint = resolvedHints[0]
		} else {
			hint, err = resolver(ctx)
		}
		if err != nil {
			current.Conditions = append(current.Conditions, Condition{Name: "official_metadata", Inconclusive: true, Detail: err.Error()})
		} else if hint != nil && versionPattern.MatchString(hint.Version) && hint.Identity.Valid() && validClientFormat(hint.ClientFormat) {
			candidate := *current
			candidate.Identity = hint.Identity
			candidate.ClientFormat = hint.ClientFormat
			candidate.UserAgent = "opencode/" + hint.Version + " " + p.ExtraUA
			candidate.RepairSource = "official opencode-ai@" + hint.Version
			if hint.ClientFormat == ClientV2 {
				candidate.UserAgent = hint.UserAgent
				candidate.RepairSource = "official @opencode/cli@" + hint.Version
			}
			if versionFromUA(candidate.UserAgent) == hint.Version {
				candidates = append([]Profile{candidate}, candidates...)
			}
		}
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		candidate.Identity = candidate.Identity.Normalized()
		key, _ := json.Marshal(struct {
			UA       string
			Policy   IdentityPolicy
			Format   string
			Endpoint string
		}{candidate.UserAgent, candidate.Identity, candidate.ClientFormat, candidate.BaseURL})
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		candidate.Identity.Epoch = time.Now().UnixMilli()
		p.setProbeProfile(&candidate)
		session := SessionIDWithPolicy("gate-probe", candidate.Identity)
		v := p.send(ctx, candidate.UserAgent, session, p.probeTools(), true)
		if accepted(v) {
			verified := p.send(ctx, candidate.UserAgent, session, p.probeTools(), true)
			if accepted(verified) {
				if candidate.ClientFormat == ClientV2 {
					candidate.Conditions = append([]Condition(nil), current.Conditions...)
					candidate.Conditions = append(candidate.Conditions, Condition{Name: "official_client", OK: true, Status: 200, Detail: candidate.RepairSource})
				}
				candidate.CheckedAt = time.Now()
				candidate.Source = "probe"
				candidate.Verdict = "ok"
				return &candidate, session, verified
			}
			if inconclusive(verified) {
				if candidate.ClientFormat == ClientV2 {
					current.Conditions = append(current.Conditions, Condition{Name: "official_client", Status: verified.status, Inconclusive: true, Detail: candidate.RepairSource})
				}
				break
			}
		} else if inconclusive(v) {
			if candidate.ClientFormat == ClientV2 {
				current.Conditions = append(current.Conditions, Condition{Name: "official_client", Status: v.status, Inconclusive: true, Detail: candidate.RepairSource})
			}
			break
		} else if candidate.ClientFormat == ClientV2 {
			current.Conditions = append(current.Conditions, Condition{Name: "official_client", Status: v.status, Detail: candidate.RepairSource})
		}
	}
	p.setProbeProfile(current)
	return nil, "", verdict{}
}

// Only parse a bounded, recognizable data layout from the official source.
// New executable code is never evaluated, installed, or run.
func ParseOfficialIdentity(source string) (IdentityPolicy, error) {
	if strings.Contains(source, "const length =") {
		return parseV2Identity(source)
	}
	find := func(pattern string) (string, error) {
		m := regexp.MustCompile(pattern).FindStringSubmatch(source)
		if len(m) < 2 {
			return "", fmt.Errorf("unrecognized official identifier layout")
		}
		return m[1], nil
	}
	length, e := find(`const\s+LENGTH\s*=\s*(\d+)`)
	if e != nil {
		return IdentityPolicy{}, e
	}
	bytes, e := find(`Buffer\.alloc\(\s*(\d+)\s*\)`)
	if e != nil {
		return IdentityPolicy{}, e
	}
	mult, e := find(`BigInt\(currentTimestamp\)\s*\*\s*BigInt\(\s*(0x[0-9a-fA-F]+|\d+)\s*\)`)
	if e != nil {
		return IdentityPolicy{}, e
	}
	if !strings.Contains(source, `direction === "descending" ? ~now : now`) {
		return IdentityPolicy{}, fmt.Errorf("unrecognized timestamp direction")
	}
	n, _ := strconv.Atoi(length)
	b, _ := strconv.Atoi(bytes)
	value, e := strconv.ParseUint(mult, 0, 64)
	if e != nil || value == 0 || value&(value-1) != 0 {
		return IdentityPolicy{}, fmt.Errorf("invalid identifier counter")
	}
	policy := DefaultIdentity()
	policy.TimestampBytes = b
	policy.CounterBits = bits.TrailingZeros64(value)
	policy.SuffixLength = n - b*2
	if !policy.Valid() {
		return IdentityPolicy{}, fmt.Errorf("unsupported official identifier layout")
	}
	return policy, nil
}

var hintCache struct {
	sync.Mutex
	value   *OfficialHints
	err     error
	attempt time.Time
	version string
}

func officialHints(ctx context.Context, versions ...string) (*OfficialHints, error) {
	version := ""
	if len(versions) > 0 {
		version = versions[0]
	}
	hintCache.Lock()
	defer hintCache.Unlock()
	ttl := 12 * time.Hour
	if hintCache.err != nil {
		ttl = 5 * time.Minute
	}
	if hintCache.version == version && !hintCache.attempt.IsZero() && time.Since(hintCache.attempt) < ttl {
		return hintCache.value, hintCache.err
	}
	hintCache.attempt = time.Now()
	hintCache.version = version
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	read := func(url string) ([]byte, error) {
		req, e := http.NewRequestWithContext(ctx, "GET", url, nil)
		if e != nil {
			return nil, e
		}
		client := &http.Client{CheckRedirect: func(r *http.Request, via []*http.Request) error { return fmt.Errorf("metadata redirect rejected") }}
		r, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return nil, fmt.Errorf("official metadata HTTP %d", r.StatusCode)
		}
		body, e := io.ReadAll(io.LimitReader(r.Body, maxOfficialDownload+1))
		if len(body) > maxOfficialDownload {
			return nil, fmt.Errorf("official metadata too large")
		}
		return body, e
	}
	hints, err := loadOfficialHints(ctx, read, version)
	hintCache.err = err
	if err != nil {
		hintCache.value = nil
		return nil, err
	}
	hintCache.value = hints
	return hintCache.value, nil
}
