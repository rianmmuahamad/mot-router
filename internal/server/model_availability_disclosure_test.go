package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rian/mot-router/internal/zen"
)

// writeAvailability publishes a snapshot for the test to read.
func writeAvailability(t *testing.T, models map[string]int, available []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "availability.json")
	raw, err := json.Marshal(modelAvailability{
		UpdatedAt: time.Now().Unix(),
		Models:    models,
		Available: available,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestModelListReportsAvailability(t *testing.T) {
	path := writeAvailability(t,
		map[string]int{"deepseek-v4.1-flash-free": 429, "beam-free": 200},
		[]string{"beam-free"},
	)
	t.Setenv(availabilityEnv, path)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-v4.1-flash-free"},{"id":"beam-free"}]}`))
	}))
	defer upstream.Close()

	s := New(Config{}, openTestStore(t), zen.New(upstream.URL, "", ""), nil)
	w := httptest.NewRecorder()
	s.handleModels(w, httptest.NewRequest("GET", "/v1/models", nil))

	var out struct {
		Data []struct {
			ID        string `json:"id"`
			Available *bool  `json:"available"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range out.Data {
		// Only the Zen models carry an availability flag; the static entries
		// for other providers are not measured here.
		if !strings.HasPrefix(m.ID, "cline") && !strings.HasPrefix(m.ID, "zai") &&
			!strings.HasPrefix(m.ID, "stealth") && !strings.HasPrefix(m.ID, "tokenharbor") &&
			!strings.HasPrefix(m.ID, "codebuddy/") && !strings.HasSuffix(m.ID, ":free") {
			if m.Available == nil {
				t.Fatalf("model %s has no availability flag", m.ID)
			}
			seen[m.ID] = *m.Available
		}
	}
	if seen["beam-free"] != true {
		t.Error("available model reported as unavailable")
	}
	if seen["deepseek-v4.1-flash-free"] != false {
		t.Error("spent model reported as available")
	}
}

func TestFallbackModelIsDisclosedToClient(t *testing.T) {
	// The router must not silently substitute a model: the client asked for
	// one id and has to learn which one actually answered.
	path := writeAvailability(t,
		map[string]int{"deepseek-v4.1-flash-free": 429, "beam-free": 200},
		[]string{"beam-free"},
	)
	t.Setenv(availabilityEnv, path)

	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		served = append(served, body.Model)
		if body.Model == "deepseek-v4.1-flash-free" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"FreeUsageLimitError","message":"Rate limit exceeded."}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	st := openTestStore(t)
	key, err := st.CreateKey("fixture")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{DefaultModel: "deepseek-v4.1-flash-free"}, st, zen.New(upstream.URL, "", ""), nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash-free","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	s.handleProxy(w, req, "openai")

	if got := w.Header().Get("X-Mot-Served-Model"); got != "beam-free" {
		t.Fatalf("served model header = %q, want beam-free", got)
	}
	if got := w.Header().Get("X-Mot-Requested-Model"); got != "deepseek-v4.1-flash-free" {
		t.Fatalf("requested model header = %q", got)
	}
	// The router probes the gate before inference, so the upstream sees an
	// extra request without a model id. Only the inference calls matter here.
	var inference []string
	for _, m := range served {
		if m != "" {
			inference = append(inference, m)
		}
	}
	if len(inference) != 2 || inference[0] != "deepseek-v4.1-flash-free" || inference[1] != "beam-free" {
		t.Fatalf("upstream saw %q, want the requested model then the fallback", inference)
	}
}
