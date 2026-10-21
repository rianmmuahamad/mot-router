package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rian/mot-router/internal/zen"
)

func TestProviderRoutingPreservesExplicitGLMSelection(t *testing.T) {
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"glm-5.3-flash-free"},{"id":"glm-5.2"}]}`)
	}))
	defer catalog.Close()
	s := &Server{store: openTestStore(t), zen: zen.New(catalog.URL, "", "")}
	for _, tc := range []struct{ request, provider, model string }{
		{"opencode-zen/glm-5.3-flash-free", ProviderZen, "glm-5.3-flash-free"},
		{"OPENCODE-ZEN/glm-5.3-flash-free", ProviderZen, "glm-5.3-flash-free"},
		{"opencode-zen/zai/glm-future-free", ProviderZen, "zai/glm-future-free"},
		{"opencode-zen/glm-unavailable-free", ProviderZen, "glm-unavailable-free"},
		{"glm-5.3-flash-free", ProviderZen, "glm-5.3-flash-free"},
		{"glm-5.2", ProviderZen, "glm-5.2"},
		{"cline/glm-5.2", ProviderCline, "zai/glm-5.2"},
		{"cline/zai/glm-5.2", ProviderCline, "zai/glm-5.2"},
		{"cline/glm-other", ProviderCline, "glm-other"},
		{"zai/glm-5.2", ProviderCline, "zai/glm-5.2"},
		{"codebuddy/glm-5.2", ProviderCodeBuddy, "glm-5.2"},
		{"tokenharbor/zai/glm-5.2:free", ProviderTokenHarbor, "zai/glm-5.2:free"},
	} {
		t.Run(tc.request, func(t *testing.T) {
			got, model := s.providerFor(t.Context(), tc.request)
			if got != tc.provider || model != tc.model {
				t.Fatalf("got %s/%s; want %s/%s", got, model, tc.provider, tc.model)
			}
		})
	}
}

func TestPlaygroundSendsExplicitGLMToZen(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "glm-5.3-flash-free" {
			t.Errorf("wrong upstream model %q", body.Model)
		}
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	st := openTestStore(t)
	if err := st.SetSetting("mihomo_enabled", "false"); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, zen: zen.New(upstream.URL, "", "")}
	request := httptest.NewRequest("POST", "/api/playground", strings.NewReader(`{"model":"opencode-zen/glm-5.3-flash-free","prompt":"hi"}`))
	w := httptest.NewRecorder()
	s.handlePlayground(w, request)
	if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), `"text":"OK"`) {
		t.Fatalf("unexpected response calls=%d status=%d body=%s", calls, w.Code, w.Body.String())
	}
}
