package server

import (
	"testing"

	"github.com/rian/mot-router/internal/mihomo"
)

// TestLeafNodesKeepsDirect is the regression guard for the bug that broke
// rotation on the VPS deployment.
//
// leafNodes excluded DIRECT as "not an upstream server". But a group may
// legitimately route through the host's own connection — the VPS does exactly
// that, because the VPS IP works where WARP is rate limited. Excluding DIRECT
// reduced a two-node group (DIRECT + WARP) to just WARP, so rotation kept
// selecting the same limited node. Observed in the router log as:
//
//	mihomo rotate: from="WARP" to="WARP"
//
// Effect: 12 consecutive 429s with no way out.
func TestLeafNodesKeepsDirect(t *testing.T) {
	all := map[string]mihomo.Group{
		"MOT": {
			Name: "MOT",
			Type: "Selector",
			Now:  "WARP",
			All:  []string{"DIRECT", "WARP"},
		},
		"WARP": {
			Name:    "WARP",
			Type:    "Http",
			Alive:   true,
			History: []mihomo.History{{Delay: 120}},
		},
	}

	got := leafNodes(all["MOT"], all)

	var hasDirect, hasWarp bool
	for _, n := range got {
		switch n {
		case "DIRECT":
			hasDirect = true
		case "WARP":
			hasWarp = true
		}
	}

	if !hasDirect {
		t.Errorf("DIRECT must be a usable exit, got %v", got)
	}
	if !hasWarp {
		t.Errorf("WARP should still be listed, got %v", got)
	}
	if len(got) != 2 {
		t.Errorf("expected both exits, got %v", got)
	}
}

// TestLeafNodesStillExcludesReject keeps the filter narrow: REJECT is never a
// usable exit, and group references are decided by their own group.
func TestLeafNodesStillExcludesReject(t *testing.T) {
	all := map[string]mihomo.Group{
		"MOT": {
			Name: "MOT",
			Type: "Selector",
			Now:  "香港节点",
			All:  []string{"REJECT", "香港节点", "DIRECT"},
		},
		"香港节点": {
			Name: "香港节点",
			Type: "Selector",
			All:  []string{"h1", "h2"},
		},
		"h1": {Name: "h1", Type: "Vless", Alive: true, History: []mihomo.History{{Delay: 40}}},
	}

	got := leafNodes(all["MOT"], all)

	for _, n := range got {
		if n == "REJECT" {
			t.Error("REJECT must never be selected")
		}
		if n == "香港节点" {
			t.Error("a group reference must not be selected directly")
		}
	}
	if len(got) != 1 || got[0] != "DIRECT" {
		t.Errorf("expected only DIRECT, got %v", got)
	}
}

// TestLeafNodesSkipsUnhealthyNodes keeps the health filter intact.
func TestLeafNodesSkipsUnhealthyNodes(t *testing.T) {
	all := map[string]mihomo.Group{
		"MOT": {
			Name: "MOT", Type: "Selector", Now: "good",
			All: []string{"good", "dead", "noDelay", "notice"},
		},
		"good":    {Name: "good", Type: "Vless", Alive: true, History: []mihomo.History{{Delay: 50}}},
		"dead":    {Name: "dead", Type: "Vless", Alive: false},
		"noDelay": {Name: "noDelay", Type: "Vless", Alive: true, History: []mihomo.History{{Delay: 0}}},
		"notice":  {Name: "notice", Type: "Vless", Alive: true, History: []mihomo.History{{Delay: 0}}},
	}

	got := leafNodes(all["MOT"], all)
	if len(got) != 1 || got[0] != "good" {
		t.Errorf("expected only the healthy node, got %v", got)
	}
}
