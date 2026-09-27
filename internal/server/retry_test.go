package server

import "testing"

// TestIpTiedFailureClassifiesByFix is the regression guard for two coupled bugs.
//
// First, the Zen path ignored the operator's rotation switch so the Zen path, so turning rotation off appeared to work but
// kept switching nodes. Second, fixing that made disabling rotation also
// disable retrying, so one transient stall failed outright — measured as a 502
// after a 22s header timeout on a single-node path.
//
// The distinction matters because a rate limit is decided from the exit IP, so
// repeating the request against the same node is pointless. A dropped
// connection is transient, and the same node often serves the next attempt.
func TestIpTiedFailureClassifiesByFix(t *testing.T) {
	cases := []struct {
		name   string
		status int
		detail string
		want   bool
	}{
		// Bound to the exit IP: retrying the same node cannot help.
		{"rate limit", 429, `{"error":{"type":"FreeUsageLimitError"}}`, true},
		{"region block", 403, `{"error":{"type":"RegionError"}}`, true},

		// Transient: worth retrying on the same node.
		{"dropped connection", 0, "unexpected EOF", false},
		{"overloaded", 503, "service_overloaded", false},
		{"bad gateway", 502, "bad gateway", false},
		{"success", 200, "", false},
	}

	for _, c := range cases {
		if got := ipTiedFailure(c.status, c.detail); got != c.want {
			t.Errorf("%s: ipTiedFailure(%d, %q) = %v, want %v",
				c.name, c.status, c.detail, got, c.want)
		}
	}
}

// TestShouldRotateIsBroaderThanIpTied keeps the two predicates from drifting:
// everything that follows the exit IP must also be a rotation trigger, but not
// the reverse — a transport failure rotates yet is still retryable in place.
func TestShouldRotateIsBroaderThanIpTied(t *testing.T) {
	transient := []struct {
		status int
		detail string
	}{
		{0, "unexpected EOF"},
		{503, "service_overloaded"},
		{502, "bad gateway"},
	}
	for _, c := range transient {
		if !shouldRotate(c.status, c.detail) {
			t.Errorf("shouldRotate(%d, %q) must be true", c.status, c.detail)
		}
		if ipTiedFailure(c.status, c.detail) {
			t.Errorf("ipTiedFailure(%d, %q) must be false so the same node is retried",
				c.status, c.detail)
		}
	}

	ipTied := []struct {
		status int
		detail string
	}{
		{429, "rate limit"},
		{403, `{"type":"RegionError"}`},
	}
	for _, c := range ipTied {
		if !shouldRotate(c.status, c.detail) {
			t.Errorf("shouldRotate(%d, %q) must be true", c.status, c.detail)
		}
		if !ipTiedFailure(c.status, c.detail) {
			t.Errorf("ipTiedFailure(%d, %q) must be true", c.status, c.detail)
		}
	}
}
