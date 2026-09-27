// Package mihomo talks to a running mihomo/Clash instance.
//
// mihomo exposes an HTTP proxy (mixed-port) for traffic and a separate control
// API (external-controller) for inspection and reconfiguration. The control API
// is what makes IP rotation possible: switching the selected node in a Selector
// group changes the egress IP, which is how a per-IP rate limit is escaped.
//
// This is fundamentally different from a Cloudflare Worker relay, whose egress
// IPs are shared across all free users and therefore already saturated. A
// subscription-backed mihomo instance has many nodes with distinct, mostly
// unshared IPs.
package mihomo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Client talks to a mihomo control API.
type Client struct {
	// transportOnce guards lazy construction of the shared proxy transport.
	transportOnce sync.Once
	// proxyTransport is the pooled transport for all proxied requests.
	proxyTransport *http.Transport

	// ControllerURL is the external-controller, e.g. http://127.0.0.1:9090
	ControllerURL string
	// ProxyURL is the mixed-port HTTP proxy, e.g. http://127.0.0.1:7890
	ProxyURL string
	// Secret is the control API bearer token, when configured.
	Secret string
	HTTP   *http.Client
}

// New builds a client with the conventional defaults.
func New(controllerURL, proxyURL, secret string) *Client {
	if controllerURL == "" {
		controllerURL = "http://127.0.0.1:9090"
	}
	if proxyURL == "" {
		proxyURL = "http://127.0.0.1:7890"
	}
	return &Client{
		ControllerURL: strings.TrimRight(controllerURL, "/"),
		ProxyURL:      strings.TrimRight(proxyURL, "/"),
		Secret:        secret,
		HTTP:          &http.Client{Timeout: 20 * time.Second},
	}
}

// Group is a proxy group that can have its selected node changed.
type Group struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Now  string   `json:"now"`
	All  []string `json:"all"`
	// Alive and History describe the node's health. A subscription often injects
	// pseudo-entries carrying account notices ("traffic remaining: 0", "expires
	// on …") that look like real proxies but always fail; they report
	// alive=false with a zero delay.
	Alive   bool      `json:"alive"`
	History []History `json:"history"`
}

// History is one health-check result.
type History struct {
	Time  string `json:"time"`
	Delay int    `json:"delay"`
}

// Delay returns the most recent measured latency in milliseconds, or 0 when
// unknown.
func (g Group) Delay() int {
	if len(g.History) == 0 {
		return 0
	}
	return g.History[len(g.History)-1].Delay
}

// Healthy reports whether the entry is a usable upstream node.
//
// An entry with a recorded history but a zero delay is one of the subscription's
// notice rows rather than a real server.
func (g Group) Healthy() bool {
	if !g.Alive {
		return false
	}
	if len(g.History) > 0 && g.Delay() <= 0 {
		return false
	}
	return true
}

// Selectable reports whether the group's selection can be changed via the API.
// Only Selector groups are directly settable; URLTest and Fallback groups choose
// for themselves.
func (g Group) Selectable() bool {
	return g.Type == "Selector"
}

// Nodes lists the real upstream nodes in a group, excluding the synthetic
// DIRECT/REJECT entries.
func (g Group) Nodes() []string {
	out := make([]string, 0, len(g.All))
	for _, n := range g.All {
		if n == "DIRECT" || n == "REJECT" || n == "GLOBAL" {
			continue
		}
		out = append(out, n)
	}
	return out
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encode request: %w", err)
		}
		reader = strings.NewReader(string(payload))
	}

	req, err := http.NewRequestWithContext(ctx, method, c.ControllerURL+path, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("control API unreachable: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return out, resp.StatusCode, nil
}

// Groups returns every proxy group, keyed by name.
func (c *Client) Groups(ctx context.Context) (map[string]Group, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/proxies", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("control API returned %d", status)
	}

	var payload struct {
		Proxies map[string]Group `json:"proxies"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode proxies: %w", err)
	}
	return payload.Proxies, nil
}

// AllProxies returns every proxy group from /proxies merged with all provider/proxies from /providers/proxies, keyed by name with health and latency data.
func (c *Client) AllProxies(ctx context.Context) (map[string]Group, error) {
	base, err := c.Groups(ctx)
	if err != nil {
		return nil, err
	}
	body, status, err := c.do(ctx, http.MethodGet, "/providers/proxies", nil)
	if err != nil || status != http.StatusOK {
		return base, nil
	}
	var payload struct {
		Providers map[string]struct {
			Proxies []Group `json:"proxies"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && len(payload.Providers) > 0 {
		for _, prov := range payload.Providers {
			for _, p := range prov.Proxies {
				if p.Name != "" {
					base[p.Name] = p
				}
			}
		}
	}
	return base, nil
}

// SelectorGroups returns the settable groups, sorted by name for stable output.
func (c *Client) SelectorGroups(ctx context.Context) ([]Group, error) {
	all, err := c.Groups(ctx)
	if err != nil {
		return nil, err
	}
	out := []Group{}
	for name, g := range all {
		if name == "GLOBAL" || !g.Selectable() {
			continue
		}
		if len(g.Nodes()) == 0 {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Select switches the node used by a Selector group.
func (c *Client) Select(ctx context.Context, group, node string) error {
	body, status, err := c.do(ctx, http.MethodPut,
		"/proxies/"+url.PathEscape(group), map[string]string{"name": node})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("selecting %q in %q returned %d: %s",
			node, group, status, strings.TrimSpace(string(body)))
	}
	return nil
}

// Current returns the node a group currently uses.
func (c *Client) Current(ctx context.Context, group string) (string, error) {
	all, err := c.Groups(ctx)
	if err != nil {
		return "", err
	}
	g, ok := all[group]
	if !ok {
		return "", fmt.Errorf("group %q not found", group)
	}
	return g.Now, nil
}

// Version reports the running mihomo version, used as a health check.
func (c *Client) Version(ctx context.Context) (string, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("control API returned %d", status)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode version: %w", err)
	}
	return payload.Version, nil
}

// egressProbes are the services used to discover the exit IP.
//
// More than one is tried because a single blocked or flaky endpoint would
// otherwise look like a dead proxy. api.ipify.org in particular is blocked by
// some exit nodes, which produced a misleading "proxy unreachable" error even
// while every real request through the same proxy succeeded.
var egressProbes = []string{
	"https://api.ipify.org",
	"https://icanhazip.com",
	"https://ifconfig.me/ip",
}

// EgressIP asks an external service which IP the proxy currently presents.
//
// It goes through the proxy rather than the control API, because only the
// proxy path reflects the node actually carrying traffic.
//
// The transport is cloned from the process default so the connection-phase
// timeouts are inherited. A bare &http.Transport{} has no dial or TLS handshake
// timeout, so a node that accepts the connection and then stalls would hang
// here instead of failing fast.
func (c *Client) EgressIP(ctx context.Context) (string, error) {
	proxyURL, err := url.Parse(c.ProxyURL)
	if err != nil {
		return "", fmt.Errorf("bad proxy url %q: %w", c.ProxyURL, err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	transport.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	defer transport.CloseIdleConnections()

	var firstErr error
	for _, probe := range egressProbes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "mot-router/1.0")

		resp, err := client.Do(req)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s returned %d", probe, resp.StatusCode)
			}
			continue
		}
		if ip := strings.TrimSpace(string(body)); ip != "" {
			return ip, nil
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no probe answered")
	}
	// Report which probe failed first so the operator has something to act on,
	// while making clear that every probe was tried.
	return "", fmt.Errorf("proxy tidak bisa dipakai: %w", firstErr)
}

// ProxyClient returns an HTTP client that routes through the mihomo proxy.
//
// The transport is built once and reused. Building one per request leaks a
// connection pool each time: idle connections are only reclaimed after
// IdleConnTimeout, so a busy router exhausts the proxy's connection table and
// starts seeing "unexpected EOF" on new requests.
//
// No timeout is set on the client: model responses stream for as long as the
// upstream needs, and a client-level timeout would abort a long generation.
func (c *Client) ProxyClient() *http.Client {
	c.transportOnce.Do(func() {
		proxyURL, err := url.Parse(c.ProxyURL)
		if err != nil {
			c.proxyTransport = nil
			return
		}
		// Start from the process default so the connection-phase timeouts are
		// inherited. A bare &http.Transport{} has a zero DialContext timeout and
		// a zero TLSHandshakeTimeout, which means "no limit": an exit node that
		// accepts the TCP connection and then stalls during the TLS handshake
		// would hold the request open until the caller's own deadline. Measured
		// as a 65s failure with status 0 ("EOF") before this was fixed.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = http.ProxyURL(proxyURL)
		tr.DialContext = (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		tr.TLSHandshakeTimeout = 15 * time.Second
		// Keep-alive is deliberately disabled.
		//
		// A pooled connection to the local proxy terminates at whichever exit
		// node was active when it was opened. When mihomo switches node, or
		// the exit drops the idle connection, reusing it fails with
		// "unexpected EOF" — measured as 7 of 8 requests failing with reuse
		// versus 6 of 6 succeeding without. The handshake is to localhost, so
		// the cost of a fresh connection is negligible.
		tr.DisableKeepAlives = true
		// A proxied request can stall behind a dead exit node. The SSE headers
		// arrive promptly even for long generations, so a bounded header wait
		// catches a bad node without truncating a slow reply.
		//
		// Kept well under the caller's retry budget: a node that never answers
		// must fail fast enough to leave room for the remaining attempts.
		// Measured at 45s, one stalled node consumed the whole budget.
		tr.ResponseHeaderTimeout = 20 * time.Second
		tr.ExpectContinueTimeout = 1 * time.Second
		c.proxyTransport = tr
	})
	return &http.Client{Transport: c.proxyTransport}
}

// ResetConnections drops pooled connections.
//
// Must be called after switching nodes: pooled connections terminate at the
// previous exit node, and reusing them fails with "unexpected EOF" or a TLS
// error. Without this, the first request after every rotation fails.
func (c *Client) ResetConnections() {
	if c.proxyTransport != nil {
		c.proxyTransport.CloseIdleConnections()
	}
}
