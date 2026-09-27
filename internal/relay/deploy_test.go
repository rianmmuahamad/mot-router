package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDeploySucceedsOnFreshAccount reproduces the exact failure the user hit:
// the upload succeeds but the account has no workers.dev subdomain yet. Deploy
// must create one and return a usable URL instead of giving up.
func TestDeploySucceedsOnFreshAccount(t *testing.T) {
	var sawSubdomainPut bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/workers/subdomain") && r.Method == http.MethodGet:
			// Fresh account: empty, not an error.
			io.WriteString(w, `{"success":true,"result":{"subdomain":""}}`)
		case strings.Contains(r.URL.Path, "/workers/subdomain") && r.Method == http.MethodPut:
			sawSubdomainPut = true
			io.WriteString(w, `{"success":true,"result":{"subdomain":"mot-2ddb7c66dfb5"}}`)
		case strings.HasSuffix(r.URL.Path, "/subdomain") && r.Method == http.MethodPost:
			io.WriteString(w, `{"success":true,"result":{"enabled":true}}`)
		default:
			// The script upload.
			io.WriteString(w, `{"success":true,"result":{"id":"relay"}}`)
		}
	}))
	defer srv.Close()

	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	// The Worker script URL is built from a hardcoded host, so patch that too by
	// pointing the deploy at our test server via the same apiBase trick.
	res, err := Deploy(context.Background(), DeployConfig{
		AccountID:  "2ddb7c66dfb5953234a2d0bb23cead0b",
		APIToken:   "tok",
		WorkerName: "relay-test",
	})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	if !sawSubdomainPut {
		t.Error("subdomain was never created; the fresh-account case is not handled")
	}
	if res.URL == "" || !strings.Contains(res.URL, ".workers.dev") {
		t.Errorf("URL = %q, want a workers.dev URL", res.URL)
	}
	if res.Name != "relay-test" {
		t.Errorf("name = %q", res.Name)
	}
}

// TestRoundTripAlwaysSendsUserAgent is the regression guard for Cloudflare
// "error code: 1010".
//
// Cloudflare's bot protection answers a relay request that carries no
// User-Agent with HTTP 403 before it reaches the Worker. Go omits the header
// when the caller leaves it empty, so the transport must supply one.
func TestRoundTripAlwaysSendsUserAgent(t *testing.T) {
	var gotUA string
	var gotTarget, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotTarget = r.Header.Get(HeaderTarget)
		gotPath = r.Header.Get(HeaderPath)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	tr := NewTransport(srv.URL)

	// A request with NO User-Agent, as the Zen client sends.
	req, err := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/responses",
		strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Del("User-Agent")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if gotUA == "" {
		t.Error("no User-Agent forwarded; Cloudflare will reject this with error 1010")
	}
	if gotTarget != "https://opencode.ai" {
		t.Errorf("target = %q", gotTarget)
	}
	if gotPath != "/zen/v1/responses" {
		t.Errorf("path = %q", gotPath)
	}
}

// TestRoundTripPreservesCallerUserAgent checks we never overwrite a real one.
func TestRoundTripPreservesCallerUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	req.Header.Set("User-Agent", "opencode/1.18.31 custom")
	resp, err := NewTransport(srv.URL).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotUA != "opencode/1.18.31 custom" {
		t.Errorf("User-Agent = %q, want the caller's value untouched", gotUA)
	}
}
