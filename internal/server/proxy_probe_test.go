package server

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rian/mot-router/internal/mihomo"
	"github.com/rian/mot-router/internal/zen"
)

// Local TLS + CONNECT fixture exercises both IP lookup and inference, including
// the actual native node header, without contacting any external service.
func TestProxyProbePinsNodeAndReportsProviderResult(t *testing.T) {
	for _, tc := range []struct {
		name, body, stage string
		status            int
		ok                bool
	}{
		{"access denial", `{"error":{"type":"FreeTierError"}}`, "upstream_access", 403, false},
		{"quota", `{"error":{"type":"FreeUsageLimitError"}}`, "upstream_quota", 429, false},
		{"deprecated", `{"error":{"type":"ModelDeprecated"}}`, "upstream_request", 410, false},
		{"chat truncated", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", "upstream_stream", 200, false},
		{"chat finished", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", "", 200, true},
		{"bounded probe", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", "", 200, true},
		{"chat error", "data: {\"error\":{\"message\":\"failed\"}}\n\ndata: [DONE]\n\n", "upstream_stream", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MOT_NODEBANK_RUNTIME", "1")
			var inference atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					io.WriteString(w, "203.0.113.9\n")
					return
				}
				inference.Add(1)
				var body struct {
					Model string `json:"model"`
					Tools []struct {
						Name     string `json:"name"`
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != "deepseek-v4.1-flash-free" || r.URL.Path != "/chat/completions" {
					t.Errorf("wrong diagnostic model/endpoint: %s %s", body.Model, r.URL.Path)
				}
				if len(body.Tools) != 1 || (body.Tools[0].Name != "fixture_tool" && body.Tools[0].Function.Name != "fixture_tool") {
					t.Errorf("test bypassed existing profile: %+v", body.Tools)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer origin.Close()
			// Trust only this isolated fixture's TLS connection. Every CONNECT
			// is redirected to its local listener, including the IP lookup.
			oldTransport := http.DefaultTransport
			tr := oldTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local test fixture only
			http.DefaultTransport = tr
			defer func() { http.DefaultTransport = oldTransport; tr.CloseIdleConnections() }()
			var mu sync.Mutex
			var pinned []string
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					t.Errorf("unexpected proxy method: %s", r.Method)
					w.WriteHeader(500)
					return
				}
				mu.Lock()
				pinned = append(pinned, r.Header.Get("X-Nodebank-Node"))
				mu.Unlock()
				up, err := net.Dial("tcp", origin.Listener.Addr().String())
				if err != nil {
					w.WriteHeader(502)
					return
				}
				defer up.Close()
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				rw.Flush()
				go func() { io.Copy(up, rw); up.Close() }()
				io.Copy(conn, up)
			}))
			defer proxy.Close()
			var selections atomic.Int32
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					selections.Add(1)
				}
				json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{"MOT": map[string]any{"name": "MOT", "type": "Selector", "now": "NB-picked"}}})
			}))
			defer control.Close()
			zc := zen.New(origin.URL, "fixture", "")
			zc.Gate = &zen.GateProfile{RequiredTools: []string{"fixture_tool"}, AcceptsToolSuperset: true}
			model := "muse-spark-1.3-contributor-free"
			st := openTestStore(t)
			// Previously enabled pool settings and long cooldowns must not
			// reactivate parallel probes or block the restored single-node test.
			_ = st.SetSetting("zen_pool.config.v1", `{"enabled":true,"parallelism":10}`)
			_ = st.SetSetting("zen_pool.policy.v1", `{"fixture":{"cooldown_until":"2099-10-18T00:00:00Z","reason":"rate_limit"}}`)
			_ = st.SetSetting("default_model", model)
			s := New(Config{DefaultModel: model}, st, zc, nil)
			s.mihomoCached = mihomo.New(control.URL, proxy.URL, "")
			s.mihomoKey = "http://127.0.0.1:9093|http://127.0.0.1:7893|"
			w := httptest.NewRecorder()
			s.handleMihomoTest(w, httptest.NewRequest(http.MethodPost, "/api/mihomo/test", nil))
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["ok"] != tc.ok || got["proxy_ok"] != true || got["node"] != "NB-picked" || got["status"] != float64(tc.status) || got["model"] != "deepseek-v4.1-flash-free" {
				t.Fatalf("wrong diagnostic result: %s", w.Body.String())
			}
			if tc.stage != "" && got["failure_stage"] != tc.stage {
				t.Fatal(w.Body.String())
			}
			if inference.Load() != 1 || selections.Load() != 0 || got["rotated"] != false || got["mode"] != "single_node" {
				t.Fatalf("diagnostic retried/rotated: inference=%d selections=%d %s", inference.Load(), selections.Load(), w.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(pinned) != 2 || pinned[0] != "NB-picked" || pinned[1] != "NB-picked" {
				t.Fatalf("IP lookup and inference were not pinned together: %v", pinned)
			}
		})
	}
}
