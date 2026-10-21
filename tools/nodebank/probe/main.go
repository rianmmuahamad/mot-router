// SPDX-License-Identifier: GPL-3.0-or-later
// A bounded, disposable-adapter probe worker. It never loads the node bank.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	sbhttp "github.com/sagernet/sing-box/protocol/http"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	"github.com/sagernet/sing-box/protocol/socks"
	"github.com/sagernet/sing-box/protocol/trojan"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing-box/protocol/vmess"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

const maxFrame = 128 * 1024
const maxBody = 16 * 1024

type request struct {
	ID             string          `json:"id"`
	Config         json.RawMessage `json:"config"`
	TimeoutMS      int             `json:"timeout_ms"`
	Deadline       int64           `json:"deadline_ms"`
	HealthURL      string          `json:"health_url"`
	ExitURL        string          `json:"exit_url"`
	ExpectedStatus int             `json:"expected_status"`
	CheckCAPem     string          `json:"check_ca_pem"`
	ResolvedIP     string          `json:"resolved_ip"`
}
type result struct {
	ID      string  `json:"id"`
	Status  string  `json:"status"`
	Latency float64 `json:"latency_ms"`
	ExitIP  string  `json:"exit_ip"`
	Error   string  `json:"error"`
}

func preserveTransportIdentity(values map[string]json.RawMessage, server string) {
	var tr map[string]any
	_ = json.Unmarshal(values["transport"], &tr)
	if tr == nil {
		return
	}
	var port int
	_ = json.Unmarshal(values["server_port"], &port)
	authority := net.JoinHostPort(server, fmt.Sprint(port))
	switch tr["type"] {
	case "ws":
		headers, _ := tr["headers"].(map[string]any)
		if headers == nil {
			headers = map[string]any{}
		}
		present := false
		for key := range headers {
			if strings.EqualFold(key, "Host") {
				present = true
			}
		}
		if !present {
			headers["Host"] = authority
		}
		tr["headers"] = headers
	case "http":
		hosts, _ := tr["host"].([]any)
		if len(hosts) == 0 {
			tr["host"] = []string{authority}
		}
	case "httpupgrade":
		if tr["host"] == nil || tr["host"] == "" {
			var tlsValues map[string]any
			_ = json.Unmarshal(values["tls"], &tlsValues)
			if name, ok := tlsValues["server_name"].(string); ok && name != "" && tlsValues["enabled"] == true {
				tr["host"] = name
			} else {
				tr["host"] = authority
			}
		}

	}
	values["transport"], _ = json.Marshal(tr)
}

func newOutbound(ctx context.Context, config json.RawMessage, resolved string) (adapter.Outbound, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(config, &values); err != nil {
		return nil, err
	}
	var kind, server string
	_ = json.Unmarshal(values["type"], &kind)
	_ = json.Unmarshal(values["server"], &server)
	delete(values, "type")
	if net.ParseIP(server) == nil {
		ip := net.ParseIP(resolved)
		if ip == nil {
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, server)
			if err != nil {
				return nil, fmt.Errorf("dns: %w", err)
			}
			if len(ips) == 0 {
				return nil, errors.New("dns: no address")
			}
			ip = ips[0].IP
		}
		// Preserve original TLS/HTTP identity while avoiding a global sing-box DNS service.
		if raw := values["tls"]; len(raw) > 0 {
			var tls map[string]any
			_ = json.Unmarshal(raw, &tls)
			if tls["server_name"] == nil || tls["server_name"] == "" {
				tls["server_name"] = server
			}
			values["tls"], _ = json.Marshal(tls)
		}
		preserveTransportIdentity(values, server)
		values["server"], _ = json.Marshal(ip.String())
	}
	body, _ := json.Marshal(values)
	logger := log.NewNOPFactory().NewLogger("probe")
	switch kind {
	case "vless":
		var o option.VLESSOutboundOptions
		if err := json.Unmarshal(body, &o); err != nil {
			return nil, err
		}
		return vless.NewOutbound(ctx, nil, logger, "probe", o)
	case "vmess":
		var o option.VMessOutboundOptions
		if err := json.Unmarshal(body, &o); err != nil {
			return nil, err
		}
		return vmess.NewOutbound(ctx, nil, logger, "probe", o)
	case "trojan":
		var o option.TrojanOutboundOptions
		if err := json.Unmarshal(body, &o); err != nil {
			return nil, err
		}
		return trojan.NewOutbound(ctx, nil, logger, "probe", o)
	case "shadowsocks":
		var o option.ShadowsocksOutboundOptions
		if err := json.Unmarshal(body, &o); err != nil {
			return nil, err
		}
		return shadowsocks.NewOutbound(ctx, nil, logger, "probe", o)
	case "http":
		var o option.HTTPOutboundOptions
		if err := json.Unmarshal(body, &o); err != nil {
			return nil, err
		}
		return sbhttp.NewOutbound(ctx, nil, logger, "probe", o)
	case "socks":
		var o option.SOCKSOutboundOptions
		if err := json.Unmarshal(body, &o); err != nil {
			return nil, err
		}
		o.Version = "5"
		return socks.NewOutbound(ctx, nil, logger, "probe", o)
	default:
		return nil, errors.New("unsupported protocol")
	}
}
func getSmall(ctx context.Context, client *http.Client, target string, expected int) ([]byte, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return nil, errors.New("HTTPS check URL required")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mot-NodeBank/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != expected {
		return nil, errors.New("unexpected HTTPS status")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if len(body) > maxBody {
		return nil, errors.New("response over 16 KiB")
	}
	return body, err
}
func exitIP(body []byte) string {
	s := strings.TrimSpace(string(body))
	if net.ParseIP(s) != nil {
		return s
	}
	var v struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(body, &v) == nil && net.ParseIP(v.IP) != nil {
		return v.IP
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "ip=") && net.ParseIP(strings.TrimPrefix(line, "ip=")) != nil {
			return strings.TrimPrefix(line, "ip=")
		}
	}
	return ""
}

// Track raw adapter sockets too: transports may perform handshakes before returning a connection.
type probeConnections struct {
	adapter.ConnectionManager
	ctx   context.Context
	mu    sync.Mutex
	conns []net.Conn
}

func (m *probeConnections) TrackConn(c net.Conn) net.Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		_ = c.Close()
		return c
	}
	m.conns = append(m.conns, c)
	if deadline, ok := m.ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	return c
}
func (m *probeConnections) TrackPacketConn(c net.PacketConn) net.PacketConn { return c }
func (m *probeConnections) Count() int                                      { m.mu.Lock(); defer m.mu.Unlock(); return len(m.conns) }
func (m *probeConnections) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		_ = c.Close()
	}
	m.conns = nil
}

func probe(r request) (out result) {
	out = result{ID: r.ID, Status: "failed"}
	defer func() {
		if recover() != nil {
			out.Status = "unsupported"
			out.Error = "adapter panic; unsupported configuration"
		}
	}()
	timeout := r.TimeoutMS
	if timeout <= 0 || timeout > 15000 {
		timeout = 15000
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
	if r.Deadline > 0 && time.UnixMilli(r.Deadline).Before(deadline) {
		deadline = time.UnixMilli(r.Deadline)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	manager := &probeConnections{ctx: ctx}
	stop := context.AfterFunc(ctx, manager.CloseAll)
	defer stop()
	defer manager.CloseAll()
	ctx = service.ContextWith[adapter.ConnectionManager](ctx, manager)
	ob, err := newOutbound(ctx, r.Config, r.ResolvedIP)
	if err != nil {
		if strings.Contains(err.Error(), "dns:") || ctx.Err() != nil {
			out.Error = "DNS or deadline failed"
		} else {
			out.Status = "unsupported"
			out.Error = "adapter rejected configuration"
		}
		return
	}
	scope := adapter.NewScope(ctx, log.NewNOPFactory().NewLogger("probe"))
	if starter, ok := ob.(interface {
		Start(adapter.StartStage, *adapter.Scope) error
	}); ok {
		if err = starter.Start(adapter.StartStateInitialize, scope); err != nil {
			out.Status = "unsupported"
			out.Error = "adapter initialization failed"
			return
		}
	}
	defer scope.Close()
	if keeper, ok := ob.(adapter.IdleConnectionKeeper); ok {
		defer keeper.CloseIdleConnections()
	}
	var roots *x509.CertPool
	if r.CheckCAPem != "" {
		roots, _ = x509.SystemCertPool()
		if roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM([]byte(r.CheckCAPem)) {
			out.Status = "unsupported"
			out.Error = "invalid check CA"
			return
		}
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return ob.DialContext(ctx, "tcp", M.ParseSocksaddr(address))
	}, DisableKeepAlives: true, MaxIdleConns: 0, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 16 * 1024, ForceAttemptHTTP2: false}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if r.HealthURL == "" {
		r.HealthURL = "https://www.gstatic.com/generate_204"
	}
	if r.ExitURL == "" {
		r.ExitURL = "https://api.ipify.org"
	}
	if r.ExpectedStatus == 0 {
		r.ExpectedStatus = 204
	}
	started := time.Now()
	_, err = getSmall(ctx, client, r.HealthURL, r.ExpectedStatus)
	out.Latency = float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		out.Error = "protocol / verified HTTPS check failed"
		if ctx.Err() != nil {
			out.Error = "overall deadline exceeded"
		}
		return
	}
	body, err := getSmall(ctx, client, r.ExitURL, 200)
	if err != nil {
		out.Error = "exit IP check failed"
		return
	}
	out.ExitIP = exitIP(body)
	if out.ExitIP == "" {
		out.Error = "invalid exit IP response"
		return
	}
	out.Status = "healthy"
	return
}
func main() {
	runtime.GOMAXPROCS(2)
	debug.SetMemoryLimit(96 * 1024 * 1024)
	if len(os.Args) > 1 && os.Args[1] == "runtime" {
		if err := runRuntime(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), maxFrame)
	jobs := make(chan request, 32)
	var wg sync.WaitGroup
	var output sync.Mutex
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				res := probe(r)
				output.Lock()
				_ = json.NewEncoder(os.Stdout).Encode(res)
				output.Unlock()
			}
		}()
	}
	for input.Scan() {
		var r request
		if json.Unmarshal(input.Bytes(), &r) != nil || r.ID == "" || len(r.ID) > 128 {
			continue
		}
		jobs <- r
	}
	close(jobs)
	wg.Wait()
	if input.Err() != nil {
		os.Exit(2)
	}
}
