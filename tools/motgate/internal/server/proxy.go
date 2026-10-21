package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rian/motgate/internal/store"
)

// extractKey accepts `Authorization: Bearer`, `x-api-key`, or `?key=`, matching
// mot-router's own extractor.
func extractKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return r.URL.Query().Get("key")
}

// handleInference authenticates the portal user, then reverse-proxies the
// request to the guard (and on to mot-router) while counting tokens.
func (s *Server) handleInference(w http.ResponseWriter, r *http.Request) {
	keyStr := extractKey(r)
	if keyStr == "" {
		writeAPIError(w, http.StatusUnauthorized, "missing API key", "authentication_error")
		return
	}
	u, err := s.store.LookupUser(keyStr)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if u == nil {
		writeAPIError(w, http.StatusUnauthorized, "invalid API key", "authentication_error")
		return
	}
	if u.Revoked != 0 {
		writeAPIError(w, http.StatusForbidden, "key revoked", "permission_error")
		return
	}
	// The expiry is the access gate for the portal; mot-router knows nothing
	// about it.
	if u.ExpiresAt != 0 && nowMillis() >= u.ExpiresAt {
		writeAPIError(w, http.StatusForbidden, "access expired", "permission_error")
		return
	}
	quotaRequest := ""
	if u.PlanType == "token" && !(r.Method == http.MethodGet && r.URL.Path == "/v1/models") {
		b, readErr := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if readErr != nil {
			writeAPIError(w, http.StatusBadRequest, "could not read request body", "invalid_request_error")
			return
		}
		if len(b) >= 2<<20 {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request body exceeds 2 MiB", "invalid_request_error")
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			writeAPIError(w, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error")
			return
		}
		var model string
		_ = json.Unmarshal(fields["model"], &model)
		model = strings.TrimSpace(model)
		surface := "openai"
		if strings.HasPrefix(r.URL.Path, "/v1/messages") {
			surface = "anthropic"
		} else if strings.Contains(r.URL.Path, "systemone") {
			surface = "systemone"
		}
		expectedModel := quotaModelExpected(model, surface)
		if model == "" || expectedModel == "" {
			writeAPIError(w, http.StatusForbidden, "this model and endpoint do not have verified token metering for token plans", "permission_error")
			return
		}
		if surface == "openai" {
			var streaming bool
			_ = json.Unmarshal(fields["stream"], &streaming)
			if streaming {
				b, readErr = ensureStreamUsage(b)
				if readErr != nil {
					writeAPIError(w, http.StatusBadRequest, "could not enable final usage reporting", "invalid_request_error")
					return
				}
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.TransferEncoding = nil
		r.Header.Del("Transfer-Encoding")
		if r.ContentLength > 0 {
			r.Header.Set("Content-Length", strconvI64(r.ContentLength))
		}
		var beginErr error
		quotaRequest, beginErr = s.store.BeginQuotaRequest(r.Context(), u.ID, surface, expectedModel, true)
		if beginErr != nil {
			writeAPIError(w, http.StatusForbidden, "token package is exhausted, expired, busy, or awaiting usage review", "permission_error")
			return
		}
	}

	target, err := url.Parse(s.cfg.Upstream)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "bad upstream url", "api_error")
		return
	}

	surface := "openai"
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		surface = "anthropic"
	} else if strings.Contains(r.URL.Path, "systemone") {
		surface = "systemone"
	}

	userID := u.ID
	started := time.Now()

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// The guard authenticates with the internal master key; the user's
			// key must never travel further. X-Motgate-User carries the portal
			// identity for guard admission control — keep it.
			req.Header.Del("Authorization")
			req.Header.Del("x-api-key")
			req.Header.Set("X-Motgate-User", strconvI64(userID))
		},
		// SSE must reach the client as it is produced.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if u.PlanType == "token" && r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
				return filterQuotaModels(resp)
			}
			body := newUsageScanner(resp.Body)
			resp.Body = &countingBody{inner: body, onFinish: func(eof bool) {
				in, out, model, complete := body.totals()
				status := resp.StatusCode
				usage := store.UsageInput{
					UserID:    userID,
					Surface:   surface,
					Model:     model,
					Status:    status,
					Latency:   time.Since(started).Milliseconds(),
					InTokens:  in,
					OutTokens: out,
					Measured:  complete,
					RequestID: quotaRequest,
				}
				if quotaRequest != "" {
					if err := s.store.SettleQuotaRequest(context.Background(), quotaRequest, usage, complete, true); err != nil {
						_ = s.store.QuarantineQuotaRequest(context.Background(), quotaRequest, err.Error())
					}
				} else {
					_ = s.store.AddUsage(usage)
				}
			}}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if quotaRequest != "" {
				if settleErr := s.store.SettleQuotaRequest(context.Background(), quotaRequest, store.UsageInput{UserID: userID, Surface: surface, Status: http.StatusBadGateway, Latency: time.Since(started).Milliseconds()}, false, false); settleErr != nil {
					_ = s.store.QuarantineQuotaRequest(context.Background(), quotaRequest, settleErr.Error())
				}
			}
			writeAPIError(w, http.StatusBadGateway, "upstream error: "+err.Error(), "upstream_error")
		},
	}
	proxy.ServeHTTP(w, r)
}

func ensureStreamUsage(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	options := map[string]json.RawMessage{}
	if raw := fields["stream_options"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &options); err != nil {
			return nil, err
		}
	}
	if options == nil {
		options = make(map[string]json.RawMessage)
	}
	options["include_usage"] = json.RawMessage("true")
	encoded, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	fields["stream_options"] = encoded
	return json.Marshal(fields)
}

// countingBody invokes onClose exactly once, when the response body ends.
type countingBody struct {
	inner    io.ReadCloser
	onFinish func(bool)
	done     sync.Once
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	if err == io.EOF {
		c.finish(true)
	}
	return n, err
}

func (c *countingBody) Close() error {
	c.finish(false)
	return c.inner.Close()
}

func (c *countingBody) finish(eof bool) {
	c.done.Do(func() { c.onFinish(eof) })
}

var verifiedQuotaModels = map[string]bool{
	"deepseek-v4.1-flash-free": true,
}

func quotaModelExpected(model, surface string) string {
	if surface != "openai" {
		return ""
	}
	model = strings.TrimPrefix(model, "opencode-zen/")
	if model == "deepseek-v4.1-flash" {
		return "deepseek-v4.1-flash-free"
	}
	if verifiedQuotaModels[model] {
		return model
	}
	return ""
}
func quotaModelAllowed(model, surface string) bool {
	return quotaModelExpected(model, surface) != ""
}

func filterQuotaModels(resp *http.Response) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if len(body) > 2<<20 {
		return fmt.Errorf("model list exceeded size limit")
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(body, &doc); err != nil {
		return err
	}
	var models []map[string]any
	if err = json.Unmarshal(doc["data"], &models); err != nil {
		return err
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if id, ok := model["id"].(string); ok && quotaModelAllowed(id, "openai") {
			filtered = append(filtered, model)
		}
	}
	doc["data"], err = json.Marshal(filtered)
	if err != nil {
		return err
	}
	body, err = json.Marshal(doc)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.TransferEncoding = nil
	resp.Header.Set("Content-Length", strconvI64(int64(len(body))))
	resp.Header.Del("Transfer-Encoding")
	return nil
}

func strconvI64(v int64) string {
	// Small local helper to avoid importing strconv twice across files.
	return strconvFormatInt(v)
}
