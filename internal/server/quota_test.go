package server

import (
	"testing"

	"github.com/rian/mot-router/internal/mihomo"
)

// TestQuotaNoticeDetectsExhaustedSubscription is the regression guard for a
// misleading failure mode: when a subscription runs out, the provider replaces
// the node list with account-notice rows. Every request through them fails, and
// the dashboard reported only "proxy unreachable: ... EOF", which sends the
// operator looking for a network bug instead of renewing the plan.
//
// The names below are the exact rows a real exhausted subscription injected.
func TestQuotaNoticeDetectsExhaustedSubscription(t *testing.T) {
	all := map[string]mihomo.Group{
		"苏菲家宽": {
			Type: "Selector",
			Now:  "剩余流量：0",
			All: []string{
				"美国AT&T家宽-01套餐hy2",
				"剩余流量：0",
				"距离下次重置剩余：16 天",
				"套餐到期：2026-09-21",
			},
		},
		"DIRECT": {Type: "Direct"},
	}

	got := quotaNotice(all)
	if got == "" {
		t.Fatal("expected an exhausted subscription to be detected")
	}
	if !isNoticeName(got) {
		t.Errorf("quotaNotice returned %q, which is not a notice row", got)
	}
}

// TestQuotaNoticeIgnoresHealthyNodes ensures a working subscription is not
// misreported as exhausted, which would tell the operator to renew needlessly.
func TestQuotaNoticeIgnoresHealthyNodes(t *testing.T) {
	all := map[string]mihomo.Group{
		"苏菲家宽": {
			Type: "Selector",
			Now:  "香港01",
			All:  []string{"香港01", "日本02", "新加坡03", "DIRECT"},
		},
	}

	if got := quotaNotice(all); got != "" {
		t.Errorf("healthy nodes reported as a quota notice: %q", got)
	}
}

// TestIsNoticeName checks the individual markers.
func TestIsNoticeName(t *testing.T) {
	notices := []string{
		"剩余流量：0",
		"距离下次重置剩余：16 天",
		"套餐到期：2026-09-21",
		"苏菲家宽官网地址：sufe.us/ai.sufe.pro",
		"官网地址防失联发布页：sufe.uk",
		"流量重置时间：每月1日",
	}
	for _, n := range notices {
		if !isNoticeName(n) {
			t.Errorf("%q should be detected as a notice", n)
		}
	}

	realNodes := []string{
		"美国AT&T家宽-01套餐hy2",
		"【3x】中转|美国洛杉矶",
		"优秀|[3x]中转|台湾hinet家宽02",
		"香港01",
		"🇯🇵 日本节点",
		"WARP",
	}
	for _, n := range realNodes {
		if isNoticeName(n) {
			t.Errorf("%q is a real node but was detected as a notice", n)
		}
	}
}

// TestTotalNodesSumsGroups covers the fallback signal used when no notice row is
// present but every group is empty of healthy nodes.
func TestTotalNodesSumsGroups(t *testing.T) {
	st := mihomoStatus{
		Groups: []mihomoGroupView{
			{Name: "a", Nodes: 3},
			{Name: "b", Nodes: 4},
		},
	}
	if got := st.totalNodes(); got != 7 {
		t.Errorf("totalNodes() = %d, want 7", got)
	}
	if got := (mihomoStatus{}).totalNodes(); got != 0 {
		t.Errorf("empty status totalNodes() = %d, want 0", got)
	}
}
