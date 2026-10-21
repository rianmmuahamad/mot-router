package zen

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestOutboundDescriptionContractCoversBypassRoutesAndExtraTools(t *testing.T) {
	profile := &GateProfile{ToolDescriptions: map[string]string{"edit": "verified edit description", "bash": "verified bash description"}}
	params := map[string]any{"type": "object", "required": []string{"file_path"}, "properties": map[string]any{"file_path": map[string]any{"type": "string"}}}
	original := []Tool{{Type: "function", Name: "edit", Description: "caller edit description", Parameters: params}, {Type: "function", Name: "mcp_ping", Description: "caller MCP description"}}
	body := map[string]any{"tools": original}
	out := withToolDescriptions("test-free", body, profile)
	tools := out["tools"].([]Tool)
	if tools[0].Description != profile.ToolDescriptions["edit"] || !reflect.DeepEqual(tools[0].Parameters, params) || tools[1].Description != "caller MCP description" {
		t.Fatal("outbound contract or unrelated tool was changed")
	}
	if original[0].Description != "caller edit description" {
		t.Fatal("caller tools mutated")
	}
	for _, nested := range []bool{false, true} {
		fn := map[string]any{"name": "edit", "description": "caller edit description", "parameters": params}
		tool := fn
		if nested {
			tool = map[string]any{"type": "function", "function": fn}
		}
		raw := map[string]any{"tools": []any{tool}}
		adapted := withToolDescriptions("test-free", raw, profile)
		mapped := adapted["tools"].([]any)[0].(map[string]any)
		if nested {
			mapped = mapped["function"].(map[string]any)
		}
		if mapped["description"] != profile.ToolDescriptions["edit"] || !reflect.DeepEqual(mapped["parameters"], params) || fn["description"] != "caller edit description" {
			t.Fatal("raw route lost schema or mutated source")
		}
	}
	paid := withToolDescriptions("paid-model", body, profile)
	encoded, _ := json.Marshal(paid)
	before, _ := json.Marshal(body)
	if string(encoded) != string(before) {
		t.Fatal("paid request was modified")
	}
}
