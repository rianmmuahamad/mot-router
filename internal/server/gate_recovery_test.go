package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/zen"
)

func TestAutomaticRecoveryReplaysOriginalRequestAndRestoresProfile(t *testing.T) {
	t.Setenv("ZEN_RESPONSES_MODELS", "zen-fixture-free")
	var rejected atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := r.Header.Get("x-opencode-session")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		valid := false
		if len(session) == 30 {
			if value, e := strconv.ParseUint(session[4:16], 16, 64); e == nil {
				stamp := (^value & ((1 << 48) - 1)) >> 12
				now := uint64(time.Now().UnixMilli()) & ((1 << 36) - 1)
				valid = stamp <= now+100 && stamp+1000 >= now
			}
		}
		toolNames := map[string]bool{}
		if tools, ok := body["tools"].([]any); ok {
			for _, item := range tools {
				if tool, ok := item.(map[string]any); ok {
					name, _ := tool["name"].(string)
					toolNames[name] = true
				}
			}
		}
		if !valid || !strings.HasPrefix(r.UserAgent(), "opencode/") || r.Header.Get("x-opencode-request") == "" || body["stream"] != true || !toolNames["bash"] || !toolNames["read"] {
			rejected.Add(1)
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"GATE_OK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer upstream.Close()
	p := gate.NewProber(upstream.URL, "zen-fixture-free")
	p.ToolPool = []string{"bash", "read"}
	cache := gate.NewCache(p, time.Hour)
	legacy := gate.Fallback()
	legacy.RequiredTools = []string{"bash", "read"}
	legacy.Source = "probe"
	legacy.Verdict = "ok"
	legacy.Identity.Mode = "hash"
	if !cache.Restore(legacy) {
		t.Fatal("fixture profile not restored")
	}
	st := openTestStore(t)
	client := zen.New(upstream.URL, "", "")
	s := New(Config{}, st, client, cache)
	raw := []byte(`{"model":"zen-fixture-free","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"execute_command","parameters":{"type":"object"}}}]}`)
	body, mapping, err := s.translateZenBody(raw, "openai", "zen-fixture-free")
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := false
	ctx := context.WithValue(context.Background(), gateRebuildKey{}, func() (map[string]any, error) {
		rebuilt = true
		body, mapping, err = s.translateZenBody(raw, "openai", "zen-fixture-free")
		return body, err
	})
	events, status, err, _ := s.mihomoStreamZen(ctx, "zen-fixture-free", "conversation", body, requestRecord{})
	if err != nil || status != 200 || !rebuilt {
		t.Fatalf("request not recovered: status=%d rebuild=%v err=%v", status, rebuilt, err)
	}
	text := ""
	for event := range events {
		text += event.Delta
	}
	if text != "GATE_OK" {
		t.Fatal("reply lost after recovery")
	}
	if mapping.Originals["execute_command"] != "execute_command" {
		t.Fatal("client tool mapping lost during replay")
	}
	if cache.Automatic().Repairs != 1 || st.GetSetting("gate.verified_profile.v1") == "" {
		t.Fatal("verified repair was not recorded and persisted")
	}
	fresh := gate.NewCache(gate.NewProber(upstream.URL, "zen-fixture-free"), time.Hour)
	New(Config{}, st, zen.New(upstream.URL, "", ""), fresh)
	if fresh.Snapshot().Identity.Mode != "timestamp" || fresh.NeedsRefresh() {
		t.Fatal("restart did not restore the verified profile")
	}
	if rejected.Load() == 0 {
		t.Fatal("fixture did not exercise rejection")
	}
}

func TestQuotaOrUnrelated403DoesNotStartHeaderRepair(t *testing.T) {
	for _, status := range []int{429, 403} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":{"type":"RegionError"}}`))
		}))
		cache := gate.NewCache(gate.NewProber(upstream.URL, "zen-fixture-free"), time.Hour)
		s := New(Config{}, openTestStore(t), zen.New(upstream.URL, "", ""), cache)
		s.mihomoStreamZen(context.Background(), "zen-fixture-free", "conversation", map[string]any{}, requestRecord{})
		if cache.Automatic().RecoveryAttempts != 0 {
			t.Fatal("quota or unrelated 403 triggered identity repair")
		}
		upstream.Close()
	}
}
