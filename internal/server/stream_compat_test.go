package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rian/mot-router/internal/zen"
)

func TestCanceledClientReleasesBlockedStreamForwarders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan zen.Event, 128)
	for range 128 {
		in <- zen.Event{Type: "response.output_text.delta", Delta: "fixture"}
	}
	closed := make(chan struct{})
	out := detachCancel(ctx, replayFirst(ctx, zen.Event{}, in), func() { close(closed) })
	deadline := time.Now().Add(time.Second)
	for len(out) < cap(out) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("canceled request leaked a blocked forwarding goroutine")
	}
	for range out {
	}
}

func TestStreamFailuresReachClientAndRequestLog(t *testing.T) {
	for _, tc := range []struct {
		name, surface, body string
		status              int
	}{
		{"JSON", "openai", `{"model":"opencode-zen/fixture-free","stream":false,"messages":[{"role":"user","content":"fixture"}]}`, 502},
		{"SSE", "openai", `{"model":"opencode-zen/fixture-free","stream":true,"messages":[{"role":"user","content":"fixture"}]}`, 200},
		{"Anthropic", "anthropic", `{"model":"opencode-zen/fixture-free","max_tokens":128,"messages":[{"role":"user","content":"fixture"}]}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte("data: {\"error\":{\"type\":\"fixture_error\",\"message\":\"LOCAL_FAILURE\"}}\n\n"))
			}))
			defer up.Close()
			st := openTestStore(t)
			key, err := st.CreateKey("fixture")
			if err != nil {
				t.Fatal(err)
			}
			s := New(Config{}, st, zen.New(up.URL, "fixture", ""), nil)
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+key.Key)
			w := httptest.NewRecorder()
			s.handleProxy(w, r, tc.surface)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), "LOCAL_FAILURE") || strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), "message_stop") {
				t.Fatalf("hidden error: %d %s", w.Code, w.Body.String())
			}
			rows, err := st.RecentRequests(1)
			if err != nil || len(rows) != 1 || rows[0].Status != 502 || rows[0].Error == nil {
				t.Fatalf("misleading request log: %+v %v", rows, err)
			}
		})
	}
}
func TestPlaygroundDoesNotLogStreamErrorAsSuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"error\":{\"message\":\"LOCAL_FAILURE\"}}\n\n"))
	}))
	defer up.Close()
	st := openTestStore(t)
	s := New(Config{}, st, zen.New(up.URL, "fixture", ""), nil)
	r := httptest.NewRequest("POST", "/api/playground", strings.NewReader(`{"model":"opencode-zen/fixture-free","prompt":"fixture"}`))
	w := httptest.NewRecorder()
	s.handlePlayground(w, r)
	rows, _ := st.RecentRequests(1)
	if !strings.Contains(w.Body.String(), "LOCAL_FAILURE") || len(rows) != 1 || rows[0].Status != 502 {
		t.Fatalf("false playground success: %s %+v", w.Body.String(), rows)
	}
}
