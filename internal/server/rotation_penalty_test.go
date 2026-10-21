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

	"github.com/rian/mot-router/internal/store"
)

// TestPenaltySurvivesSuccess is the regression guard for the rotation bug that
// let a limited exit back into service.
//
// Zen's free tier is a token budget over a multi-hour window, not a momentary
// refusal: an IP can answer one request and still be nearly exhausted. Clearing
// the penalty on success made rotation return to that IP immediately and the
// next burst hit the limit again.
func TestPenaltySurvivesSuccess(t *testing.T) {
	s := &Server{}
	s.penalizeNode("MOT", "node-a")

	if got := s.penalizedNodes("MOT"); !got["node-a"] {
		t.Fatal("a penalty must survive a success; only the TTL expires it")
	}
}

// TestPenaltyTTLIsHoursNotMinutes pins the default to the measured upstream
// window. At 10 minutes the router rotated straight back onto a limited IP.
func TestPenaltyTTLIsHoursNotMinutes(t *testing.T) {
	s := &Server{}
	if got := s.penaltyTTL(); got < time.Hour {
		t.Fatalf("default penalty TTL is %v; the upstream limit window is hours", got)
	}
}

// TestPenaltyTTLIsConfigurable checks the operator override, which exists so
// the window can be retuned without a rebuild.
func TestPenaltyTTLIsConfigurable(t *testing.T) {
	st := openTestStore(t)
	if err := st.SetSetting(settingNodePenaltyMinutes, "90"); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	if got := s.penaltyTTL(); got != 90*time.Minute {
		t.Fatalf("penaltyTTL = %v, want 90m", got)
	}
}

// TestRotateSkipsPenalizedNode is the end-to-end guard: after a node is
// penalized, rotation must land on a different node.
func TestRotateSkipsPenalizedNode(t *testing.T) {
	var selected string
	srv := newRotateStub(t, &selected)

	// "limited" is the group's current node and has just been rejected.
	srv.penalizeNode("MOT", "limited")

	_, node, err := srv.rotateMihomoNode(t.Context(), srv.mihomoClient(), "MOT")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if node == "limited" {
		t.Fatalf("rotation chose the penalized node %q; it must be skipped", node)
	}
	if selected == "" {
		t.Fatal("rotation never called Select")
	}
	if selected != node {
		t.Fatalf("mihomo Select got %q but rotate reported %q", selected, node)
	}
}

// TestRotateRefusesWhenAllPenalized checks that rotation reports an error
// instead of handing back a node known to be limited. Returning one would waste
// the caller's retry attempt on a request that cannot succeed.
//
// DIRECT is penalized too: it is the host's own egress and is a legitimate
// rotation target, so leaving it clean would make rotation succeed (correctly)
// and the assertion would never exercise the all-penalized path.
func TestRotateRefusesWhenAllPenalized(t *testing.T) {
	var selected string
	srv := newRotateStub(t, &selected)

	srv.penalizeNode("MOT", "limited")
	srv.penalizeNode("MOT", "working")
	srv.penalizeNode("MOT", "DIRECT")

	if _, _, err := srv.rotateMihomoNode(t.Context(), srv.mihomoClient(), "MOT"); err == nil {
		t.Fatal("rotation must fail when every node is rate limited")
	}
	if selected != "" {
		t.Fatalf("rotation selected %q despite every node being penalized", selected)
	}
}

// TestRotateFallsBackToDirectWhenProxiesPenalized documents the other half:
// DIRECT stays usable, so a fully limited proxy pool still has an exit.
func TestRotateFallsBackToDirectWhenProxiesPenalized(t *testing.T) {
	var selected string
	srv := newRotateStub(t, &selected)

	srv.penalizeNode("MOT", "limited")
	srv.penalizeNode("MOT", "working")

	_, node, err := srv.rotateMihomoNode(t.Context(), srv.mihomoClient(), "MOT")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if node != "DIRECT" {
		t.Fatalf("expected DIRECT, got %q", node)
	}
}

// TestNodeIPPenalizedSkipsSiblingNode covers nodes that share one exit IP: Zen
// limits the IP, so a sibling node is equally unusable.
func TestNodeIPPenalizedSkipsSiblingNode(t *testing.T) {
	s := &Server{}
	s.exitIPMu.Lock()
	s.exitIPCache = map[string]string{
		"node-a": "203.0.113.9",
		"node-b": "203.0.113.9",
		"node-c": "198.51.100.4",
	}
	s.exitIPAt = time.Now()
	s.exitIPMu.Unlock()

	avoid := map[string]bool{"node-a": true}
	if !s.nodeIPPenalized("MOT", "node-b", avoid) {
		t.Error("node-b shares node-a's exit IP and must be skipped too")
	}
	if s.nodeIPPenalized("MOT", "node-c", avoid) {
		t.Error("node-c has its own exit IP and must stay usable")
	}
}

// TestNodeExitIPsParsesStateFile checks the state reader against the exact
// format ipfarm writes.
func TestNodeExitIPsParsesStateFile(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.tsv")
	body := "name\tproto\thost\tport\texit_ip\tzen_ms\tok\tfail\tpenalized_until\n" +
		"p00-0001-412ms\thttp\t1.2.3.4\t8080\t203.0.113.7\t412\t0\t0\t0\n" +
		"p00-0002-620ms\tsocks5\t5.6.7.8\t1080\t\t620\t0\t0\t0\n"
	if err := os.WriteFile(state, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	st := openTestStore(t)
	if err := st.SetSetting(settingIpFarmState, state); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}

	got := s.nodeExitIPs()
	if got["p00-0001-412ms"] != "203.0.113.7" {
		t.Errorf("missing mapping for a node with an IP: %v", got)
	}
	if _, present := got["p00-0002-620ms"]; present {
		t.Errorf("a node without an exit IP must be omitted, got %v", got)
	}
}

// openTestStore creates an isolated store on a temporary directory.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newRotateStub builds a Server wired to a stub mihomo control API that reports
// the given nodes as healthy leaves under group MOT.
func newRotateStub(t *testing.T, selected *string) *Server {
	t.Helper()
	nodes := []string{"limited", "working"}
	base := startStubMihomo(t, selected, nodes)

	st := openTestStore(t)
	for k, v := range map[string]string{
		settingMihomoController: base,
		settingMihomoProxy:      "http://127.0.0.1:1",
		settingMihomoGroup:      "MOT",
		settingMihomoEnabled:    "1",
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	return &Server{store: st}
}

// startStubMihomo serves the subset of the mihomo control API that rotation
// touches: /proxies, /providers/proxies, and PUT /proxies/{group}. Every node
// is reported alive with a positive delay, which is what leafNodes requires.
func startStubMihomo(t *testing.T, selected *string, nodes []string) string {
	t.Helper()

	group := map[string]any{
		"name": "MOT",
		"type": "Selector",
		"now":  nodes[0],
		"all":  append(append([]string{}, nodes...), "DIRECT"),
	}
	proxies := map[string]any{"MOT": group, "DIRECT": map[string]any{"name": "DIRECT", "type": "Direct"}}
	for _, n := range nodes {
		proxies[n] = map[string]any{
			"name":  n,
			"type":  "Http",
			"alive": true,
			"history": []map[string]any{
				{"delay": 120},
			},
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"version": "stub"})
	})
	mux.HandleFunc("/proxies", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"proxies": proxies})
	})
	mux.HandleFunc("/providers/proxies", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"providers": map[string]any{}})
	})
	mux.HandleFunc("/proxies/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.HasPrefix(r.URL.Path, "/proxies/MOT") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if *selected != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		*selected = body.Name
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}
