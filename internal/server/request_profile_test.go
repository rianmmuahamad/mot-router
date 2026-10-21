package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/zen"
)

func TestCapturedOMPToolSchemasAndOutputLimitSurviveGLMTranslation(t *testing.T) {
	raw, err := os.ReadFile("../../tools/diagnostics/results/omp-tool-schemas.json")
	if err != nil {
		t.Fatal(err)
	}
	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]any{
		"model": "opencode-zen/glm-5.3-flash-free", "max_completion_tokens": 256,
		"messages": []map[string]any{{"role": "system", "content": "Local fixture"}, {"role": "user", "content": "hi"}},
		"tools":    tools,
	})
	s := New(Config{}, openTestStore(t), zen.New("", "", ""), nil)
	body, _, err := s.translateZenBodyWithProfile(request, "openai", "glm-5.3-flash-free", zen.ProfileFromGate(gate.Fallback()))
	if err != nil {
		t.Fatal(err)
	}
	chat := zen.ConvertToChatPayload("glm-5.3-flash-free", body)
	if chat["model"] != "glm-5.3-flash-free" || chat["max_tokens"] != 256 {
		t.Fatalf("model or output limit lost: model=%v limit=%v", chat["model"], chat["max_tokens"])
	}
	actual := map[string]any{}
	for _, tool := range chat["tools"].([]map[string]any) {
		fn := tool["function"].(map[string]any)
		actual[fn["name"].(string)] = fn["parameters"]
	}
	for _, tool := range tools {
		fn := tool["function"].(map[string]any)
		if !reflect.DeepEqual(actual[fn["name"].(string)], fn["parameters"]) {
			t.Errorf("OMP schema changed for %s", fn["name"])
		}
	}
}

func TestUnchangedVerifiedProfileDoesNotReplayRejectedRequest(t *testing.T) {
	var inference, probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["max_tokens"] == float64(16) {
			probes.Add(1)
			w.Write([]byte(fixtureProbeCompletion))
			return
		}
		inference.Add(1)
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
	}))
	defer upstream.Close()
	cache := gate.NewCache(gate.NewProber(upstream.URL, "glm-5.3-flash-free"), time.Hour)
	cache.Refresh(context.Background())
	s := New(Config{}, openTestStore(t), zen.New(upstream.URL, "", ""), cache)
	body, _, err := s.translateZenBody([]byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":128}`), "openai", "glm-5.3-flash-free")
	if err != nil {
		t.Fatal(err)
	}
	_, status, err, _ := s.mihomoStreamZen(context.Background(), "glm-5.3-flash-free", "fixture", body, requestRecord{})
	if status != 403 || err == nil || inference.Load() != 1 || probes.Load() != 4 {
		t.Fatalf("unchanged request replayed: status=%d inference=%d probes=%d", status, inference.Load(), probes.Load())
	}
}

func TestGLMRequestUsesOneSnapshotAndOneInference(t *testing.T) {
	var reads, hits atomic.Int32
	old := zen.ProfileFromGate(gate.Fallback())
	old.RequiredTools = []string{"bash", "read"}
	next := zen.ProfileFromGate(gate.Fallback())
	next.ClientFormat, next.UserAgent = gate.ClientV2, "opencode/latest/2.0.29/cli"
	next.RequiredTools = []string{"shell", "execute"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["model"] != "glm-5.3-flash-free" || r.UserAgent() != old.UserAgent || b["max_completion_tokens"] != nil {
			t.Errorf("mixed profile/model: ua=%s model=%v", r.UserAgent(), b["model"])
		}
		names := map[string]bool{}
		for _, v := range b["tools"].([]any) {
			f := v.(map[string]any)["function"].(map[string]any)
			names[f["name"].(string)] = true
		}
		if !names["bash"] || names["shell"] {
			t.Errorf("mixed tool roster: %+v", names)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"GLM_SNAPSHOT_OK\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":13,\"completion_tokens\":2,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()
	st := openTestStore(t)
	key, err := st.CreateKey("fixture")
	if err != nil {
		t.Fatal(err)
	}
	client := zen.New(upstream.URL, "", "")
	client.GateProvider = func() *zen.GateProfile {
		if reads.Add(1) == 1 {
			return old
		}
		return next
	}
	s := New(Config{}, st, client, nil)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"opencode-zen/glm-5.3-flash-free","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`))
	r.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	s.handleProxy(w, r, "openai")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "GLM_SNAPSHOT_OK") || hits.Load() != 1 || reads.Load() != 1 {
		t.Fatalf("snapshot/duplicate regression: status=%d hits=%d reads=%d body=%s", w.Code, hits.Load(), reads.Load(), w.Body.String())
	}
}

func TestGLMRecoveryProbesGLMInsteadOfDeepSeek(t *testing.T) {
	var real, probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["model"] != "glm-5.3-flash-free" {
			t.Errorf("wrong model verified: %v", b["model"])
		}
		if b["max_tokens"] == float64(16) {
			probes.Add(1)
			if len(b["tools"].([]any)) != 11 {
				t.Error("recovery reduced roster")
			}
			w.Write([]byte(fixtureProbeCompletion))
			return
		}
		n := real.Add(1)
		if n == 1 {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
			return
		}
		if len(b["tools"].([]any)) < 11 {
			t.Error("request body not rebuilt with verified roster")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"GLM_REPAIRED\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()
	p := gate.NewProber(upstream.URL, gate.DefaultProbeModel)
	p.ToolPool = []string{"bash", "edit", "glob", "grep", "read", "write", "skill", "task", "webfetch", "websearch", "question"}
	cache := gate.NewCache(p, time.Hour)
	legacy := gate.Fallback()
	legacy.ProbeModel, legacy.Source, legacy.Verdict, legacy.CheckedAt = gate.DefaultProbeModel, "probe", "ok", time.Now()
	legacy.RequiredTools = []string{"read"}
	if !cache.Restore(legacy) {
		t.Fatal("fixture profile rejected")
	}
	st := openTestStore(t)
	key, _ := st.CreateKey("fixture")
	s := New(Config{}, st, zen.New(upstream.URL, "", ""), cache)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"opencode-zen/glm-5.3-flash-free","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`))
	r.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	s.handleProxy(w, r, "openai")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "GLM_REPAIRED") || real.Load() != 2 || probes.Load() != 2 || cache.ProbeModel() != "glm-5.3-flash-free" {
		t.Fatalf("wrong recovery: status=%d real=%d probes=%d model=%s body=%s", w.Code, real.Load(), probes.Load(), cache.ProbeModel(), w.Body.String())
	}
}
