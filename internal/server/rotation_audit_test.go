package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rian/mot-router/internal/mihomo"
	"github.com/rian/mot-router/internal/zen"
)

// Regression coverage from the localhost audit; no real nodes are touched.
func TestRotationAuditRegressions(t *testing.T) {
	t.Setenv("MOT_NODEBANK_RUNTIME", "1")
	controller := func(t *testing.T, concurrentChange bool) (*mihomo.Client, func() string) {
		t.Helper()
		current := "node-a"
		var mu sync.Mutex
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.Method == "PUT":
				var body struct {
					Name     string  `json:"name"`
					Expected *string `json:"expected_now"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if body.Expected != nil && *body.Expected != current {
					http.Error(w, "selection changed during check", 409)
					return
				}
				current = body.Name
				w.WriteHeader(204)
			case r.URL.Path == "/proxies":
				all := map[string]mihomo.Group{
					"MOT":    {Name: "MOT", Type: "Selector", Now: current, All: []string{"node-a", "node-b", "node-c"}},
					"node-a": {Name: "node-a", Type: "http", Alive: true, ExitIP: "198.18.0.1", History: []mihomo.History{{Delay: 10}}},
					"node-b": {Name: "node-b", Type: "http", Alive: true, ExitIP: "198.18.0.1", History: []mihomo.History{{Delay: 20}}},
					"node-c": {Name: "node-c", Type: "http", Alive: true, ExitIP: "198.18.0.2", History: []mihomo.History{{Delay: 30}}},
				}
				json.NewEncoder(w).Encode(map[string]any{"proxies": all})
				if concurrentChange {
					current = "node-c" // another actor selected C after the snapshot
				}
			case r.URL.Path == "/providers/proxies":
				json.NewEncoder(w).Encode(map[string]any{"providers": map[string]any{}})
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(stub.Close)
		return mihomo.New(stub.URL, "http://127.0.0.1:1", ""), func() string { mu.Lock(); defer mu.Unlock(); return current }
	}
	t.Run("rotation_must_change_known_exit_IP", func(t *testing.T) {
		c, _ := controller(t, false)
		s := &Server{store: openTestStore(t)}
		_, next, err := s.rotateMihomoNode(t.Context(), c, "MOT")
		if err != nil || next != "node-c" {
			t.Fatalf("selected=%q err=%v: node-b shares node-a's exit IP despite a distinct healthy node-c", next, err)
		}
	})
	t.Run("previous_IP_must_not_be_node_name", func(t *testing.T) {
		c, _ := controller(t, false)
		s := &Server{store: openTestStore(t)}
		previousIP, _, err := s.rotateMihomoNode(t.Context(), c, "MOT")
		if err != nil || net.ParseIP(previousIP) == nil {
			t.Fatalf("previous_ip=%q err=%v: rotate endpoint compares a node name against an IP", previousIP, err)
		}
	})
	t.Run("stale_rotation_must_not_overwrite_another_selection", func(t *testing.T) {
		c, current := controller(t, true)
		s := &Server{store: openTestStore(t)}
		_, _, err := s.rotateMihomoNode(t.Context(), c, "MOT")
		if current() != "node-c" || err == nil {
			t.Fatalf("current=%q err=%v: stale snapshot overwrote the concurrent node-c selection", current(), err)
		}
	})
	t.Run("another_request_already_rotated_the_failed_node", func(t *testing.T) {
		c, current := controller(t, false)
		s := &Server{store: openTestStore(t)}
		_, next, err := s.rotateMihomoNode(t.Context(), c, "MOT", "previously-failed-node")
		if err != nil || next != "node-a" || current() != "node-a" {
			t.Fatalf("replacement was unnecessarily rotated: current=%s next=%s err=%v", current(), next, err)
		}
	})
	t.Run("closed_initial_stream_must_be_retryable_transport_failure", func(t *testing.T) {
		events := make(chan zen.Event)
		close(events)
		_, _, err := streamFirst(context.Background(), events, time.Millisecond)
		if err == nil || !shouldRetryZen(200, err) {
			t.Fatalf("initial stream failure does not trigger retry: %v", err)
		}
	})
	t.Run("silent_initial_stream_is_retryable_but_policy_denials_are_not", func(t *testing.T) {
		events := make(chan zen.Event)
		_, _, err := streamFirst(context.Background(), events, time.Millisecond)
		if err == nil || !shouldRetryZen(200, err) {
			t.Fatal("silent initial stream was not retryable")
		}
		for _, status := range []int{401, 403, 429} {
			if shouldRetryZen(status, err) {
				t.Fatalf("policy denial became retryable: %d", status)
			}
		}
	})
}
