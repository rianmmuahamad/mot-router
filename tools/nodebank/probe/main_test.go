// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	sbhttp "github.com/sagernet/sing-box/protocol/http"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	"github.com/sagernet/sing-box/protocol/socks"
	"github.com/sagernet/sing-box/protocol/trojan"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing-box/protocol/vmess"
	N "github.com/sagernet/sing/common/network"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fixtureRouter struct{ adapter.Router }

func (*fixtureRouter) RouteConnection(ctx context.Context, c net.Conn, m adapter.InboundContext) error {
	defer c.Close()
	up, err := (&net.Dialer{}).DialContext(ctx, "tcp", m.Destination.String())
	if err != nil {
		return err
	}
	defer up.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(up, c)
		if tcp, ok := up.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(c, up)
	<-done
	return nil
}
func (r *fixtureRouter) RouteConnectionEx(ctx context.Context, c net.Conn, m adapter.InboundContext, onClose N.CloseHandlerFunc) {
	go func() {
		err := r.RouteConnection(ctx, c, m)
		if onClose != nil {
			onClose(err)
		}
	}()
}
func TestAdaptersLocal(t *testing.T) {
	// Trust only this fixture's certificate through the normal OS root hook; destination verification remains enabled.
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(204)
		case "/ip":
			_, _ = w.Write([]byte("203.0.113.7"))
		case "/large":
			_, _ = w.Write(make([]byte, maxBody+1))
		case "/stall":
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(204)
		default:
			w.WriteHeader(403)
		}
	}))
	defer target.Close()
	cert := target.Certificate()
	pemCert := target.TLS.Certificates[0]
	_ = cert
	// In-process trust store, equivalent to a private local CA without disabling verification.
	ctx := context.Background()
	logger := log.NewNOPFactory().NewLogger("fixture")
	router := new(fixtureRouter)
	for _, kind := range []string{"http", "socks", "shadowsocks", "vmess", "vless", "trojan"} {
		transports := []string{""}
		if kind == "vless" {
			transports = []string{"", "ws", "grpc", "vision", "reality"}
		}
		if kind == "trojan" {
			transports = []string{"", "ws", "grpc"}
		}
		for _, transport := range transports {
			t.Run(kind+"/"+transport, func(t *testing.T) {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port := l.Addr().(*net.TCPAddr).Port
				_ = l.Close()
				uuid := "11111111-1111-4111-8111-111111111111"
				server := map[string]any{"listen": "127.0.0.1", "listen_port": port}
				client := map[string]any{"type": kind, "server": "127.0.0.1", "server_port": port}
				needTLS := kind == "trojan" || transport == "vision" || transport == "reality"
				if kind == "http" || kind == "socks" {
					server["users"] = []map[string]string{{"username": "test", "password": "correct"}}
					client["username"] = "test"
					client["password"] = "correct"
				}
				if kind == "vless" || kind == "vmess" {
					server["users"] = []map[string]string{{"uuid": uuid, "flow": func() string {
						if transport == "vision" || transport == "reality" {
							return "xtls-rprx-vision"
						}
						return ""
					}()}}
					client["uuid"] = uuid
					if kind == "vmess" {
						client["security"] = "auto"
					}
				}
				if kind == "trojan" {
					server["users"] = []map[string]string{{"password": "correct"}}
					client["password"] = "correct"
				}
				if kind == "shadowsocks" {
					server["method"] = "aes-128-gcm"
					server["password"] = "correct"
					client["method"] = "aes-128-gcm"
					client["password"] = "correct"
				}
				if transport == "ws" || transport == "grpc" {
					tr := map[string]any{"type": transport}
					if transport == "ws" {
						tr["path"] = "/socket"
					} else {
						tr["service_name"] = "fixture"
					}
					server["transport"] = tr
					client["transport"] = tr
				}
				if needTLS {
					server["tls"] = map[string]any{"enabled": true, "certificate": []string{encodePEM("CERTIFICATE", pemCert.Certificate[0])}, "key": []string{encodeKey(pemCert.PrivateKey)}}
					client["tls"] = map[string]any{"enabled": true, "server_name": "example.com", "insecure": true}
					if transport == "vision" || transport == "reality" {
						client["flow"] = "xtls-rprx-vision"
					}
				}
				if transport == "reality" {
					key, _ := ecdh.X25519().GenerateKey(rand.Reader)
					_, p, _ := net.SplitHostPort(strings.TrimPrefix(target.URL, "https://"))
					var pn int
					_, _ = fmt.Sscan(p, &pn)
					server["tls"] = map[string]any{"enabled": true, "server_name": "example.com", "reality": map[string]any{"enabled": true, "handshake": map[string]any{"server": "127.0.0.1", "server_port": pn}, "private_key": base64.RawURLEncoding.EncodeToString(key.Bytes()), "short_id": []string{"0123456789abcdef"}}}
					client["tls"] = map[string]any{"enabled": true, "server_name": "example.com", "utls": map[string]any{"enabled": true, "fingerprint": "chrome"}, "reality": map[string]any{"enabled": true, "public_key": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "short_id": "0123456789abcdef"}}
				}
				body, _ := json.Marshal(server)
				var in adapter.Inbound
				switch kind {
				case "http":
					var o option.HTTPMixedInboundOptions
					_ = json.Unmarshal(body, &o)
					in, err = sbhttp.NewInbound(ctx, router, logger, "fixture", o)
				case "socks":
					var o option.SocksInboundOptions
					_ = json.Unmarshal(body, &o)
					in, err = socks.NewInbound(ctx, router, logger, "fixture", o)
				case "vless":
					var o option.VLESSInboundOptions
					_ = json.Unmarshal(body, &o)
					in, err = vless.NewInbound(ctx, router, logger, "fixture", o)
				case "vmess":
					var o option.VMessInboundOptions
					_ = json.Unmarshal(body, &o)
					in, err = vmess.NewInbound(ctx, router, logger, "fixture", o)
				case "trojan":
					var o option.TrojanInboundOptions
					_ = json.Unmarshal(body, &o)
					in, err = trojan.NewInbound(ctx, router, logger, "fixture", o)
				case "shadowsocks":
					var o option.ShadowsocksInboundOptions
					_ = json.Unmarshal(body, &o)
					in, err = shadowsocks.NewInbound(ctx, router, logger, "fixture", o)
				}
				if err != nil {
					t.Fatal(err)
				}
				scope := adapter.NewScope(ctx, logger)
				defer scope.Close()
				if err = in.Start(adapter.StartStateStart, scope); err != nil {
					t.Fatal(err)
				}
				config, _ := json.Marshal(client)
				req := request{ID: "test", Config: config, HealthURL: target.URL + "/health", ExitURL: target.URL + "/ip", ExpectedStatus: 204, TimeoutMS: 3000, CheckCAPem: encodePEM("CERTIFICATE", pemCert.Certificate[0])}
				res := probe(req)
				if res.Status != "healthy" || res.ExitIP != "203.0.113.7" {
					t.Fatalf("probe: %+v", res)
				}
				if kind == "vless" && transport == "ws" {
					upstream, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
					forwarder := httputil.NewSingleHostReverseProxy(upstream)
					var expectedHost string
					front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Host != expectedHost {
							w.WriteHeader(403)
							return
						}
						forwarder.ServeHTTP(w, r)
					}))
					defer front.Close()
					frontPort := front.Listener.Addr().(*net.TCPAddr).Port
					expectedHost = net.JoinHostPort("localhost", fmt.Sprint(frontPort))
					client["server"] = "localhost"
					client["server_port"] = frontPort
					req.Config, _ = json.Marshal(client)
					req.ResolvedIP = "127.0.0.1"
					res := probe(req)
					if res.Status != "healthy" {
						t.Fatalf("resolved IP changed WebSocket Host: %+v", res)
					}
					client["server"] = "127.0.0.1"
					client["server_port"] = port
					req.Config, _ = json.Marshal(client)
					req.ResolvedIP = ""
				}
				req.HealthURL = target.URL + "/large"
				req.ExpectedStatus = 200
				if probe(req).Status == "healthy" {
					t.Fatal("oversized response accepted")
				}
				req.HealthURL = target.URL + "/denied"
				req.ExpectedStatus = 204
				if probe(req).Status == "healthy" {
					t.Fatal("unexpected HTTP status accepted")
				}
				if kind == "http" || kind == "socks" || kind == "trojan" || kind == "shadowsocks" {
					client["password"] = "wrong"
				} else {
					client["uuid"] = "22222222-2222-4222-8222-222222222222"
				}
				req.Config, _ = json.Marshal(client)
				req.HealthURL = target.URL + "/health"
				req.TimeoutMS = 500
				if probe(req).Status == "healthy" {
					t.Fatal("wrong credentials accepted")
				}
			})
		}
	}
}
func TestTimeoutDisconnectUnsupported(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				select {
				case <-stop:
				case <-time.After(2 * time.Second):
				}
			}()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	for _, transport := range []string{"", "ws", "grpc"} {
		cfg := map[string]any{"type": "vless", "server": "127.0.0.1", "server_port": port, "uuid": "11111111-1111-4111-8111-111111111111"}
		if transport != "" {
			if transport == "ws" {
				cfg["transport"] = map[string]any{"type": transport, "path": "/test"}
			} else {
				cfg["transport"] = map[string]any{"type": transport, "service_name": "test"}
			}
		}
		config, _ := json.Marshal(cfg)
		started := time.Now()
		res := probe(request{ID: "timeout", Config: config, TimeoutMS: 100})
		if res.Status != "failed" || time.Since(started) > time.Second {
			t.Fatalf("%s deadline: %+v after %s", transport, res, time.Since(started))
		}
	}
	raw := json.RawMessage(`{"type":"unknown","server":"127.0.0.1","server_port":1}`)
	if probe(request{ID: "unsupported", Config: raw}).Status != "unsupported" {
		t.Fatal("unsupported configuration misclassified")
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	port = closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	config, _ := json.Marshal(map[string]any{"type": "http", "server": "127.0.0.1", "server_port": port})
	if probe(request{ID: "disconnect", Config: config, TimeoutMS: 200}).Status != "failed" {
		t.Fatal("disconnection accepted")
	}
}

func TestVerifiedDestinationAndDeadline(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer target.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := getSmall(ctx, &http.Client{}, target.URL, 204)
	if err == nil {
		t.Fatal("untrusted TLS accepted")
	}
	if exitIP([]byte("ip=203.0.113.3\n")) != "203.0.113.3" {
		t.Fatal("trace parsing")
	}
	if exitIP([]byte("not an IP")) != "" {
		t.Fatal("invalid IP accepted")
	}
}

func encodePEM(kind string, b []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: b}))
}
func encodeKey(key any) string {
	b, _ := x509.MarshalPKCS8PrivateKey(key)
	return encodePEM("PRIVATE KEY", b)
}
