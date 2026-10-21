package zen

import (
	"encoding/json"
	"github.com/rian/mot-router/internal/gate"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestDeepSeekV2InferenceUsesCapturedChatContract(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if r.URL.Path != "/chat/completions" || b["model"] != "deepseek-v4.1-flash-free" || b["max_completion_tokens"] != float64(128) || b["max_tokens"] != nil || r.UserAgent() != "opencode/latest/2.0.29/cli" {
			t.Errorf("wrong V2 inference contract: %s %+v", r.URL.Path, b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"V2_OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()
	c := New(upstream.URL, "", "")
	c.Gate = &GateProfile{ClientFormat: gate.ClientV2, Identity: gate.DefaultIdentity(), UserAgent: "opencode/latest/2.0.29/cli"}
	events, status, err := c.Stream(t.Context(), "deepseek-v4.1-flash-free", "fixture", map[string]any{"max_output_tokens": 128, "input": []map[string]any{{"role": "user", "content": "hi"}}})
	if err != nil || status != 200 {
		t.Fatalf("stream failed: %d %v", status, err)
	}
	text := ""
	for event := range events {
		text += event.Delta
	}
	if text != "V2_OK" {
		t.Fatal(text)
	}
}

func TestVerifiedV2ProfileChangesInferenceHeadersAndCacheAffinity(t *testing.T) {
	c := New("", "", "")
	c.Gate = ProfileFromGate(&gate.Profile{ClientFormat: gate.ClientV2, Identity: gate.DefaultIdentity(), UserAgent: "opencode/latest/2.0.29/opencode"})
	a := c.Headers("test-free", "same-conversation")
	b := c.Headers("test-free", "same-conversation")
	if a["User-Agent"] != "opencode/latest/2.0.29/opencode" || a["x-opencode-request"] != "" || a["x-session-affinity"] == "" || a["x-session-affinity"] != b["x-session-affinity"] || a["x-opencode-client"] != "opencode" {
		t.Fatalf("V2 inference headers did not follow profile: %+v", a)
	}
	body := map[string]any{"model": "test-free"}
	out := withPromptCacheKey("test-free", body, a)
	if out["prompt_cache_key"] != a["x-opencode-session"] || body["prompt_cache_key"] != nil {
		t.Fatal("cache affinity mismatch or caller mutated")
	}
	if withPromptCacheKey("paid-model", body, a)["prompt_cache_key"] != nil {
		t.Fatal("free-tier adaptation leaked into paid body")
	}
}

func TestHeadersMatchInstalledV2LocalCapture(t *testing.T) {
	data, err := os.ReadFile("../../tools/diagnostics/results/opencode-v2-local-capture-glm-2026-10-17.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Requests []struct {
			Headers map[string]string `json:"headers"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(data, &capture); err != nil || len(capture.Requests) != 1 {
		t.Fatalf("missing capture: %v", err)
	}
	native := capture.Requests[0].Headers
	c := New("", "", "")
	c.Gate = &GateProfile{ClientFormat: gate.ClientV2, Identity: gate.DefaultIdentity(), UserAgent: native["user-agent"]}
	h := c.Headers("deepseek-v4.1-flash-free", "fixture")
	for _, key := range []string{"User-Agent", "Accept", "Content-Type", "x-opencode-client"} {
		lower := map[string]string{"User-Agent": "user-agent", "Accept": "accept", "Content-Type": "content-type", "x-opencode-client": "x-opencode-client"}[key]
		if h[key] != native[lower] {
			t.Errorf("%s does not match installed CLI", key)
		}
	}
	if h["x-opencode-request"] != "" || h["x-session-affinity"] != h["x-opencode-session"] {
		t.Fatal("V1 request header leaked into V2")
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(h["x-opencode-project"]) {
		t.Fatal("V2 project identifier does not match native format")
	}
	trace := strings.Split(h["traceparent"], "-")
	b3 := strings.Split(h["b3"], "-")
	if len(trace) != 4 || len(b3) != 4 || trace[1] != b3[0] || trace[2] != b3[1] || len(trace[1]) != 32 || len(trace[2]) != 16 {
		t.Fatal("trace correlation is missing or inconsistent")
	}
	if h["x-opencode-session"] != h["x-opencode-session-id"] || h["X-Session-Id"] != h["x-opencode-session"] {
		t.Fatal("V2 session aliases diverged")
	}
}

func TestHeadersMatchInstalledV1WireCapture(t *testing.T) {
	data, err := os.ReadFile("../../tools/diagnostics/opencode-current-headers.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Requests []struct {
			Model   string            `json:"model"`
			Headers map[string]string `json:"headers"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, r := range capture.Requests {
		if r.Model != "muse-spark-1.3-contributor-free" && r.Model != "exo-free" {
			continue
		}
		c := New("", r.Headers["user-agent"], "")
		h := c.Headers(r.Model, "capture-comparison")
		for _, pair := range [][2]string{{"User-Agent", "user-agent"}, {"Accept", "accept"}, {"Content-Type", "content-type"}, {"x-opencode-client", "x-opencode-client"}, {"x-opencode-project", "x-opencode-project"}} {
			if h[pair[0]] != r.Headers[pair[1]] {
				t.Errorf("%s: %s got %q want %q", r.Model, pair[0], h[pair[0]], r.Headers[pair[1]])
			}
		}
		if h["x-opencode-session"] != h["x-opencode-session-id"] || h["x-opencode-request"] == "" || h["x-session-affinity"] != "" {
			t.Fatal("native V1 identity headers mismatch")
		}
		checked++
	}
	if checked != 2 {
		t.Fatalf("missing endpoint capture: checked %d", checked)
	}
}

func TestV2AliasesPreserveClientToolContracts(t *testing.T) {
	shellParams := map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "timeout": map[string]any{"type": "number"}}, "required": []string{"command"}}
	taskParams := map[string]any{"type": "object", "properties": map[string]any{"prompt": map[string]any{"type": "string"}, "subagent_type": map[string]any{"type": "string"}}, "required": []string{"prompt", "subagent_type"}}
	tools, inj := PrepareTools([]Tool{{Name: "Bash", Parameters: shellParams}, {Name: "Task", Parameters: taskParams}}, &GateProfile{ClientFormat: gate.ClientV2, RequiredTools: []string{"shell", "subagent"}, AcceptsToolSuperset: true})
	expected := map[string]map[string]any{"shell": shellParams, "subagent": taskParams}
	for _, tool := range tools {
		if want, ok := expected[tool.Name]; ok && !reflect.DeepEqual(tool.Parameters, want) {
			t.Fatalf("%s changed client schema", tool.Name)
		}
	}
	for alias, original := range map[string]string{"shell": "Bash", "subagent": "Task"} {
		args := `{"prompt":"hi","subagent_type":"explore"}`
		if alias == "shell" {
			args = `{"command":"pwd","timeout":3}`
		}
		got, gotArgs, ok := RewriteToolName(alias, inj, args)
		if !ok || got != original || gotArgs != args {
			t.Fatalf("%s alias lost caller contract: %s %s %v", alias, got, gotArgs, ok)
		}
	}
}

func TestVerifiedEndpointOnlyChangesOfficialFreeInference(t *testing.T) {
	c := New(gate.LegacyBaseURL, "", "")
	c.Gate = &GateProfile{BaseURL: gate.DefaultBaseURL}
	if c.EffectiveBaseURL("muse-spark-1.3-contributor-free") != gate.DefaultBaseURL || c.EffectiveBaseURL("") != gate.DefaultBaseURL {
		t.Fatal("free inference or catalog kept rejected endpoint")
	}
	if c.EffectiveBaseURL("paid-model") != gate.LegacyBaseURL {
		t.Fatal("paid endpoint was changed")
	}
	c.BaseURL = "https://custom.example/v1"
	if c.EffectiveBaseURL("test-free") != c.BaseURL {
		t.Fatal("custom provider endpoint was overwritten")
	}
	c.BaseURL = gate.LegacyBaseURL
	c.Gate.BaseURL = "https://unverified.invalid/v1"
	if c.EffectiveBaseURL("test-free") != gate.LegacyBaseURL {
		t.Fatal("unknown endpoint was used")
	}
}

func TestVerifiedDescriptionsApplyWithoutChangingCallerSchemas(t *testing.T) {
	params := map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "timeout": map[string]any{"type": "integer"}}, "required": []string{"command"}}
	source := []Tool{{Name: "Bash", Description: "caller shell instructions", Parameters: params}, {Name: "OtherTool", Description: "keep me"}}
	profile := &GateProfile{ClientFormat: gate.ClientV1, RequiredTools: []string{"bash", "read"}, ToolDescriptions: map[string]string{"bash": gate.NativeToolDescription("bash"), "read": gate.NativeToolDescription("read")}, AcceptsToolSuperset: true}
	tools, inj := PrepareTools(source, profile)
	for _, tool := range tools {
		switch tool.Name {
		case "bash":
			if tool.Description != profile.ToolDescriptions["bash"] || !reflect.DeepEqual(tool.Parameters, params) {
				t.Fatal("shell description mismatch or caller schema changed")
			}
		case "read":
			if tool.Description != profile.ToolDescriptions["read"] {
				t.Fatal("injected description lost")
			}
			props := tool.Parameters["properties"].(map[string]any)
			if props["filePath"] == nil {
				t.Fatal("read schema missing")
			}
		case "othertool":
			if tool.Description != "keep me" {
				t.Fatal("unrelated tool changed")
			}
		}
	}
	if source[0].Description != "caller shell instructions" || inj.Originals["bash"] != "Bash" {
		t.Fatal("caller tools mutated or execution mapping lost")
	}
}
