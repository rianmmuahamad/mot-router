package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCloudflareErrorExtractsMessage verifies the API error body is turned into
// something a human can act on, rather than a raw JSON blob.
func TestCloudflareErrorExtractsMessage(t *testing.T) {
	raw := []byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication failed"}]}`)
	got := cloudflareError(raw)

	if !strings.Contains(got, "Authentication failed") {
		t.Errorf("message lost: %q", got)
	}
	// A hint must be appended, otherwise the user cannot tell what to fix.
	if !strings.Contains(got, "API Token") {
		t.Errorf("no actionable hint appended: %q", got)
	}
}

// TestCloudflareErrorHints covers each known failure mode mapping to its fix.
func TestCloudflareErrorHints(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"Authentication failed", "API Token"},
		{"You do not have permission to perform this action", "permission"},
		{"Account not found", "Account ID"},
		{"Could not route to /client/v4/accounts/x", "Account ID"},
		{"workers.dev subdomain not configured", "workers.dev"},
		{"Invalid script name", "nama Worker"},
	}
	for _, c := range cases {
		hint := cloudflareHint(c.msg)
		if hint == "" {
			t.Errorf("no hint for %q", c.msg)
			continue
		}
		if !strings.Contains(hint, c.want) {
			t.Errorf("hint for %q = %q, want it to mention %q", c.msg, hint, c.want)
		}
	}
}

// TestCloudflareErrorUnknownMessage checks an unrecognised failure still yields
// a readable string rather than being dropped.
func TestCloudflareErrorUnknownMessage(t *testing.T) {
	raw := []byte(`{"errors":[{"message":"Some brand new failure"}]}`)
	got := cloudflareError(raw)
	if got != "Some brand new failure" {
		t.Errorf("got %q, want the message verbatim with no hint", got)
	}
}

// TestCloudflareErrorNonJSON covers a response that is not the documented shape,
// which happens behind a proxy or on a crash.
func TestCloudflareErrorNonJSON(t *testing.T) {
	raw := []byte(`<html>502 Bad Gateway</html>`)
	got := cloudflareError(raw)
	if got == "" {
		t.Fatal("empty result hides the cause")
	}
	if strings.Contains(got, "[object") {
		t.Errorf("got %q, want a plain string", got)
	}
}

// TestCloudflareErrorTruncatesLongBody guards against dumping a huge body.
func TestCloudflareErrorTruncatesLongBody(t *testing.T) {
	raw := []byte(strings.Repeat("x", 5000))
	got := cloudflareError(raw)
	if len(got) > 400 {
		t.Errorf("result length %d, want it truncated", len(got))
	}
}

// TestDiagnoseKeepsOriginalErrorWhenCredentialsAreFine checks that a failure
// unrelated to auth is reported as-is, without a misleading hint.
//
// This guards the regression where a permission preflight wrongly blocked a
// token that could actually deploy: the upload must be attempted first, and
// Diagnose consulted only to explain a failure.
func TestDiagnoseKeepsOriginalErrorWhenCredentialsAreFine(t *testing.T) {
	// No network: an unreachable API must not lose the original message.
	orig := errors.New("Cloudflare upload failed (400): Main module must be an ES module")
	got := Diagnose(context.Background(), "acct", "tok", orig)
	if !strings.Contains(got, "Main module must be an ES module") {
		t.Errorf("original error lost: %q", got)
	}
}

// TestCfMessageExtractsFirstError checks the helper used by preflight messages.
func TestCfMessageExtractsFirstError(t *testing.T) {
	body := []byte(`{"success":false,"errors":[{"code":9109,"message":"Unauthorized to access requested resource"}]}`)
	if got := cfMessage(body); got != "Unauthorized to access requested resource" {
		t.Errorf("got %q", got)
	}
	// A non-JSON body must still yield something readable.
	if got := cfMessage([]byte("<html>oops</html>")); got == "" {
		t.Error("non-JSON body produced an empty message")
	}
}

// TestBuildWorkerUploadPartTypes is the regression guard for the
// "Main module must be an ES module" failure.
//
// mime/multipart's CreateFormFile writes application/octet-stream for every
// part, which Cloudflare reads as an opaque blob. The module part must declare
// application/javascript+module.
func TestBuildWorkerUploadPartTypes(t *testing.T) {
	body, contentType, err := buildWorkerUpload("export default {fetch(){}}", []byte(`{"main_module":"index.js"}`))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(body)

	if !strings.HasPrefix(contentType, "multipart/form-data; boundary=") {
		t.Errorf("content type = %q", contentType)
	}
	if !strings.Contains(s, ModuleContentType) {
		t.Errorf("module part is missing %q; got:\n%s", ModuleContentType, s)
	}
	if !strings.Contains(s, "application/json") {
		t.Error("metadata part is missing application/json")
	}
	if strings.Contains(s, "application/octet-stream") {
		t.Errorf("octet-stream leaked in; Cloudflare rejects this:\n%s", s)
	}
	// The metadata names index.js, so that exact part name must exist.
	if !strings.Contains(s, `name="index.js"`) {
		t.Error(`module part must be named "index.js" to match main_module`)
	}
	if !strings.Contains(s, `name="metadata"`) {
		t.Error(`metadata part must be named "metadata"`)
	}
	if !strings.HasSuffix(strings.TrimRight(s, "\r\n"), "--") {
		t.Error("multipart body is not terminated with a closing boundary")
	}
}

// TestBuildWorkerUploadBoundaryMatchesHeader checks the boundary in the header
// matches the one used in the body, otherwise Cloudflare cannot parse it.
func TestBuildWorkerUploadBoundaryMatchesHeader(t *testing.T) {
	body, contentType, err := buildWorkerUpload("x", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, boundary, found := strings.Cut(contentType, "boundary=")
	if !found {
		t.Fatalf("no boundary in %q", contentType)
	}
	if !strings.Contains(string(body), "--"+boundary) {
		t.Error("boundary in body does not match the header")
	}
}

// TestSanitizeSubdomain covers the characters Cloudflare accepts.
func TestSanitizeSubdomain(t *testing.T) {
	cases := map[string]string{
		"My-Relay":        "my-relay",
		"mot router":      "motrouter",
		"UPPER_case":      "uppercase",
		"--dashes--":      "dashes",
		"":                "mot-router",
		"weird!@#$%^&*()": "weird",
	}
	for in, want := range cases {
		if got := sanitizeSubdomain(in); got != want {
			t.Errorf("sanitizeSubdomain(%q) = %q, want %q", in, got, want)
		}
	}
	// Length cap must hold.
	long := strings.Repeat("a", 100)
	if got := sanitizeSubdomain(long); len(got) > 63 {
		t.Errorf("length %d exceeds the 63-character limit", len(got))
	}
}

// TestEnsureSubdomainCreatesWhenMissing is the regression guard for the
// "no workers.dev subdomain is configured" failure: a fresh account returns an
// empty string from GET, and the deploy must then create one via PUT.
func TestEnsureSubdomainCreatesWhenMissing(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	var calls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotMethod, gotPath = r.Method, r.URL.Path
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			// A brand-new account: 200 with an empty subdomain.
			io.WriteString(w, `{"success":true,"result":{"subdomain":""}}`)
			return
		}
		io.WriteString(w, `{"success":true,"result":{"subdomain":"mot-abc123"}}`)
	}))
	defer srv.Close()

	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	got, err := ensureSubdomain(context.Background(), "acct123", "tok", "")
	if err != nil {
		t.Fatalf("ensureSubdomain: %v", err)
	}
	if got != "mot-abc123" {
		t.Errorf("subdomain = %q, want mot-abc123", got)
	}
	if calls != 2 {
		t.Errorf("made %d calls, want GET then PUT", calls)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("last method = %s, want PUT", gotMethod)
	}
	if !strings.Contains(gotPath, "/workers/subdomain") {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, "subdomain") {
		t.Errorf("PUT body missing subdomain: %q", gotBody)
	}
}

// TestEnsureSubdomainUsesExisting checks an account that already has one is left
// alone, with no extra write.
func TestEnsureSubdomainUsesExisting(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		io.WriteString(w, `{"success":true,"result":{"subdomain":"already-there"}}`)
	}))
	defer srv.Close()

	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	got, err := ensureSubdomain(context.Background(), "acct", "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "already-there" {
		t.Errorf("got %q", got)
	}
	if len(methods) != 1 || methods[0] != http.MethodGet {
		t.Errorf("methods = %v, want a single GET", methods)
	}
}

// TestEnsureSubdomainPrefersRequestedName checks the caller's choice wins.
func TestEnsureSubdomainPrefersRequestedName(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
		}
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"success":true,"result":{"subdomain":""}}`)
			return
		}
		io.WriteString(w, `{"success":true,"result":{"subdomain":"my-pick"}}`)
	}))
	defer srv.Close()

	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	got, err := ensureSubdomain(context.Background(), "acct", "tok", "My-Pick")
	if err != nil {
		t.Fatal(err)
	}
	if got != "my-pick" {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(body, `"my-pick"`) {
		t.Errorf("requested name not sent sanitized: %q", body)
	}
}
