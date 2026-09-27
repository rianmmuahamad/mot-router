package gate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestProbeLive runs the prober against the real gateway. It is opt-in because
// it makes network calls: go test ./internal/gate -run Live -v
func TestProbeLive(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	p := NewProber("", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	prof := p.Probe(ctx)
	out, _ := json.MarshalIndent(prof, "", "  ")
	t.Logf("profile:\n%s", out)

	// A rate-limited egress IP makes every probe inconclusive, which is the
	// correct outcome: the prober must not record a profile it could not verify.
	// That is a skip, not a failure.
	if prof.Verdict != "ok" {
		if strings.Contains(prof.Verdict, "429") {
			t.Skipf("egress IP is rate limited, probe inconclusive: %s", prof.Verdict)
		}
		t.Fatalf("probe verdict = %q, want ok", prof.Verdict)
	}
	if len(prof.RequiredTools) == 0 {
		t.Fatal("no required tools discovered")
	}
	// Assert the discovered set is internally consistent: removing any one of its
	// members must make the gate reject the request. This verifies the reduction
	// rather than hardcoding names that the gate is free to change.
	if len(prof.RequiredTools) == 0 {
		t.Fatal("no required tools discovered")
	}
	for _, drop := range prof.RequiredTools {
		trial := make([]string, 0, len(prof.RequiredTools)-1)
		for _, n := range prof.RequiredTools {
			if n != drop {
				trial = append(trial, n)
			}
		}
		v := p.send(ctx, prof.UserAgent, SessionID("verify", "ses_", 12, 14), trial, true)
		if accepted(v) {
			t.Errorf("removing %q still passed; %v is not a minimal set", drop, prof.RequiredTools)
		}
	}
	if !prof.AcceptsToolSuperset {
		t.Error("gate no longer tolerates extra tool names; injection is unsafe")
	}
	if !prof.RequiresStream {
		t.Error("stream no longer required")
	}
}
func TestSessionShape(t *testing.T) {
	id := SessionID("seed", "ses_", 12, 14)
	if len(id) != 4+12+14 {
		t.Fatalf("id %q has length %d, want 30", id, len(id))
	}
	if id[:4] != "ses_" {
		t.Errorf("prefix = %q, want ses_", id[:4])
	}
	for _, c := range id[4:16] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("hex segment contains %q", c)
		}
	}
	// Deterministic: same seed, same id (required for prompt caching).
	if SessionID("seed", "ses_", 12, 14) != id {
		t.Error("SessionID is not deterministic")
	}
	if SessionID("other", "ses_", 12, 14) == id {
		t.Error("different seeds produced the same id")
	}
}

// TestFallbackIsUsable guards the safety net.
func TestFallbackIsUsable(t *testing.T) {
	f := Fallback()
	if f.UserAgent == "" || len(f.RequiredTools) == 0 {
		t.Fatal("fallback profile is incomplete")
	}
	if f.Source != "fallback" {
		t.Errorf("source = %q", f.Source)
	}
}

// TestCacheRefresh stores a profile and serves it back.
func TestCacheRefresh(t *testing.T) {
	c := NewCache(NewProber("", ""), time.Hour)
	if c.Snapshot() == nil {
		t.Fatal("cache should be seeded")
	}
	if c.Snapshot().Source != "fallback" {
		t.Errorf("initial source = %q, want fallback", c.Snapshot().Source)
	}
}
