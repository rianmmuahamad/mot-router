package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rian/mot-router/internal/zen"
)

func TestInitialStreamRetryHonorsRotationAndSingleNodePool(t *testing.T) {
	t.Setenv("ZEN_RESPONSES_MODELS", "zen-fixture-free")
	for _, mode := range []string{"rotation_off", "rotation_on", "single_node"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("MOT_NODEBANK_RUNTIME", "0")
			var calls, selections atomic.Int32
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					selections.Add(1)
					w.WriteHeader(204)
					return
				}
				if r.URL.Path != "/proxies" {
					http.NotFound(w, r)
					return
				}
				nodes := []string{"first"}
				if mode != "single_node" {
					nodes = append(nodes, "second")
				}
				json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{
					"MOT":    map[string]any{"type": "Selector", "now": "first", "all": nodes},
					"first":  map[string]any{"type": "Http", "alive": true, "history": []any{map[string]any{"delay": 10}}},
					"second": map[string]any{"type": "Http", "alive": true, "history": []any{map[string]any{"delay": 20}}},
				}})
			}))
			defer control.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					w.WriteHeader(200) // upstream closes before producing any event
					return
				}
				w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"RETRY_OK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"))
			}))
			defer proxy.Close()
			st := openTestStore(t)
			for k, v := range map[string]string{settingMihomoEnabled: "1", settingMihomoController: control.URL, settingMihomoProxy: proxy.URL, settingMihomoGroup: "MOT", settingMihomoRotate: "1"} {
				st.SetSetting(k, v)
			}
			if mode == "rotation_off" {
				st.SetSetting(settingMihomoRotate, "0")
			}
			s := New(Config{}, st, zen.New("http://fixture.invalid/v1", "", ""), nil)
			events, status, err, _ := s.mihomoStreamZenOnce(t.Context(), "zen-fixture-free", "fixture", map[string]any{"stream": true}, requestRecord{})
			if err != nil || status != 200 {
				t.Fatalf("initial stream was not retried: status=%d err=%v", status, err)
			}
			text := ""
			for event := range events {
				text += event.Delta
			}
			if text != "RETRY_OK" || calls.Load() != 2 {
				t.Fatalf("bad retry result: text=%s calls=%d", text, calls.Load())
			}
			want := int32(0)
			if mode == "rotation_on" {
				want = 1
			}
			if selections.Load() != want {
				t.Fatalf("rotation switch ignored: selections=%d want=%d", selections.Load(), want)
			}
		})
	}
}
