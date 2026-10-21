package zen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rian/mot-router/internal/gate"
)

func TestGLMStreamCannotMixProviderSnapshots(t *testing.T) {
	var reads atomic.Int32
	v1 := ProfileFromGate(gate.Fallback())
	v2 := ProfileFromGate(gate.Fallback())
	v2.ClientFormat, v2.UserAgent = gate.ClientV2, "opencode/latest/2.0.29/cli"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if r.UserAgent() != v1.UserAgent || b["max_tokens"] != float64(32) || b["max_completion_tokens"] != nil {
			t.Errorf("mixed stream snapshot: ua=%s", r.UserAgent())
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	c := New(upstream.URL, "", "")
	c.GateProvider = func() *GateProfile {
		if reads.Add(1) == 1 {
			return v1
		}
		return v2
	}
	ch, status, err := c.Stream(t.Context(), "glm-5.3-flash-free", "fixture", map[string]any{"max_output_tokens": 32})
	if err != nil || status != 200 {
		t.Fatal(status, err)
	}
	for range ch {
	}
	if reads.Load() != 1 {
		t.Fatalf("provider read %d times in one inference", reads.Load())
	}
}

func TestGateContextSnapshotCopiesMapsAndSupportsNil(t *testing.T) {
	p := &GateProfile{RequiredTools: []string{"read"}, ToolDescriptions: map[string]string{"read": "original"}}
	ctx := WithGateSnapshot(context.Background(), p)
	p.RequiredTools[0] = "changed"
	p.ToolDescriptions["read"] = "changed"
	snapshot := GateSnapshot(ctx, func() *GateProfile { t.Fatal("read latest provider"); return nil })
	if snapshot.RequiredTools[0] != "read" || snapshot.ToolDescriptions["read"] != "original" {
		t.Fatal("snapshot retained mutable fields")
	}
	if GateSnapshot(WithGateSnapshot(context.Background(), nil), func() *GateProfile { return p }) != nil {
		t.Fatal("nil snapshot incorrectly adopted latest provider")
	}
}
