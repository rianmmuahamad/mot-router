package server

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/rian/mot-router/internal/mihomo"
	"github.com/rian/mot-router/internal/zen"
)

var errNoAlternativeExit = errors.New("tidak ada node sehat dengan IP keluar berbeda")

type nodeRotation struct {
	From, Node, PreviousIP string
}

func canonicalExitIP(value string) string {
	if ip := net.ParseIP(value); ip != nil {
		return ip.String()
	}
	return ""
}

func (s *Server) rotateMihomoNode(ctx context.Context, c *mihomo.Client, group string, failedNode ...string) (string, string, error) {
	rotation, err := s.rotateMihomoNodeResult(ctx, c, group, failedNode...)
	return rotation.PreviousIP, rotation.Node, err
}

func (s *Server) rotateMihomoNodeResult(ctx context.Context, c *mihomo.Client, group string, failedNode ...string) (nodeRotation, error) {
	mihomoMu.Lock()
	defer mihomoMu.Unlock()
	all, err := c.AllProxies(ctx)
	if err != nil {
		return nodeRotation{}, err
	}
	g, ok := all[group]
	if !ok {
		return nodeRotation{}, fmt.Errorf("group %q tidak ditemukan", group)
	}
	ipFor := func(name string) string {
		ip := canonicalExitIP(all[name].ExitIP)
		if ip == "" && !nodeBankRuntimeEnabled() {
			ip = canonicalExitIP(s.nodeExitIPs()[name])
		}
		return ip
	}
	rotation := nodeRotation{From: g.Now, PreviousIP: ipFor(g.Now)}
	// A different request already switched away from the failed node. Reuse
	// that selection instead of rotating its healthy replacement again.
	if len(failedNode) > 0 && failedNode[0] != "" && g.Now != failedNode[0] {
		rotation.Node = g.Now
		return rotation, nil
	}
	nodes := leafNodes(g, all)
	if len(nodes) == 0 {
		return rotation, fmt.Errorf("group %q tidak punya node sehat", group)
	}
	avoid := s.penalizedNodes(group)
	avoidIPs := map[string]bool{}
	for name := range avoid {
		if ip := ipFor(name); ip != "" {
			avoidIPs[ip] = true
		}
	}
	start := 0
	for i, name := range nodes {
		if name == g.Now {
			start = (i + 1) % len(nodes)
			break
		}
	}
	next := ""
	hasEligible := false
	for offset := range nodes {
		name := nodes[(start+offset)%len(nodes)]
		ip := ipFor(name)
		if avoid[name] || (ip != "" && avoidIPs[ip]) {
			continue
		}
		hasEligible = true
		if name == g.Now {
			continue
		}
		if rotation.PreviousIP != "" && ip == rotation.PreviousIP {
			continue
		}
		// Unknown exit IPs remain eligible, but the response cannot claim that
		// their address changed until both addresses have actually been observed.
		next = name
		break
	}
	if next == "" {
		if !hasEligible {
			return rotation, fmt.Errorf("tidak ada node sehat yang tersedia di group %q", group)
		}
		return rotation, errNoAlternativeExit
	}
	if nodeBankRuntimeEnabled() {
		err = c.SelectExpected(ctx, group, next, g.Now)
	} else {
		err = c.Select(ctx, group, next)
	}
	if err != nil {
		return rotation, err
	}
	c.ResetConnections()
	rotation.Node = next
	return rotation, nil
}

type initialStreamError struct{ detail string }

func (e *initialStreamError) Error() string { return e.detail }

func shouldRetryZen(status int, err error) bool {
	var streamError *zen.StreamError
	if errors.As(err, &streamError) {
		return streamError.Retryable && streamError.HTTPStatus() != 401 && streamError.HTTPStatus() != 403 && streamError.HTTPStatus() != 429
	}
	if err == nil {
		return false
	}
	if status == 401 || status == 403 || isRateLimited(status, err.Error()) {
		return false
	}
	var initial *initialStreamError
	return errors.As(err, &initial) || shouldRotate(status, err.Error())
}
