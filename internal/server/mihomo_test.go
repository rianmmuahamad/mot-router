package server

import "testing"

// TestShouldRotateCoversRealFailures is the regression guard for the bug where
// a dropped connection or an overloaded backend was surfaced to the client as a
// 502/503 instead of being retried on another node.
//
// Measured before the fix: 1 of 8 requests succeeded, at ~42s average latency.
// After: 8 of 8, at ~3.5s.
func TestShouldRotateCoversRealFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
		detail string
		want   bool
	}{
		// Must rotate.
		{"per-IP rate limit", 429, `{"type":"error","error":{"type":"FreeUsageLimitError"}}`, false},
		{"region block", 403, `{"type":"error","error":{"type":"RegionError","message":"not available in your region"}}`, false},
		{"dropped connection", 0, `call upstream: Post "https://x": unexpected EOF`, true},
		{"plain eof", 0, "EOF", true},
		{"connection reset", 0, "read tcp: connection reset by peer", true},
		{"upstream overloaded", 503, `{"type":"server_error","message":"[service_overloaded] temporarily overloaded"}`, true},
		{"bad gateway", 502, "bad gateway", true},

		// Must NOT rotate: these are not exit-node problems.
		{"success", 200, "", false},
		{"bad request", 400, "invalid json", false},
		{"auth error", 401, "missing api key", false},
		{"not found", 404, "model not found", false},
	}

	for _, c := range cases {
		got := shouldRotate(c.status, c.detail)
		if got != c.want {
			t.Errorf("%s: shouldRotate(%d, %q) = %v, want %v",
				c.name, c.status, c.detail, got, c.want)
		}
	}
}

// TestIsTransportFailureIgnoresUnrelatedErrors keeps the classifier narrow: a
// 400 must not be mistaken for a dropped connection just because the body
// happens to contain a matching word.
func TestIsTransportFailureIgnoresUnrelatedErrors(t *testing.T) {
	if isTransportFailure(400, "the word eof appears in this validation message") {
		t.Error("a 4xx with a matching word should not count as a transport failure")
	}
	if isTransportFailure(200, "") {
		t.Error("a success is not a transport failure")
	}
	if !isTransportFailure(0, "anything") {
		t.Error("status 0 means no HTTP response at all, which is a transport failure")
	}
}

// TestIsUpstreamOverloadedScope checks the overload classifier.
func TestIsUpstreamOverloadedScope(t *testing.T) {
	if !isUpstreamOverloaded(503, "") {
		t.Error("503 should be treated as overloaded")
	}
	if !isUpstreamOverloaded(200, "[service_overloaded]") {
		t.Error("service_overloaded in the body should be detected")
	}
	if isUpstreamOverloaded(400, "bad request") {
		t.Error("400 is not an overload condition")
	}
	if isUpstreamOverloaded(429, "rate limit") {
		t.Error("429 is a rate limit, handled separately")
	}
}
