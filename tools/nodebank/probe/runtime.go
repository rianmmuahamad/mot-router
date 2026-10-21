// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"golang.org/x/net/netutil"
)

// Only the bounded candidate shortlist is resident. Adapters live for one tunnel.
type runtimeNode struct {
	Config    json.RawMessage  `json:"-"`
	Name      string           `json:"name"`
	Type      string           `json:"type"`
	Alive     bool             `json:"alive"`
	ExitIP    string           `json:"exit_ip,omitempty"`
	CheckedAt int64            `json:"checked_at,omitempty"`
	Status    string           `json:"status"`
	History   []runtimeHistory `json:"history"`
	next      time.Time
	failures  int
}

const maxRuntimeCandidates = 5000

type runtimeHistory struct {
	Time  string `json:"time"`
	Delay int    `json:"delay"`
}
type runtimeBank struct {
	mu                          sync.RWMutex
	nodes                       map[string]*runtimeNode
	current                     string
	pause                       string
	sourceError                 string
	connections                 map[net.Conn]struct{}
	slots                       chan struct{}
	check                       func(request) result
	pressure                    func() bool
	healthURL, exitURL, checkCA string
	expected                    int
	directIP                    string
}

func newRuntimeBank() *runtimeBank {
	return &runtimeBank{nodes: map[string]*runtimeNode{}, current: "DIRECT", connections: map[net.Conn]struct{}{}, slots: make(chan struct{}, 64), check: probe, pressure: runtimeMemoryPressure, expected: 204}
}
func (b *runtimeBank) load(raw []byte) error {
	var snapshot struct {
		Nodes []struct {
			Config json.RawMessage `json:"config"`
		} `json:"nodes"`
	}
	if len(raw) > 32<<20 || json.Unmarshal(raw, &snapshot) != nil || snapshot.Nodes == nil || len(snapshot.Nodes) > maxRuntimeCandidates {
		return errors.New("invalid bounded candidate snapshot")
	}
	next := map[string]*runtimeNode{}
	for _, n := range snapshot.Nodes {
		var config map[string]any
		if len(n.Config) > maxFrame || json.Unmarshal(n.Config, &config) != nil {
			return errors.New("invalid candidate configuration")
		}
		kind, _ := config["type"].(string)
		server, _ := config["server"].(string)
		port, _ := config["server_port"].(float64)
		if server == "" || port < 1 || port > 65535 || port != float64(int(port)) {
			return errors.New("invalid candidate endpoint")
		}
		canonical, _ := json.Marshal(config)
		sum := sha256.Sum256(canonical)
		name := fmt.Sprintf("NB-%x", sum[:12])
		next[name] = &runtimeNode{Config: canonical, Name: name, Type: kind, Status: "pending", History: []runtimeHistory{}}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for name := range next {
		if old := b.nodes[name]; old != nil {
			next[name] = old
		}
	}
	b.nodes = next
	if b.current != "DIRECT" {
		if n := next[b.current]; n == nil || !n.Alive || time.Since(time.Unix(n.CheckedAt, 0)) > time.Hour {
			b.current = "DIRECT"
		}
	}
	b.sourceError = ""
	return nil
}
func (b *runtimeBank) proxies() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := []string{"DIRECT"}
	out := map[string]any{"DIRECT": map[string]any{"name": "DIRECT", "type": "Direct", "alive": true, "exit_ip": b.directIP, "history": []runtimeHistory{}}}
	for name, n := range b.nodes {
		copy := *n
		if time.Since(time.Unix(n.CheckedAt, 0)) > time.Hour {
			copy.Alive = false
		}
		copy.History = append([]runtimeHistory{}, n.History...)
		out[name] = copy
		names = append(names, name)
	}
	sort.Strings(names[1:])
	out["MOT"] = map[string]any{"name": "MOT", "type": "Selector", "now": b.current, "all": names, "alive": true}
	return out
}
func (b *runtimeBank) selectNode(name string) error { return b.selectNodeExpected(name, nil) }
func (b *runtimeBank) selectNodeExpected(name string, expected *string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if expected != nil && b.current != *expected {
		return errors.New("selection changed during check")
	}
	if name != "DIRECT" {
		n := b.nodes[name]
		if n == nil || !n.Alive || time.Since(time.Unix(n.CheckedAt, 0)) > time.Hour {
			return errors.New("node has not passed a recent VPS check")
		}
	}
	b.current = name
	return nil
}
func (b *runtimeBank) update(name string, config json.RawMessage, res result) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.nodes[name]
	if n == nil || string(n.Config) != string(config) {
		return
	}
	n.Status = res.Status
	n.Alive = res.Status == "healthy"
	n.CheckedAt = time.Now().Unix()
	delay := 0
	if n.Alive {
		delay = max(1, int(res.Latency))
		n.ExitIP = res.ExitIP
		n.failures = 0
		n.next = time.Now().Add(30 * time.Minute)
	} else {
		n.failures++
		n.next = time.Now().Add(min(6*time.Hour, time.Minute*time.Duration(1<<min(n.failures, 8))))
	}
	n.History = append(n.History, runtimeHistory{time.Now().UTC().Format(time.RFC3339), delay})
	if len(n.History) > 8 {
		n.History = n.History[len(n.History)-8:]
	}
	if b.current == name && !n.Alive {
		b.current = "DIRECT"
	}
}
func (b *runtimeBank) status() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	counts := map[string]int{"healthy": 0, "pending": 0, "failed": 0, "unsupported": 0}
	exits := map[string]struct{}{}
	for _, n := range b.nodes {
		status := n.Status
		if status == "healthy" && time.Since(time.Unix(n.CheckedAt, 0)) > time.Hour {
			status = "pending"
		}
		counts[status]++
		if status == "healthy" {
			if ip := net.ParseIP(n.ExitIP); ip != nil {
				exits[ip.String()] = struct{}{}
			}
		}
	}
	return map[string]any{"engine": "nodebank-native", "group": "MOT", "current": b.current, "total": len(b.nodes), "candidate_cap": maxRuntimeCandidates, "unique_healthy_exit_ips": len(exits), "target_gap": maxRuntimeCandidates - len(exits), "counts": counts, "pause_reason": b.pause, "source_error": b.sourceError, "active_connections": len(b.connections), "active_tunnels": len(b.slots), "tunnel_capacity": cap(b.slots), "selection_cas": true, "zen_verified": false, "vantage": "vps"}
}
func (b *runtimeBank) control(w http.ResponseWriter, r *http.Request) {
	// Loopback listeners plus browser-origin rejection protect mutation endpoints.
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "forbidden", 403)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case r.Method == "GET" && r.URL.Path == "/version":
		json.NewEncoder(w).Encode(map[string]string{"version": "nodebank-native/1"})
	case r.Method == "GET" && r.URL.Path == "/status":
		json.NewEncoder(w).Encode(b.status())
	case r.Method == "GET" && r.URL.Path == "/providers/proxies":
		json.NewEncoder(w).Encode(map[string]any{"providers": map[string]any{}})
	case r.Method == "GET" && r.URL.Path == "/proxies":
		json.NewEncoder(w).Encode(map[string]any{"proxies": b.proxies()})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/proxies/"):
		p := b.proxies()[strings.TrimPrefix(r.URL.Path, "/proxies/")]
		if p == nil {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(p)
	case r.Method == "PUT" && r.URL.Path == "/proxies/MOT":
		var v struct {
			Name     string  `json:"name"`
			Expected *string `json:"expected_now,omitempty"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&v) != nil {
			http.Error(w, "invalid selection", 400)
			return
		}
		if err := b.selectNodeExpected(v.Name, v.Expected); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

type runtimeConn struct {
	net.Conn
	once    sync.Once
	cleanup func()
}

func (c *runtimeConn) Close() error { err := c.Conn.Close(); c.once.Do(c.cleanup); return err }

func (b *runtimeBank) dial(ctx context.Context, address string) (conn net.Conn, err error) {
	b.mu.RLock()
	name := b.current
	b.mu.RUnlock()
	return b.dialNode(ctx, address, name)
}

func (b *runtimeBank) dialNode(ctx context.Context, address, name string) (conn net.Conn, err error) {
	b.mu.RLock()
	var config json.RawMessage
	if name != "DIRECT" {
		n := b.nodes[name]
		if n != nil && n.Alive && time.Since(time.Unix(n.CheckedAt, 0)) <= time.Hour {
			config = append(json.RawMessage{}, n.Config...)
		}
	}
	b.mu.RUnlock()
	if name == "DIRECT" {
		return (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", address)
	}
	if config == nil {
		return nil, errors.New("selected node is no longer healthy")
	}
	// Adapter scope outlives the dial deadline and lasts until the tunnel closes.
	life, cancel := context.WithCancel(context.Background())
	manager := &probeConnections{ctx: life}
	life = service.ContextWith[adapter.ConnectionManager](life, manager)
	scope := adapter.NewScope(life, log.NewNOPFactory().NewLogger("runtime"))
	var ob adapter.Outbound
	cleanup := func() {
		cancel()
		manager.CloseAll()
		if keeper, ok := ob.(adapter.IdleConnectionKeeper); ok {
			keeper.CloseIdleConnections()
		}
		scope.Close()
	}
	defer func() {
		if recover() != nil {
			err = errors.New("adapter configuration unsupported")
		}
		if err != nil {
			cleanup()
		}
	}()
	var endpoint struct {
		Server string `json:"server"`
	}
	_ = json.Unmarshal(config, &endpoint)
	resolved := ""
	if net.ParseIP(endpoint.Server) == nil {
		ips, dnsErr := net.DefaultResolver.LookupIPAddr(ctx, endpoint.Server)
		if dnsErr != nil || len(ips) == 0 {
			err = errors.New("proxy DNS failed")
			return nil, err
		}
		resolved = ips[0].IP.String()
	}
	// Construction uses the tunnel lifetime, so the 15s dial deadline cannot
	// truncate a long generation. DNS and the actual handshake remain bounded.
	ob, err = newOutbound(life, config, resolved)
	if err != nil {
		return nil, errors.New("proxy adapter initialization failed")
	}
	if starter, ok := ob.(interface {
		Start(adapter.StartStage, *adapter.Scope) error
	}); ok {
		if err = starter.Start(adapter.StartStateInitialize, scope); err != nil {
			return nil, errors.New("proxy adapter initialization failed")
		}
	}
	// Attach the lifetime manager to the bounded dial context. Closing the manager
	// releases raw sockets even when a transport handshake fails before returning.
	dialCtx := service.ContextWith[adapter.ConnectionManager](ctx, manager)
	conn, err = ob.DialContext(dialCtx, "tcp", M.ParseSocksaddr(address))
	if err != nil {
		return nil, errors.New("proxy tunnel failed")
	}
	return &runtimeConn{Conn: conn, cleanup: cleanup}, nil
}
func (b *runtimeBank) track(c net.Conn)   { b.mu.Lock(); b.connections[c] = struct{}{}; b.mu.Unlock() }
func (b *runtimeBank) untrack(c net.Conn) { b.mu.Lock(); delete(b.connections, c); b.mu.Unlock() }
func (b *runtimeBank) closeConnections() {
	b.mu.RLock()
	cs := make([]net.Conn, 0, len(b.connections))
	for c := range b.connections {
		cs = append(cs, c)
	}
	b.mu.RUnlock()
	for _, c := range cs {
		c.Close()
	}
}
func (b *runtimeBank) proxy(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "forbidden", 403)
		return
	}
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	default:
		http.Error(w, "proxy capacity reached", 503)
		return
	}
	// HTTPS inference travels as an opaque CONNECT tunnel; no AI payload is parsed.
	if r.Method != http.MethodConnect {
		http.Error(w, "HTTPS CONNECT required", 405)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || host == "" || portNumber < 1 || portNumber > 65535 {
		http.Error(w, "invalid CONNECT target", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var up net.Conn
	if name := r.Header.Get("X-Nodebank-Node"); name != "" {
		if len(name) > 128 {
			http.Error(w, "invalid node", 400)
			return
		}
		up, err = b.dialNode(ctx, r.Host, name)
	} else {
		up, err = b.dial(ctx, r.Host)
	}
	if err != nil {
		http.Error(w, "upstream tunnel failed", 502)
		return
	}
	defer up.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unavailable", 500)
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	b.track(client)
	defer b.untrack(client)
	b.track(up)
	defer b.untrack(up)
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if rw.Flush() != nil {
		return
	}
	// Do not discard client bytes already buffered after CONNECT headers.
	done := make(chan struct{})
	go func() { io.Copy(up, rw); up.Close(); close(done) }()
	io.Copy(client, up)
	client.Close()
	up.Close()
	<-done
}
func localListen(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("runtime listeners must use a loopback IP")
	}
	return net.Listen("tcp", address)
}
func readCandidates(ctx context.Context, path, bankURL string) ([]byte, error) {
	if bankURL == "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, 32<<20+1))
	}
	u, err := url.Parse(bankURL)
	if err != nil || u.Scheme != "http" || u.User != nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, errors.New("bank API must be loopback HTTP")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(bankURL, "/")+"/api/runtime/candidates", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-NodeBank-Runtime", "1")
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("bank candidates API unavailable")
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
}
func (b *runtimeBank) cycle(ctx context.Context, path, bankURL string) {
	raw, err := readCandidates(ctx, path, bankURL)
	if err == nil {
		err = b.load(raw)
	}
	if err != nil {
		b.mu.Lock()
		b.sourceError = "candidate source unavailable or invalid; retained previous shortlist"
		b.mu.Unlock()
	}
	// Verify baseline before changing node health. A broken laptop/VPS connection
	// or probe endpoint must not mark the entire bank dead.
	var roots *x509.CertPool
	if b.checkCA != "" {
		roots, _ = x509.SystemCertPool()
		if roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM([]byte(b.checkCA)) {
			b.mu.Lock()
			b.pause = "Invalid check CA"
			b.mu.Unlock()
			return
		}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	health := b.healthURL
	if health == "" {
		health = "https://www.gstatic.com/generate_204"
	}
	exit := b.exitURL
	if exit == "" {
		exit = "https://api.ipify.org"
	}
	_, err = getSmall(ctx, client, health, b.expected)
	var ipBody []byte
	if err == nil {
		ipBody, err = getSmall(ctx, client, exit, 200)
		if err == nil && exitIP(ipBody) == "" {
			err = errors.New("invalid baseline IP")
		}
	}
	if err != nil {
		b.mu.Lock()
		b.pause = "VPS connection or HTTPS check endpoint unavailable"
		b.mu.Unlock()
		return
	}
	b.mu.Lock()
	b.pause = ""
	b.directIP = exitIP(ipBody)
	jobs := []runtimeNode{}
	for _, n := range b.nodes {
		if time.Now().After(n.next) {
			jobs = append(jobs, *n)
		}
	}
	b.mu.Unlock()
	// Fixed four workers and a bounded shortlist, never one task per bank node.
	queue := make(chan runtimeNode, 4)
	var wg sync.WaitGroup
	var baselineDown atomic.Bool
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range queue {
				if ctx.Err() != nil {
					return
				}
				if baselineDown.Load() {
					continue
				}
				if b.pressure() {
					b.mu.Lock()
					b.pause = "VPS memory pressure; checks paused"
					b.mu.Unlock()
					continue
				}
				res := b.check(request{ID: n.Name, Config: n.Config, TimeoutMS: 15000, HealthURL: health, ExitURL: exit, ExpectedStatus: b.expected, CheckCAPem: b.checkCA})
				if res.Status == "failed" {
					_, baselineErr := getSmall(ctx, client, health, b.expected)
					if baselineErr == nil {
						_, baselineErr = getSmall(ctx, client, exit, 200)
					}
					if baselineErr != nil {
						baselineDown.Store(true)
						b.mu.Lock()
						b.pause = "VPS connection or HTTPS check endpoint unavailable"
						b.mu.Unlock()
						continue
					}
				}
				b.update(n.Name, n.Config, res)
			}
		}()
	}
loop:
	for _, n := range jobs {
		select {
		case queue <- n:
		case <-ctx.Done():
			break loop
		}
	}
	close(queue)
	wg.Wait()
}

// restoreHealth consumes a private, bounded controller snapshot from this VPS.
// It never replaces configurations or turns stale/unhealthy entries healthy.
func (b *runtimeBank) restoreHealth(raw []byte) {
	var state struct {
		Proxies map[string]struct {
			Alive     bool             `json:"alive"`
			Status    string           `json:"status"`
			CheckedAt int64            `json:"checked_at"`
			ExitIP    string           `json:"exit_ip"`
			Now       string           `json:"now"`
			History   []runtimeHistory `json:"history"`
		} `json:"proxies"`
	}
	if len(raw) > 8<<20 || json.Unmarshal(raw, &state) != nil || len(state.Proxies) > maxRuntimeCandidates+2 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for name, saved := range state.Proxies {
		n := b.nodes[name]
		if n == nil || !saved.Alive || saved.Status != "healthy" || saved.CheckedAt <= 0 || saved.CheckedAt > now.Unix()+5 || now.Sub(time.Unix(saved.CheckedAt, 0)) > time.Hour || net.ParseIP(saved.ExitIP) == nil || len(saved.History) == 0 {
			continue
		}
		history := saved.History
		if history[len(history)-1].Delay <= 0 {
			continue
		}
		if len(history) > 8 {
			history = history[len(history)-8:]
		}
		n.Alive = true
		n.Status = "healthy"
		n.CheckedAt = saved.CheckedAt
		n.ExitIP = saved.ExitIP
		n.History = history
		n.next = time.Unix(saved.CheckedAt, 0).Add(30 * time.Minute)
	}
	selected := state.Proxies["MOT"].Now
	if n := b.nodes[selected]; n != nil && n.Alive {
		b.current = selected
	}
	if ip := net.ParseIP(state.Proxies["DIRECT"].ExitIP); ip != nil {
		b.directIP = ip.String()
	}
}

func runRuntime(args []string) error {
	flags := flag.NewFlagSet("runtime", flag.ContinueOnError)
	snapshot := flags.String("snapshot", "data/healthy-v1.json", "bounded snapshot path")
	bankURL := flags.String("bank-url", "", "optional loopback NodeBank API")
	controller := flags.String("controller", "127.0.0.1:9093", "loopback controller")
	proxyAddr := flags.String("proxy", "127.0.0.1:7893", "loopback HTTPS proxy")
	health := flags.String("health-url", "https://www.gstatic.com/generate_204", "verified HTTPS check")
	exit := flags.String("exit-url", "https://api.ipify.org", "exit IP check")
	caPath := flags.String("check-ca", "", "additional CA for HTTPS check fixtures")
	if err := flags.Parse(args); err != nil {
		return err
	}
	b := newRuntimeBank()
	if *caPath != "" {
		ca, err := os.ReadFile(*caPath)
		if err != nil {
			return errors.New("cannot read check CA")
		}
		b.checkCA = string(ca)
	}
	b.healthURL = *health
	b.exitURL = *exit
	// Controlled upgrades can carry only fresh VPS measurements across restart.
	// Node names are configuration hashes; changed credentials cannot inherit health.
	resume := *snapshot + ".restart-health.json"
	if info, e := os.Lstat(resume); e == nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0 && info.Size() <= 8<<20 {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && (stat.Uid == 0 || int(stat.Uid) == os.Geteuid()) {
			if raw, e := os.ReadFile(resume); e == nil {
				if candidates, e := readCandidates(context.Background(), *snapshot, *bankURL); e == nil && b.load(candidates) == nil {
					b.restoreHealth(raw)
				}
			}
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	controlListener, err := localListen(*controller)
	if err != nil {
		return err
	}
	defer controlListener.Close()
	proxyListener, err := localListen(*proxyAddr)
	if err != nil {
		return err
	}
	defer proxyListener.Close()
	controlServer := &http.Server{Handler: http.HandlerFunc(b.control), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, MaxHeaderBytes: 16 << 10}
	proxyListener = netutil.LimitListener(proxyListener, 96)
	proxyServer := &http.Server{Handler: http.HandlerFunc(b.proxy), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, ConnState: func(c net.Conn, state http.ConnState) {
		if state == http.StateNew {
			b.track(c)
		} else if state == http.StateClosed {
			b.untrack(c)
		}
	}}
	errs := make(chan error, 2)
	go func() { errs <- controlServer.Serve(controlListener) }()
	go func() { errs <- proxyServer.Serve(proxyListener) }()
	go func() {
		for {
			b.cycle(ctx, *snapshot, *bankURL)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()
	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	cancel()
	b.closeConnections()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	controlServer.Shutdown(shutdown)
	proxyServer.Shutdown(shutdown)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func runtimeMemoryPressure() bool {
	data, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemAvailable:") {
				fields := strings.Fields(line)
				if len(fields) > 1 {
					kb, _ := strconv.ParseUint(fields[1], 10, 64)
					if kb < 128*1024 {
						return true
					}
				}
			}
		}
	}
	data, err = os.ReadFile("/proc/self/status")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				fields := strings.Fields(line)
				if len(fields) > 1 {
					kb, _ := strconv.ParseUint(fields[1], 10, 64)
					if kb > 96*1024 {
						return true
					}
				}
			}
		}
	}
	return false
}
