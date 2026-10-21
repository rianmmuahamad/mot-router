package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func timestampValid(id string, descending bool, counterBits int) bool {
	if len(id) != 30 {
		return false
	}
	raw, e := strconv.ParseUint(id[4:16], 16, 64)
	if e != nil {
		return false
	}
	if descending {
		raw = ^raw & timestampMask
	}
	now := uint64(time.Now().UnixMilli()) & (timestampMask >> counterBits)
	stamp := raw >> counterBits
	return stamp <= now+100 && stamp+1000 >= now && raw&((1<<counterBits)-1) > 0
}
func identityFixture(t *testing.T, version string, counterBits int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		tools := map[string]bool{}
		for _, tool := range body.Tools {
			tools[tool.Name] = true
		}
		if !strings.HasPrefix(r.UserAgent(), "opencode/"+version) || !timestampValid(r.Header.Get("x-opencode-session"), true, counterBits) || !timestampValid(r.Header.Get("x-opencode-request"), false, counterBits) || !body.Stream || !tools["read"] {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(testProbeCompletion))
	}))
}

func TestAutomaticallyRepairsLegacyHashIdentifiers(t *testing.T) {
	t.Setenv("ZEN_RESPONSES_MODELS", "zen-fixture-free")
	upstream := identityFixture(t, "1.19.0", 12)
	defer upstream.Close()
	p := NewProber(upstream.URL, "zen-fixture-free")
	legacy := Fallback()
	legacy.Identity.Mode = "hash"
	p.setSeed(legacy)
	profile := p.Probe(context.Background())
	if profile.Verdict != "ok" || profile.Identity.Mode != "timestamp" || profile.RepairSource == "" {
		t.Fatalf("did not automatically repair hashed ID: %+v", profile)
	}
	if !timestampValid(SessionIDWithPolicy("next-turn", profile.Identity), true, 12) {
		t.Fatal("candidate policy was not applied to future IDs")
	}
}

func TestAutomaticallyAdoptsNewOfficialVersionAndCounterLayout(t *testing.T) {
	t.Setenv("ZEN_RESPONSES_MODELS", "zen-fixture-free")
	upstream := identityFixture(t, "1.20.1", 13)
	defer upstream.Close()
	p := NewProber(upstream.URL, "zen-fixture-free")
	var resolved atomic.Int32
	p.Resolve = func(context.Context) (*OfficialHints, error) {
		resolved.Add(1)
		policy := DefaultIdentity()
		policy.CounterBits = 13
		return &OfficialHints{Version: "1.20.1", Identity: policy}, nil
	}
	profile := p.Probe(context.Background())
	if profile.Verdict != "ok" || profile.Identity.CounterBits != 13 || !strings.HasPrefix(profile.UserAgent, "opencode/1.20.1") || resolved.Load() != 1 {
		t.Fatalf("new metadata not adopted: %+v", profile)
	}
	if profile.VerifiedVersion != "1.20.1" || profile.MinVersion != "" {
		t.Fatalf("dynamic version was not verified: %s", profile.VerifiedVersion)
	}
}

func TestConcurrentRecoveryCoalescesAndUnsupportedChangeKeepsProfile(t *testing.T) {
	t.Setenv("ZEN_RESPONSES_MODELS", "zen-fixture-free")
	upstream := identityFixture(t, "1.19.0", 12)
	defer upstream.Close()
	c := NewCache(NewProber(upstream.URL, "zen-fixture-free"), time.Hour)
	legacy := Fallback()
	legacy.Identity.Mode = "hash"
	legacy.Source = "probe"
	legacy.Verdict = "ok"
	c.Restore(legacy)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !c.Recover(context.Background()) {
				t.Error("concurrent recovery failed")
			}
		}()
	}
	wg.Wait()
	if c.Automatic().RecoveryAttempts != 1 || c.Automatic().Repairs != 1 {
		t.Fatalf("recovery storm: %+v", c.Automatic())
	}
	good := c.Snapshot()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
	}))
	defer bad.Close()
	c.prober.BaseURL = bad.URL
	c.prober.Resolve = func(context.Context) (*OfficialHints, error) {
		policy := DefaultIdentity()
		policy.Mode = "unknown-code"
		return &OfficialHints{Version: "1.21.0", Identity: policy}, nil
	}
	c.recoveryAt = time.Time{}
	if c.Recover(context.Background()) {
		t.Fatal("unsupported change was marked repaired")
	}
	after := c.Snapshot()
	if after.Identity != good.Identity || after.UserAgent != good.UserAgent || after.Source != "fallback" {
		t.Fatal("unverified candidate replaced good identity")
	}
	attempts := c.Automatic().RecoveryAttempts
	c.Recover(context.Background())
	if c.Automatic().RecoveryAttempts != attempts {
		t.Fatal("failed recovery ignored cooldown")
	}
}

func TestOfficialLayoutParserRejectsUnknownCodeAndReadsParameters(t *testing.T) {
	source := `const LENGTH = 28; let now = BigInt(currentTimestamp) * BigInt(0x2000) + BigInt(counter); now = direction === "descending" ? ~now : now; const timeBytes = Buffer.alloc(6)`
	p, e := ParseOfficialIdentity(source)
	if e != nil || p.CounterBits != 13 || p.TimestampBytes != 6 || p.SuffixLength != 16 {
		t.Fatalf("layout not read: %+v %v", p, e)
	}
	for _, bad := range []string{"eval(remoteCode)", strings.ReplaceAll(source, "0x2000", "1234"), strings.ReplaceAll(source, "28", "1000000")} {
		if _, e := ParseOfficialIdentity(bad); e == nil {
			t.Fatal("unknown/oversized executable layout accepted")
		}
	}
}

func TestRecoveryForDifferentModelCannotReuseSuccess(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(testProbeCompletion))
	}))
	defer upstream.Close()
	c := NewCache(NewProber(upstream.URL, DefaultProbeModel), time.Hour)
	if !c.RecoverModel(context.Background(), DefaultProbeModel) {
		t.Fatal("initial recovery failed")
	}
	if c.RecoverModel(context.Background(), "glm-5.3-flash-free") {
		t.Fatal("DeepSeek result incorrectly verified GLM")
	}
	if hits.Load() != 2 {
		t.Fatal("different-model result caused redundant probes")
	}
}
