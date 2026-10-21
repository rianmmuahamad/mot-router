// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureSnapshot(configs ...map[string]any) []byte {
	nodes := []any{}
	for _, c := range configs {
		nodes = append(nodes, map[string]any{"config": c})
	}
	b, _ := json.Marshal(map[string]any{"nodes": nodes})
	return b
}
func runtimeFixtureNode(b *runtimeBank) (string, json.RawMessage) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for name, n := range b.nodes {
		return name, append(json.RawMessage{}, n.Config...)
	}
	return "", nil
}

func TestFiveThousandCandidateInventoryAndUniqueHealth(t *testing.T) {
	b := newRuntimeBank()
	configs := []map[string]any{}
	for i := 0; i < maxRuntimeCandidates; i++ {
		configs = append(configs, map[string]any{"type": "http", "server": "127.0.0.1", "server_port": 10000 + i})
	}
	if err := b.load(fixtureSnapshot(configs...)); err != nil {
		t.Fatal(err)
	}
	if len(b.nodes) != 5000 {
		t.Fatalf("inventory truncated: %d", len(b.nodes))
	}
	tooMany := append(configs, map[string]any{"type": "http", "server": "127.0.0.1", "server_port": 65000})
	if err := b.load(fixtureSnapshot(tooMany...)); err == nil {
		t.Fatal("5001 candidates accepted")
	}
	if len(b.nodes) != 5000 {
		t.Fatal("rejected snapshot replaced previous inventory")
	}
	i := 0
	for name, node := range b.nodes {
		if i >= 3 {
			break
		}
		ip := "198.18.0.1"
		if i == 2 {
			ip = "198.18.0.2"
		}
		b.update(name, node.Config, result{Status: "healthy", ExitIP: ip, Latency: 10})
		i++
	}
	status := b.status()
	if status["candidate_cap"] != 5000 || status["unique_healthy_exit_ips"] != 2 || status["target_gap"] != 4998 {
		t.Fatalf("node count was mistaken for unique healthy IPs: %+v", status)
	}
}
func TestRuntimeInventoryAndSelection(t *testing.T) {
	b := newRuntimeBank()
	a := map[string]any{"type": "http", "server": "127.0.0.1", "server_port": 443, "password": "SECRET"}
	c := map[string]any{"type": "http", "server": "127.0.0.1", "server_port": 443, "password": "OTHER"}
	if err := b.load(fixtureSnapshot(a, a, c)); err != nil {
		t.Fatal(err)
	}
	if len(b.nodes) != 2 {
		t.Fatal("dedup ignored connection credentials")
	}
	name, config := runtimeFixtureNode(b)
	if b.selectNode(name) == nil {
		t.Fatal("unverified candidate was selectable")
	}
	b.update(name, config, result{Status: "healthy", Latency: 20, ExitIP: "203.0.113.7"})
	if b.selectNode(name) != nil {
		t.Fatal("verified candidate not selectable")
	}
	public, _ := json.Marshal(b.proxies())
	if strings.Contains(string(public), "SECRET") || strings.Contains(string(public), "OTHER") {
		t.Fatal("credentials leaked into controller")
	}
	if err := b.load([]byte(`{"nodes":[{"config":{}}]}`)); err == nil || len(b.nodes) != 2 {
		t.Fatal("bad snapshot discarded valid inventory")
	}
	if err := b.load(fixtureSnapshot(a, c)); err != nil {
		t.Fatal(err)
	}
	if b.current != name {
		t.Fatal("reload lost selection or health")
	}
	b.update(name, config, result{Status: "failed"})
	if b.current != "DIRECT" {
		t.Fatal("dead selected node not released")
	}
	b.update(name, config, result{Status: "healthy", Latency: 10, ExitIP: "203.0.113.7"})
	b.nodes[name].CheckedAt = time.Now().Add(-2 * time.Hour).Unix()
	if b.selectNode(name) == nil {
		t.Fatal("stale result allowed")
	}
	if b.proxies()[name].(runtimeNode).Alive {
		t.Fatal("stale node counted healthy")
	}
	many := []map[string]any{}
	for i := 0; i < maxRuntimeCandidates+1; i++ {
		many = append(many, a)
	}
	if b.load(fixtureSnapshot(many...)) == nil {
		t.Fatal("shortlist cap missing")
	}
}
func TestRuntimeCycleAndBaselineFailure(t *testing.T) {
	baselineOK := atomic.Bool{}
	baselineOK.Store(true)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !baselineOK.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/ip" {
			io.WriteString(w, "203.0.113.9")
		} else {
			w.WriteHeader(204)
		}
	}))
	defer target.Close()
	b := newRuntimeBank()
	b.pressure = func() bool { return false }
	b.healthURL = target.URL + "/health"
	b.exitURL = target.URL + "/ip"
	b.checkCA = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw}))
	configs := []map[string]any{}
	for i := 0; i < 12; i++ {
		configs = append(configs, map[string]any{"type": "http", "server": "127.0.0.1", "server_port": 1000 + i})
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	os.WriteFile(path, fixtureSnapshot(configs...), 0600)
	var active, peak, calls atomic.Int32
	b.check = func(r request) result {
		n := active.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		calls.Add(1)
		return result{Status: "healthy", ExitIP: "203.0.113.7", Latency: 8}
	}
	b.cycle(context.Background(), path, "")
	if calls.Load() != 12 || peak.Load() > 4 {
		t.Fatalf("unbounded or missing checks: %d peak %d", calls.Load(), peak.Load())
	}
	name, _ := runtimeFixtureNode(b)
	before := b.nodes[name].CheckedAt
	baselineOK.Store(false)
	b.cycle(context.Background(), path, "")
	if b.nodes[name].CheckedAt != before || !b.nodes[name].Alive || b.pause == "" {
		t.Fatal("baseline failure marked nodes dead")
	}
	baselineOK.Store(true)
	for _, node := range b.nodes {
		node.next = time.Time{}
	}
	b.check = func(request) result { baselineOK.Store(false); return result{Status: "failed"} }
	b.cycle(context.Background(), path, "")
	for _, node := range b.nodes {
		if !node.Alive {
			t.Fatal("mid-cycle endpoint outage marked nodes dead")
		}
	}
	// The paused scan does not fabricate current health; stale successes expire.
}
func TestRuntimeCONNECTStreamingAndDialLifetime(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(80 * time.Millisecond)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer target.Close()
	var proxyConnections atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyConnections.Add(1)
		if r.Method != "CONNECT" {
			http.Error(w, "CONNECT required", 400)
			return
		}
		conn, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "dial", 502)
			return
		}
		defer conn.Close()
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		done := make(chan struct{})
		go func() { io.Copy(conn, rw); conn.Close(); close(done) }()
		io.Copy(client, conn)
		client.Close()
		<-done
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	host, p, _ := net.SplitHostPort(u.Host)
	var port int
	fmt.Sscan(p, &port)
	b := newRuntimeBank()
	b.load(fixtureSnapshot(map[string]any{"type": "http", "server": host, "server_port": port}))
	name, config := runtimeFixtureNode(b)
	b.update(name, config, result{Status: "healthy", Latency: 1, ExitIP: "203.0.113.7"})
	b.selectNode(name)
	proxy := httptest.NewServer(http.HandlerFunc(b.proxy))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	// A concurrent probe changed the default, but this request must keep its
	// chosen, verified node instead of unexpectedly escaping via DIRECT.
	b.selectNode("DIRECT")
	tr := &http.Transport{Proxy: http.ProxyURL(pu), ProxyConnectHeader: http.Header{"X-Nodebank-Node": []string{name}}, TLSClientConfig: &tls.Config{RootCAs: roots}, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("stream broken: %s %v", body, err)
	}
	if proxyConnections.Load() != 1 {
		t.Fatal("concurrent selection changed the pinned request's exit")
	}
	b.selectNode(name)
	// Completing a short dial context must not terminate the established tunnel.
	tu, _ := url.Parse(target.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	conn, err := b.dial(ctx, tu.Host)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	defer conn.Close()
	secure := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: tu.Hostname()})
	secure.SetDeadline(time.Now().Add(2 * time.Second))
	if err = secure.Handshake(); err != nil {
		t.Fatal("dial cancellation killed tunnel:", err)
	}
	fmt.Fprintf(secure, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", tu.Host)
	r, err := http.ReadResponse(bufio.NewReader(secure), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(r.Body)
	r.Body.Close()
	secure.Close()
	if err != nil || !strings.Contains(string(body), "[DONE]") {
		t.Fatal("stream lifetime lost", err)
	}
	control := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/proxies/MOT", strings.NewReader(`{"name":"DIRECT"}`))
	req.Header.Set("Origin", "https://attacker.invalid")
	b.control(control, req)
	if control.Code != 403 {
		t.Fatal("cross-origin mutation allowed")
	}
	if l, err := localListen("0.0.0.0:0"); err == nil {
		l.Close()
		t.Fatal("public listener permitted")
	}
}

func TestRuntimeReportsTunnelPressure(t *testing.T) {
	b := newRuntimeBank()
	b.slots <- struct{}{}
	defer func() { <-b.slots }()
	s := b.status()
	if s["active_tunnels"] != 1 || s["tunnel_capacity"] != 64 {
		t.Fatalf("missing pressure metrics: %+v", s)
	}
}

func TestRuntimeSelectionCompareAndSwap(t *testing.T) {
	b := newRuntimeBank()
	b.load(fixtureSnapshot(map[string]any{"type": "http", "server": "127.0.0.1", "server_port": 1}))
	name, config := runtimeFixtureNode(b)
	b.update(name, config, result{Status: "healthy", ExitIP: "192.0.2.4", Latency: 1})
	expected := "DIRECT"
	if err := b.selectNodeExpected(name, &expected); err != nil {
		t.Fatal(err)
	}
	if err := b.selectNodeExpected("DIRECT", &expected); err == nil {
		t.Fatal("stale selection overwrote operator choice")
	}
	if b.current != name {
		t.Fatal("choice changed")
	}
}

func TestUpgradeRestoresOnlyFreshMatchingVPSHealth(t *testing.T) {
	configs := []map[string]any{{"type": "http", "server": "127.0.0.1", "server_port": 10}, {"type": "http", "server": "127.0.0.1", "server_port": 11}}
	old := newRuntimeBank()
	old.load(fixtureSnapshot(configs...))
	var selected, stale string
	for name, n := range old.nodes {
		old.update(name, n.Config, result{Status: "healthy", ExitIP: "192.0.2.3", Latency: 20})
		if selected == "" {
			selected = name
		} else {
			stale = name
		}
	}
	old.selectNode(selected)
	old.nodes[stale].CheckedAt = time.Now().Add(-2 * time.Hour).Unix()
	snapshot, _ := json.Marshal(map[string]any{"proxies": old.proxies()})
	resumed := newRuntimeBank()
	resumed.load(fixtureSnapshot(configs...))
	resumed.restoreHealth(snapshot)
	if resumed.current != selected || !resumed.nodes[selected].Alive || resumed.nodes[stale].Alive {
		t.Fatal("restart changed selection or trusted stale health")
	}
	changed := newRuntimeBank()
	changed.load(fixtureSnapshot(map[string]any{"type": "http", "server": "127.0.0.1", "server_port": 10, "password": "changed"}))
	changed.restoreHealth(snapshot)
	for _, n := range changed.nodes {
		if n.Alive {
			t.Fatal("changed credentials inherited old health")
		}
	}
}
