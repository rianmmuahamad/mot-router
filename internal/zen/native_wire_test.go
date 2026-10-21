package zen

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rian/mot-router/internal/gate"
)

// Check the final HTTP request against the installed CLI's actual public
// tool contract, rather than checking the body builder against itself.
func TestFinalV2WireMatchesInstalledNativeContract(t *testing.T) {
	data, err := os.ReadFile("../../tools/diagnostics/results/opencode-v2-tool-schemas.json")
	if err != nil {
		t.Fatal(err)
	}
	var native []struct {
		Function struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(data, &native); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"glm-5.3-flash-free", "deepseek-v4.1-flash-free"} {
		t.Run(model, func(t *testing.T) {
			profile := &GateProfile{ClientFormat: gate.ClientV2, UserAgent: "opencode/latest/2.0.29/cli", Identity: gate.DefaultIdentity(), ToolDescriptions: map[string]string{}}
			for _, tool := range native {
				profile.RequiredTools = append(profile.RequiredTools, tool.Function.Name)
				profile.ToolDescriptions[tool.Function.Name] = tool.Function.Description
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if r.URL.Path != "/v1/chat/completions" || body["max_completion_tokens"] != float64(64) || body["max_tokens"] != nil || body["model"] != model {
					t.Errorf("wrong endpoint or output parameter: %s", r.URL.Path)
				}
				for _, name := range []string{"b3", "traceparent", "x-opencode-project", "x-opencode-client", "x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id"} {
					if r.Header.Get(name) == "" {
						t.Errorf("missing native application header %s", name)
					}
				}
				if r.UserAgent() != profile.UserAgent || r.Header.Get("x-opencode-request") != "" || r.Header.Get("Accept") != "*/*" || r.Header.Get("Authorization") != "Bearer public" {
					t.Error("application header contract differs from native V2")
				}
				actual := map[string]map[string]any{}
				for _, tool := range body["tools"].([]any) {
					fn := tool.(map[string]any)["function"].(map[string]any)
					actual[fn["name"].(string)] = fn
				}
				if len(actual) != len(native) {
					t.Fatal("native roster differs")
				}
				for _, tool := range native {
					fn := actual[tool.Function.Name]
					if fn["description"] != tool.Function.Description || !reflect.DeepEqual(fn["parameters"], tool.Function.Parameters) {
						t.Errorf("native public definition differs for %s", tool.Function.Name)
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"WIRE_OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			}))
			defer upstream.Close()
			c := New(upstream.URL+"/v1", "", "")
			c.Gate = profile
			tools, _ := PrepareTools(nil, profile)
			events, _, err := c.Stream(t.Context(), model, "wire-audit", map[string]any{"max_output_tokens": 64, "tools": tools, "input": []map[string]any{{"role": "user", "content": "hi"}}})
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			for event := range events {
				text.WriteString(event.Delta)
			}
			if text.String() != "WIRE_OK" {
				t.Fatal("native-shaped request lost its response")
			}
		})
	}
}
