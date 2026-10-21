package server

import (
	"testing"
	"time"
)

// TestNodePenaltySkipsRejectedNode is the regression guard for a real failure
// observed on the VPS.
//
// The group had two nodes: DIRECT worked (6/6 requests) and WARP was rate
// limited (4/4 requests rejected). mihomo reported both as healthy because its
// health check only proves the tunnel carries traffic — it cannot know that Zen
// rejects that particular exit. With rotation on, the router bounced onto WARP
// and produced 12 consecutive 429s.
func TestNodePenaltySkipsRejectedNode(t *testing.T) {
	s := &Server{}

	if got := s.penalizedNodes("MOT"); len(got) != 0 {
		t.Fatalf("a fresh server must have no penalties, got %v", got)
	}

	s.penalizeNode("MOT", "WARP")

	got := s.penalizedNodes("MOT")
	if !got["WARP"] {
		t.Fatal("WARP should be penalized after a rate limit")
	}
	if got["DIRECT"] {
		t.Error("DIRECT was never penalized and must stay usable")
	}

	// A different group is unaffected.
	if other := s.penalizedNodes("OTHER"); len(other) != 0 {
		t.Errorf("penalties must be scoped per group, got %v", other)
	}

	// A success must NOT clear the penalty: Zen's limit is a multi-hour budget,
	// so an IP that just answered can still be nearly exhausted. Only time
	// expires it (covered by TestNodePenaltyExpires).
	if got := s.penalizedNodes("MOT"); !got["WARP"] {
		t.Error("a penalty must survive until its TTL elapses")
	}
}

// TestNodePenaltyExpires checks the penalty does not last forever, so a node
// that recovers becomes usable again without restarting the router.
func TestNodePenaltyExpires(t *testing.T) {
	s := &Server{}
	s.penalizeNode("MOT", "WARP")

	// Backdate the entry past its TTL.
	s.nodeMu.Lock()
	s.nodePenalty["MOT|WARP"] = time.Now().Add(-nodePenaltyTTL - time.Minute)
	s.nodeMu.Unlock()

	if got := s.penalizedNodes("MOT"); len(got) != 0 {
		t.Errorf("an expired penalty must be forgotten, got %v", got)
	}
}

// TestNodePenaltyIgnoresEmptyName keeps a missing node name from creating a
// meaningless entry that would match no real node.
func TestNodePenaltyIgnoresEmptyName(t *testing.T) {
	s := &Server{}
	s.penalizeNode("MOT", "")
	if got := s.penalizedNodes("MOT"); len(got) != 0 {
		t.Errorf("an empty node name must not be recorded, got %v", got)
	}
}
