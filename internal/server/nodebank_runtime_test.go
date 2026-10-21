package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rian/mot-router/internal/mihomo"
	"github.com/rian/mot-router/internal/zen"
)

func TestNativeRotationSkipsSharedExitIP(t *testing.T) {
	t.Setenv("MOT_NODEBANK_RUNTIME", "1")
	selected := ""
	proxies := map[string]mihomo.Group{
		"MOT":     {Name: "MOT", Type: "Selector", Now: "limited", All: []string{"DIRECT", "limited", "sibling", "working", "pending"}},
		"DIRECT":  {Name: "DIRECT", Type: "Direct", ExitIP: "203.0.113.10"},
		"limited": {Name: "limited", Type: "vless", Alive: true, ExitIP: "203.0.113.10", History: []mihomo.History{{Delay: 10}}},
		"sibling": {Name: "sibling", Type: "trojan", Alive: true, ExitIP: "203.0.113.10", History: []mihomo.History{{Delay: 15}}},
		"working": {Name: "working", Type: "vmess", Alive: true, ExitIP: "203.0.113.20", History: []mihomo.History{{Delay: 20}}},
		"pending": {Name: "pending", Type: "vless", Alive: false},
	}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PUT":
			var v struct {
				Name string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&v)
			selected = v.Name
			w.WriteHeader(204)
		case r.URL.Path == "/proxies":
			json.NewEncoder(w).Encode(map[string]any{"proxies": proxies})
		default:
			http.NotFound(w, r)
		}
	}))
	defer controller.Close()
	srv := &Server{store: openTestStore(t)}
	srv.penalizeNode("MOT", "limited")
	_, node, err := srv.rotateMihomoNode(t.Context(), mihomo.New(controller.URL, "http://127.0.0.1:1", ""), "MOT")
	if err != nil || node != "working" || selected != "working" {
		t.Fatalf("rotation failed to skip siblings: %s %s %v", node, selected, err)
	}
}
func TestNativeStatusUsesVPSResultsWithoutExternalIPProbe(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			json.NewEncoder(w).Encode(map[string]string{"version": "nodebank-native/1"})
		case "/status":
			json.NewEncoder(w).Encode(map[string]any{"total": 2, "counts": map[string]int{"healthy": 1, "pending": 1}})
		case "/proxies":
			json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]mihomo.Group{
				"MOT":        {Name: "MOT", Type: "Selector", Now: "NB-good", All: []string{"DIRECT", "NB-good", "NB-pending"}},
				"NB-good":    {Name: "NB-good", Type: "vless", Alive: true, ExitIP: "203.0.113.7", History: []mihomo.History{{Delay: 20}}},
				"NB-pending": {Name: "NB-pending", Type: "trojan"},
				"DIRECT":     {Name: "DIRECT", Type: "Direct"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer controller.Close()
	st := openTestStore(t)
	st.SetSetting(settingMihomoController, controller.URL)
	st.SetSetting(settingMihomoProxy, "http://127.0.0.1:1")
	st.SetSetting(settingMihomoGroup, "MOT")
	srv := &Server{store: st}
	rec := httptest.NewRecorder()
	srv.handleMihomoStatus(rec, httptest.NewRequest("GET", "/api/mihomo", nil))
	var got mihomoStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "" || got.EgressIP != "203.0.113.7" || len(got.Groups) != 1 || got.NativeStatus == nil {
		t.Fatalf("native status not integrated: %s", rec.Body.String())
	}
	if len(got.Groups[0].ValidatedIPs) != 1 || !strings.Contains(rec.Body.String(), "NB-good") {
		t.Fatal("validated candidates missing from MOT group")
	}
}

func TestPlaygroundRoutesAndRotatesThroughMOT(t *testing.T) {
	t.Setenv("ZEN_RESPONSES_MODELS", "deepseek-v4.1-flash-free")
	selected := ""
	control := startStubMihomo(t, &selected, []string{"limited", "working"})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"deepseek-v4.1-flash-free"}]}`))
			return
		}
		t.Error("playground inference bypassed MOT proxy")
		http.Error(w, "direct inference forbidden in fixture", 500)
	}))
	defer origin.Close()
	calls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(502)
			w.Write([]byte(`{"error":"upstream tunnel broken"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer proxy.Close()
	st := openTestStore(t)
	st.SetSetting(settingMihomoEnabled, "1")
	st.SetSetting(settingMihomoRotate, "1")
	st.SetSetting(settingMihomoGroup, "MOT")
	st.SetSetting(settingMihomoController, control)
	st.SetSetting(settingMihomoProxy, proxy.URL)
	srv := &Server{store: st, zen: zen.New(origin.URL, "fixture", ""), cfg: Config{DefaultModel: "deepseek-v4.1-flash-free", DefaultProvider: ProviderZen}}
	rec := httptest.NewRecorder()
	srv.handlePlayground(rec, httptest.NewRequest("POST", "/api/playground", strings.NewReader(`{"model":"opencode-zen/deepseek-v4.1-flash-free","prompt":"Reply OK"}`)))
	var reply map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply["text"] != "OK" || calls != 2 || selected == "" || selected == "limited" {
		t.Fatalf("playground failed to route/retry: %s, calls=%d selected=%s", rec.Body.String(), calls, selected)
	}
}
