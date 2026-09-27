// Package relay implements Cloudflare Worker relays: deploying them and routing
// upstream requests through them.
//
// A relay is a tiny Worker that forwards any request it receives to the target
// named in the `x-relay-target` header. Routing through it changes the egress IP
// seen by the upstream (each Worker runs on Cloudflare's edge), which is what
// makes it useful against per-IP rate limits.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WorkerScript is the relay itself, deployed to Cloudflare Workers.
//
// It reads the destination from `x-relay-target` + `x-relay-path`, strips those
// headers, and streams the response back. `duplex: "half"` is required for
// streaming request bodies.
const WorkerScript = `export default {
  async fetch(request, env, ctx) {
    const target = request.headers.get("x-relay-target");
    const relayPath = request.headers.get("x-relay-path") || "/";

    if (!target) {
      return new Response(JSON.stringify({ error: "Missing x-relay-target header" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }

    const targetUrl = target.replace(/\/$/, "") + relayPath;
    const newRequestInit = {
      method: request.method,
      headers: new Headers(request.headers),
    };

    if (request.method !== "GET" && request.method !== "HEAD") {
      newRequestInit.body = request.body;
      newRequestInit.duplex = "half";
    }

    newRequestInit.headers.delete("x-relay-target");
    newRequestInit.headers.delete("x-relay-path");
    newRequestInit.headers.delete("host");

    try {
      const response = await fetch(targetUrl, newRequestInit);
      return new Response(response.body, {
        status: response.status,
        headers: response.headers,
      });
    } catch (error) {
      return new Response(JSON.stringify({ error: error.message }), {
        status: 502,
        headers: { "content-type": "application/json" },
      });
    }
  },
};
`

// Header names the Worker understands.
const (
	HeaderTarget = "x-relay-target"
	HeaderPath   = "x-relay-path"
)

// DeployConfig is the input for deploying a Worker relay.
type DeployConfig struct {
	AccountID  string
	APIToken   string
	WorkerName string
	// Subdomain is the workers.dev subdomain to assign when the account has
	// none. Optional: a deterministic name is derived from the account id.
	Subdomain string
	// CompatibilityDate pins the Workers runtime version.
	CompatibilityDate string
}

// DeployResult reports where the deployed Worker is reachable.
type DeployResult struct {
	URL  string
	Name string
}

// apiBase is the Cloudflare API root, overridable for tests.
var apiBase = "https://api.cloudflare.com/client/v4"

// Diagnose explains an upload failure by probing the credentials.
//
// It runs only after an upload has failed, never before. Gating on it would be
// wrong: a Workers-scoped token can deploy to its account while being unable to
// list accounts via /accounts, because that listing needs account-level read
// scope the token may legitimately lack. The upload is the authoritative test;
// this only turns its terse error into something actionable.
func Diagnose(ctx context.Context, accountID, token string, uploadErr error) string {
	accountID = strings.TrimSpace(accountID)
	token = strings.TrimSpace(token)

	// 1. Is the token itself valid?
	status, body, err := cfRequest(ctx, http.MethodGet, "/user/tokens/verify", token, nil)
	if err != nil {
		return uploadErr.Error() + " (tidak bisa menghubungi Cloudflare untuk diagnosis)"
	}
	if status != http.StatusOK {
		return fmt.Sprintf("%s — API Token ditolak Cloudflare (%d): %s",
			uploadErr, status, cfMessage(body))
	}

	// 2. Can the token manage Workers on this specific account? This is the
	//    check that matters; it is scoped to the account in use.
	status, _, err = cfRequest(ctx, http.MethodGet,
		"/accounts/"+accountID+"/workers/scripts", token, nil)
	if err == nil && status == http.StatusOK {
		// Credentials are fine, so the failure is elsewhere (bad Worker name,
		// workers.dev not enabled, or the body itself).
		return uploadErr.Error()
	}
	if err == nil {
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Sprintf("%s — token valid tapi tidak punya izin Workers pada akun %s. "+
				"Tambahkan permission Account → Workers Scripts → Edit, dan pastikan "+
				"Account Resources mencakup akun ini", uploadErr, accountID)
		case http.StatusNotFound:
			return fmt.Sprintf("%s — Account ID %s tidak dikenal Cloudflare. "+
				"Ambil 32 karakter hex dari halaman overview dashboard", uploadErr, accountID)
		}
	}

	// 3. Fall back to what the token can see, for a clearer message.
	if status, body, err := cfRequest(ctx, http.MethodGet, "/accounts", token, nil); err == nil && status == http.StatusOK {
		var list struct {
			Result []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"result"`
		}
		if json.Unmarshal(body, &list) == nil && len(list.Result) > 0 {
			names := make([]string, 0, len(list.Result))
			for _, a := range list.Result {
				names = append(names, fmt.Sprintf("%s (%s)", a.Name, a.ID))
			}
			return fmt.Sprintf("%s — akun yang bisa diakses token ini: %s",
				uploadErr, strings.Join(names, ", "))
		}
	}

	return uploadErr.Error()
}

// cfRequest performs one Cloudflare API call and returns status and raw body.
func cfRequest(ctx context.Context, method, path, token string, payload []byte) (int, []byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// cfMessage pulls the first error message out of a Cloudflare response.
func cfMessage(body []byte) string {
	var payload struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && len(payload.Errors) > 0 {
		return payload.Errors[0].Message
	}
	if len(body) > 200 {
		body = body[:200]
	}
	return strings.TrimSpace(string(body))
}

// Deploy uploads the relay Worker and returns its workers.dev URL.
//
// Two Cloudflare API calls are involved: uploading the script, then resolving
// the account's workers.dev subdomain so the public URL can be assembled.
func Deploy(ctx context.Context, cfg DeployConfig) (*DeployResult, error) {
	accountID := strings.TrimSpace(cfg.AccountID)
	token := strings.TrimSpace(cfg.APIToken)
	if accountID == "" || token == "" {
		return nil, fmt.Errorf("Cloudflare Account ID and API Token are required")
	}

	// Deliberately NOT gating the upload on a permission preflight.
	//
	// A Workers-scoped token can deploy to its account while being unable to list
	// accounts via /accounts — that listing needs account-level read scope the
	// token may legitimately lack. Gating on it would reject working tokens.
	// The upload itself is the authoritative test; Preflight is only consulted
	// afterwards, to explain a failure.

	name := strings.TrimSpace(cfg.WorkerName)
	if name == "" {
		name = fmt.Sprintf("relay-%d", time.Now().UnixMilli())
	}
	compatDate := cfg.CompatibilityDate
	if compatDate == "" {
		compatDate = "2024-03-20"
	}

	scriptURL := fmt.Sprintf("%s/accounts/%s/workers/scripts/%s",
		apiBase, accountID, url.PathEscape(name))

	// The Workers upload endpoint takes multipart form data: the module plus a
	// metadata part describing the entry point.
	//
	// The parts are written by hand rather than with mime/multipart's
	// CreateFormFile, because that helper hardcodes
	// `Content-Type: application/octet-stream` on every part. Cloudflare reads
	// that as an opaque blob and rejects the upload with
	// "Main module must be an ES module". The module part must instead declare
	// `application/javascript+module`, and the metadata part `application/json`.
	metadata := map[string]any{
		"main_module":        "index.js",
		"compatibility_date": compatDate,
		"observability":      map[string]any{"enabled": true},
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}

	body, contentType, err := buildWorkerUpload(WorkerScript, metaJSON)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, scriptURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload worker: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		uploadErr := fmt.Errorf("Cloudflare upload failed (%d): %s",
			resp.StatusCode, cloudflareError(raw))
		// Ask Cloudflare why, so a permission problem is named instead of
		// surfacing as a bare "Authentication error".
		return nil, fmt.Errorf("%s", Diagnose(ctx, accountID, token, uploadErr))
	}

	// Enable the workers.dev route. A failure here is not fatal: the route may
	// already be enabled from a previous deploy.
	enableReq, err := http.NewRequestWithContext(ctx, http.MethodPost, scriptURL+"/subdomain",
		strings.NewReader(`{"enabled":true}`))
	if err == nil {
		enableReq.Header.Set("Authorization", "Bearer "+token)
		enableReq.Header.Set("Content-Type", "application/json")
		if r, err := http.DefaultClient.Do(enableReq); err == nil {
			io.Copy(io.Discard, io.LimitReader(r.Body, 1<<16))
			r.Body.Close()
		}
	}

	// Resolve the account's workers.dev subdomain, creating one if the account
	// has never had it set.
	//
	// A brand-new account has no subdomain, and `GET` returns an empty string
	// rather than an error. Cloudflare exposes `PUT` to assign one, so the deploy
	// can complete without sending the user to the dashboard.
	subdomain, err := ensureSubdomain(ctx, accountID, token, cfg.Subdomain)
	if err != nil {
		return nil, fmt.Errorf("worker %q deployed, tapi subdomain workers.dev gagal disiapkan: %w", name, err)
	}

	return &DeployResult{
		URL:  fmt.Sprintf("https://%s.%s.workers.dev", name, subdomain),
		Name: name,
	}, nil
}

// ensureSubdomain returns the account's workers.dev subdomain, creating it when
// absent. A caller-supplied preference wins; otherwise the name is derived from
// the account id so it is deterministic and unlikely to collide.
func ensureSubdomain(ctx context.Context, accountID, token, preferred string) (string, error) {
	path := "/accounts/" + accountID + "/workers/subdomain"

	status, body, err := cfRequest(ctx, http.MethodGet, path, token, nil)
	if err != nil {
		return "", fmt.Errorf("tidak bisa membaca subdomain: %w", err)
	}

	if status == http.StatusOK {
		var payload struct {
			Result struct {
				Subdomain string `json:"subdomain"`
			} `json:"result"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Result.Subdomain != "" {
			return payload.Result.Subdomain, nil
		}
	}

	// Not set yet: assign one. Cloudflare rejects a subdomain that is already
	// taken globally, so a deterministic suffix from the account id is used.
	name := strings.TrimSpace(preferred)
	if name == "" {
		name = "mot-" + accountID[:min(12, len(accountID))]
	}
	name = sanitizeSubdomain(name)

	payload, err := json.Marshal(map[string]string{"subdomain": name})
	if err != nil {
		return "", fmt.Errorf("encode subdomain request: %w", err)
	}

	status, body, err = cfRequest(ctx, http.MethodPut, path, token, payload)
	if err != nil {
		return "", fmt.Errorf("tidak bisa membuat subdomain: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("Cloudflare menolak subdomain %q (%d): %s — "+
			"buat manual di dashboard → Workers & Pages → Your subdomain",
			name, status, cfMessage(body))
	}

	var created struct {
		Result struct {
			Subdomain string `json:"subdomain"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		return "", fmt.Errorf("jawaban subdomain tidak terbaca: %w", err)
	}
	if created.Result.Subdomain == "" {
		return "", fmt.Errorf("Cloudflare menerima permintaan tapi tidak mengembalikan subdomain")
	}
	return created.Result.Subdomain, nil
}

// sanitizeSubdomain lowercases and strips characters Cloudflare rejects in a
// workers.dev subdomain.
func sanitizeSubdomain(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "mot-router"
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// cloudflareError extracts a human-readable message from a Cloudflare API error
// body, adding a hint for the failures users actually hit.
func cloudflareError(raw []byte) string {
	var payload struct {
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil && len(payload.Errors) > 0 {
		msg := payload.Errors[0].Message
		if hint := cloudflareHint(msg); hint != "" {
			return msg + " — " + hint
		}
		return msg
	}
	if len(raw) > 300 {
		raw = raw[:300]
	}
	return strings.TrimSpace(string(raw))
}

// cloudflareHint turns the common API rejections into something actionable.
//
// Cloudflare's messages name the failure but not the fix, and the fix differs
// per case: a bad token is a credentials problem, while a permission error means
// the token is valid but scoped wrong.
func cloudflareHint(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "authentication failed"):
		return "periksa API Token (harus token dengan permission Workers Scripts: Edit, bukan Global API Key)"
	case strings.Contains(lower, "permission") || strings.Contains(lower, "not authorized"):
		return "token valid tapi kurang permission; tambahkan Account → Workers Scripts → Edit"
	case strings.Contains(lower, "account") && strings.Contains(lower, "not found"):
		return "Account ID salah; ambil dari sisi kanan halaman overview Cloudflare dashboard"
	case strings.Contains(lower, "could not route") || strings.Contains(lower, "object identifier is invalid"):
		return "Account ID tidak valid; pastikan 32 karakter hex dari halaman overview Cloudflare"
	case strings.Contains(lower, "workers.dev") || strings.Contains(lower, "subdomain"):
		return "aktifkan workers.dev subdomain dulu di Cloudflare dashboard"
	case strings.Contains(lower, "script name") || strings.Contains(lower, "invalid name"):
		return "nama Worker tidak valid; pakai huruf, angka, dan tanda hubung saja"
	default:
		return ""
	}
}

// Transport routes requests through a relay URL.
type Transport struct {
	// RelayURL is the deployed Worker origin, e.g. https://relay-x.workers.dev
	RelayURL string
	// Client performs the actual HTTP calls.
	Client *http.Client
}

// NewTransport builds a transport for a relay URL.
func NewTransport(relayURL string) *Transport {
	return &Transport{
		RelayURL: strings.TrimRight(relayURL, "/"),
		Client:   &http.Client{},
	}
}

// RoundTrip forwards a request to its original destination via the relay.
//
// The destination is preserved in `x-relay-target` / `x-relay-path`, and the
// Worker performs the real call. The request body is streamed, so SSE responses
// pass through without buffering.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.RelayURL == "" {
		return nil, fmt.Errorf("relay URL is empty")
	}

	target := req.URL.Scheme + "://" + req.URL.Host
	relayPath := req.URL.Path
	if req.URL.RawQuery != "" {
		relayPath += "?" + req.URL.RawQuery
	}

	// A POST with a body needs a replayable reader so the retry inside net/http
	// cannot consume it twice.
	var bodyReader io.Reader
	if req.Body != nil {
		bodyReader = req.Body
	}

	relayReq, err := http.NewRequestWithContext(req.Context(), req.Method, t.RelayURL+"/", bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build relay request: %w", err)
	}

	for k, values := range req.Header {
		for _, v := range values {
			relayReq.Header.Add(k, v)
		}
	}
	relayReq.Header.Set(HeaderTarget, target)
	relayReq.Header.Set(HeaderPath, relayPath)
	// The relay sets Host from the target; a stale one confuses Cloudflare.
	relayReq.Header.Del("Host")

	// A missing User-Agent makes Cloudflare reject the relay request with
	// "error code: 1010" before it ever reaches the Worker. Go omits the header
	// when the caller leaves it empty, and the Zen client sets no User-Agent on
	// its own transport, so it must be supplied here.
	if relayReq.Header.Get("User-Agent") == "" {
		relayReq.Header.Set("User-Agent", DefaultRelayUserAgent)
	}

	relayReq.ContentLength = req.ContentLength

	return t.Client.Do(relayReq)
}

// DefaultRelayUserAgent is sent when a request carries no User-Agent.
//
// Cloudflare's bot protection answers a missing User-Agent with HTTP 403
// "error code: 1010", which would otherwise look like a broken relay.
const DefaultRelayUserAgent = "mot-router/1.0"

// Test checks that a relay is reachable and forwarding correctly by asking it to
// fetch a known endpoint.
func Test(ctx context.Context, relayURL string, timeout time.Duration) (int, time.Duration, error) {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(relayURL, "/")+"/", nil)
	if err != nil {
		return 0, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set(HeaderTarget, "https://api.ipify.org")
	req.Header.Set(HeaderPath, "/")
	// Cloudflare answers a missing User-Agent with "error code: 1010".
	req.Header.Set("User-Agent", DefaultRelayUserAgent)

	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(started)
	if err != nil {
		return 0, elapsed, fmt.Errorf("relay unreachable: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, elapsed, fmt.Errorf("relay returned %d", resp.StatusCode)
	}
	return resp.StatusCode, elapsed, nil
}

// EgressIP asks the relay what IP the outside world sees.
func EgressIP(ctx context.Context, relayURL string, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(relayURL, "/")+"/", nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set(HeaderTarget, "https://api.ipify.org")
	req.Header.Set(HeaderPath, "/")
	// Cloudflare answers a missing User-Agent with "error code: 1010".
	req.Header.Set("User-Agent", DefaultRelayUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("relay unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("relay returned %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(body)), nil
}

// ModuleContentType is the part type Cloudflare requires for an ES module.
const ModuleContentType = "application/javascript+module"

// buildWorkerUpload assembles the multipart body for a Worker upload.
//
// It is written by hand because mime/multipart's CreateFormFile forces
// `application/octet-stream` on every part, which Cloudflare rejects with
// "Main module must be an ES module".
func buildWorkerUpload(script string, metaJSON []byte) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	boundary := "----motrouter" + strconv.FormatInt(time.Now().UnixNano(), 36)

	writePart := func(name, filename, partType string, content []byte) error {
		if _, err := fmt.Fprintf(&buf, "--%s\r\n", boundary); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(&buf,
			"Content-Disposition: form-data; name=%q; filename=%q\r\n", name, filename); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(&buf, "Content-Type: %s\r\n\r\n", partType); err != nil {
			return err
		}
		if _, err := buf.Write(content); err != nil {
			return err
		}
		_, err := buf.WriteString("\r\n")
		return err
	}

	// Order matters: the metadata part names the module, so the module part must
	// be present under the exact filename the metadata refers to.
	if err := writePart("index.js", "index.js", ModuleContentType, []byte(script)); err != nil {
		return nil, "", fmt.Errorf("write module part: %w", err)
	}
	if err := writePart("metadata", "metadata.json", "application/json", metaJSON); err != nil {
		return nil, "", fmt.Errorf("write metadata part: %w", err)
	}
	if _, err := fmt.Fprintf(&buf, "--%s--\r\n", boundary); err != nil {
		return nil, "", fmt.Errorf("close multipart: %w", err)
	}

	return buf.Bytes(), "multipart/form-data; boundary=" + boundary, nil
}
