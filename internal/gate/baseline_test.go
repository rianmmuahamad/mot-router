package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStableProbeNeverReducesRosterOrSearchesVersions(t *testing.T) {
	for _, finalStatus := range []int{200, 403, 429, 503} {
		t.Run(http.StatusText(finalStatus), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Tools []json.RawMessage `json:"tools"`
					Model string            `json:"model"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				n := calls.Add(1)
				if len(body.Tools) != 11 || body.Model != "glm-5.3-flash-free" {
					t.Errorf("unstable roster/model: tools=%d model=%s", len(body.Tools), body.Model)
				}
				if n > 1 {
					w.WriteHeader(finalStatus)
				}
				w.Write([]byte(testProbeCompletion))
			}))
			defer upstream.Close()
			p := NewProber(upstream.URL, "glm-5.3-flash-free")
			p.ToolPool = testLegacyRoster
			profile := p.Probe(t.Context())
			if calls.Load() != 2 {
				t.Fatalf("unexpected discovery requests: %d", calls.Load())
			}
			if finalStatus == 200 {
				if profile.Verdict != "ok" || len(profile.RequiredTools) != 11 || profile.MinVersion != "" {
					t.Fatalf("invalid verified profile: verdict=%s tools=%d floor=%s", profile.Verdict, len(profile.RequiredTools), profile.MinVersion)
				}
			} else if profile.Source != "fallback" {
				t.Fatalf("unverified status %d adopted", finalStatus)
			}
		})
	}
}

func TestRejectedBaselineReportsUpstreamAndStopsDiscovery(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"type":"FreeTierError","message":"client rejected"}}`))
	}))
	defer upstream.Close()
	p := NewProber(upstream.URL, "test-model")
	profile := p.Probe(context.Background())
	if profile.Source != "fallback" || len(profile.Conditions) != 1 || hits.Load() > 4 {
		t.Fatalf("unverified baseline continued discovery: %+v hits=%d", profile, hits.Load())
	}
	c := profile.Conditions[0]
	if c.OK || c.Inconclusive || c.Status != 403 || !strings.Contains(c.Body, "FreeTierError") || strings.Contains(c.Detail, "known-good") {
		t.Fatalf("misleading baseline diagnostics: %+v", c)
	}
}

func TestFailedProbeBackoffAndRetainsCompleteLastGoodProfile(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	cache := NewCache(NewProber(upstream.URL, "test-model"), time.Hour)
	good := Fallback()
	good.Source, good.Verdict = "probe", "ok"
	good.UserAgent, good.MinVersion = "configured/2.0", "2.0"
	good.SessionPrefix, good.SessionHex, good.SessionAlnum = "custom_", 10, 16
	good.RequiresStream, good.AcceptsToolSuperset = false, false
	good.RequiredTools = []string{"read"}
	cache.lastGood, cache.current = good, good
	for range 2 {
		failed := cache.Refresh(context.Background())
		if failed.Source != "fallback" || failed.Verdict == "ok" || failed.SessionPrefix != "custom_" || failed.SessionHex != 10 || failed.SessionAlnum != 16 || failed.RequiresStream || failed.AcceptsToolSuperset || failed.UserAgent != good.UserAgent || failed.MinVersion != good.MinVersion || len(failed.RequiredTools) != 1 {
			t.Fatalf("failed probe replaced working settings: %+v", failed)
		}
		if !failed.Conditions[0].Inconclusive {
			t.Fatal("upstream outage was reported as a gate rejection")
		}
	}
	for range 5 {
		cache.Current(context.Background())
	}
	if hits.Load() != 2 {
		t.Fatalf("failure triggered repeated probes: hits=%d", hits.Load())
	}
	if good.Source != "probe" || good.Verdict != "ok" {
		t.Fatal("failure mutated verified profile")
	}
	cache.lastAttempt = time.Now().Add(-2 * time.Minute)
	cache.Current(context.Background())
	if hits.Load() != 3 {
		t.Fatal("probe did not resume after failure backoff")
	}
}

func TestNegativeControlsDoNotPassDuringUpstreamOutage(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	profile := NewProber(upstream.URL, "test-model").Probe(context.Background())
	for _, c := range profile.Conditions {
		switch c.Name {
		case "user_agent_prefix", "session_shape", "stream_required", "tool_superset":
			if c.OK || !c.Inconclusive {
				t.Fatalf("503 falsely proved a gate condition: %+v", c)
			}
		}
	}
}
