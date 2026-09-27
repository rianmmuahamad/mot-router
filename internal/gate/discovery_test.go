package gate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestInconclusiveCoversTransportFailure is the regression guard for a bug found
// while deploying to a VPS with a flaky route.
//
// The tool-discovery scan removes tool names one at a time and keeps a removal
// only when the smaller set is accepted. A dropped connection returned neither
// acceptance nor rejection, but was read as rejection, so the name stayed in the
// list. Measured effect: the required list inflated from 5 names to 8.
func TestInconclusiveCoversTransportFailure(t *testing.T) {
	// A transport error carries no verdict about the request.
	if !inconclusive(verdict{err: errors.New("unexpected EOF")}) {
		t.Error("a transport failure must be inconclusive, not a rejection")
	}
	// A rate limit says nothing about the request shape either.
	if !inconclusive(verdict{status: http.StatusTooManyRequests}) {
		t.Error("429 must be inconclusive")
	}
	// A definite rejection is NOT inconclusive.
	if inconclusive(verdict{status: http.StatusForbidden}) {
		t.Error("403 is a definite rejection, not inconclusive")
	}
	if inconclusive(verdict{status: http.StatusOK}) {
		t.Error("200 is acceptance, not inconclusive")
	}
}

// TestDiscoverRequiredToolsSurvivesFlakyNetwork proves a dropped connection
// during the scan does not inflate the required tool list.
//
// The fake upstream fails the first request for any reduced set with a transport
// error, then answers correctly. A prober that treated the error as a rejection
// would keep every name.
func TestDiscoverRequiredToolsSurvivesFlakyNetwork(t *testing.T) {
	var failedOnce atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject a request that omits the required pair, so reduction can succeed.
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		missingRequired := !containsAll(string(body), []string{"bash", "read"})

		if !failedOnce.Load() {
			// Simulate one dropped connection early in the scan.
			failedOnce.Store(true)
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}

		if missingRequired {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"type":"error","error":{"type":"FreeTierError"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewProber(srv.URL, "test-model")
	p.Configure(srv.Client(), nil)

	got := p.discoverRequiredTools(context.Background(), "opencode/1.18.31", "ses_0123456789ababcdefghijklmn")

	// The result must contain the genuinely required names and must not have
	// swollen to the whole pool because of the one dropped connection.
	for _, want := range []string{"bash", "read"} {
		if !contains(got, want) {
			t.Errorf("required set %v is missing %q", got, want)
		}
	}
	if len(got) >= len(p.ToolPool) {
		t.Errorf("required set has %d names, which is the whole pool — a transport "+
			"failure was likely read as a rejection: %v", len(got), got)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func containsAll(hay string, needles []string) bool {
	for _, n := range needles {
		found := false
		for i := 0; i+len(n) <= len(hay); i++ {
			if hay[i:i+len(n)] == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
