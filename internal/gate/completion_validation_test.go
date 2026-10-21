package gate

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestProbeRejectsHTTP200WithoutCompletedOutput(t *testing.T) {
	for _, body := range []string{"data: {}\n\n", "data: {\"error\":{\"message\":\"fixture failure\"}}\n\n", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"} {
		var hits atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte(body)) }))
		p := NewProber(up.URL, "fixture-model").Probe(t.Context())
		up.Close()
		if p.Verdict == "ok" || p.Conditions[0].OK || hits.Load() != 1 {
			t.Fatalf("false success: %+v calls=%d", p, hits.Load())
		}
	}
}
