package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/zen"
)

func TestPlaygroundUsesVerifiedV2Tools(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		names := map[string]bool{}
		for _, tool := range b.Tools {
			names[tool.Function.Name] = true
		}
		if !names["shell"] || !names["execute"] || !names["skill"] {
			t.Errorf("Playground bypassed verified roster: %+v", names)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"PLAY_OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()
	cache := gate.NewCache(gate.NewProber(upstream.URL, ""), time.Hour)
	profile := gate.Fallback()
	profile.Source, profile.Verdict, profile.CheckedAt = "probe", "ok", time.Now()
	profile.ClientFormat, profile.UserAgent = gate.ClientV2, "opencode/latest/2.0.29/cli"
	profile.RequiredTools = []string{"shell", "read", "skill", "execute"}
	if !cache.Restore(profile) {
		t.Fatal("fixture rejected")
	}
	s := New(Config{}, openTestStore(t), zen.New(upstream.URL, "", ""), cache)
	w := httptest.NewRecorder()
	s.handlePlayground(w, httptest.NewRequest("POST", "/api/playground", strings.NewReader(`{"model":"opencode-zen/deepseek-v4.1-flash-free","prompt":"Reply PLAY_OK"}`)))
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["text"] != "PLAY_OK" {
		t.Fatal(w.Body.String())
	}
}

func TestGateEndToEndUsesProbeModelInsteadOfDeprecatedDefault(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if r.URL.Path != "/chat/completions" || b["model"] != "deepseek-v4.1-flash-free" {
			t.Errorf("wrong diagnostic target %s %+v", r.URL.Path, b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"GATE_OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()
	cache := gate.NewCache(gate.NewProber(upstream.URL, ""), time.Hour)
	profile := gate.Fallback()
	profile.Source, profile.Verdict, profile.CheckedAt = "probe", "ok", time.Now()
	if !cache.Restore(profile) {
		t.Fatal("fixture profile rejected")
	}
	s := New(Config{DefaultModel: "muse-spark-1.3-contributor-free"}, openTestStore(t), zen.New(upstream.URL, "", ""), cache)
	w := httptest.NewRecorder()
	s.handleGateTest(w, httptest.NewRequest("POST", "/api/gate/test", nil))
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["ok"] != true || out["model"] != "deepseek-v4.1-flash-free" {
		t.Fatal(w.Body.String())
	}
}
