package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestSendRotatesOnRateLimit is the regression guard for the bug where the
// prober sent every request directly. On the free tier the direct IP's quota is
// normally exhausted, so the baseline returned 429 and the whole probe gave up
// after one request — reporting "baseline rejected with 429" as if the gate had
// refused a known-good request.
func TestSendRotatesOnRateLimit(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First request is rate limited; the rotated one succeeds.
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"type":"error","error":{"type":"FreeUsageLimitError"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var rotations int32
	p := NewProber(srv.URL, "test-model")
	p.Configure(srv.Client(), func(context.Context) bool {
		atomic.AddInt32(&rotations, 1)
		return true
	})

	v := p.send(context.Background(), "opencode/1.18.31", "ses_0123456789ababcdefghijklmn", p.ToolPool, true)
	if !accepted(v) {
		t.Fatalf("expected the retry to be accepted, got status=%d err=%v", v.status, v.err)
	}
	if got := atomic.LoadInt32(&rotations); got != 1 {
		t.Errorf("expected exactly 1 rotation, got %d", got)
	}
}

// TestSendDoesNotRotateOnDefiniteRejection keeps rotation scoped: a 403 proves
// the request shape is wrong, which a different exit node cannot fix.
func TestSendDoesNotRotateOnDefiniteRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"type":"error","error":{"type":"FreeTierError"}}`))
	}))
	defer srv.Close()

	var rotations int32
	p := NewProber(srv.URL, "test-model")
	p.Configure(srv.Client(), func(context.Context) bool {
		atomic.AddInt32(&rotations, 1)
		return true
	})

	v := p.send(context.Background(), "mot-router/1.0", "ses_0123456789ababcdefghijklmn", p.ToolPool, true)
	if accepted(v) {
		t.Fatal("a 403 must not be read as acceptance")
	}
	if got := atomic.LoadInt32(&rotations); got != 0 {
		t.Errorf("a definite rejection must not rotate, got %d rotations", got)
	}
}

// TestSendWithoutRotationHookStillWorks covers the unproxied path: with no
// rotation configured the prober must still return the first verdict rather
// than blocking or panicking.
func TestSendWithoutRotationHookStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewProber(srv.URL, "test-model")
	p.Configure(nil, nil)

	v := p.send(context.Background(), "opencode/1.18.31", "ses_0123456789ababcdefghijklmn", p.ToolPool, true)
	if v.status != http.StatusTooManyRequests {
		t.Fatalf("expected the 429 to be returned as-is, got status=%d err=%v", v.status, v.err)
	}
	if inconclusive(v) != true {
		t.Error("a 429 must be reported as inconclusive, never as acceptance")
	}
}
