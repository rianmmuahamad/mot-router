package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNativeDescriptionsAreVerifiedAndPersisted(t *testing.T) {
	// The fixture asserts the flat Responses tool shape.
	t.Setenv("ZEN_RESPONSES_MODELS", "zen-fixture-free")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		found := map[string]bool{}
		for _, tool := range body.Tools {
			if native := NativeToolDescription(tool.Name); native != "" && tool.Description != native {
				w.WriteHeader(403)
				w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
				return
			}
			if tool.Name == "bash" || tool.Name == "read" {
				found[tool.Name] = tool.Description == NativeToolDescription(tool.Name)
			}
		}
		if !found["bash"] || !found["read"] {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
			return
		}
		w.Write([]byte(testProbeCompletion))
	}))
	defer upstream.Close()
	p := NewProber(upstream.URL, "zen-fixture-free")
	p.ToolPool = []string{"bash", "read", "edit"}
	profile := p.Probe(context.Background())
	if profile.Verdict != "ok" || profile.ToolDescriptions["bash"] != NativeToolDescription("bash") || profile.ToolDescriptions["read"] != NativeToolDescription("read") || profile.ToolDescriptions["edit"] != NativeToolDescription("edit") {
		t.Fatalf("descriptions unverified: %+v", profile)
	}
	c := NewCache(p, time.Hour)
	if !c.Restore(profile) {
		t.Fatal("verified descriptions not restored")
	}
	bad := *profile
	bad.ToolDescriptions = map[string]string{"bash": strings.Repeat("x", 65537)}
	if c.Restore(&bad) {
		t.Fatal("oversized description restored")
	}
}
