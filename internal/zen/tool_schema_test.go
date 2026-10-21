package zen

import (
	"encoding/json"
	"github.com/rian/mot-router/internal/gate"
	"reflect"
	"testing"
)

func TestV2SkillIDKeepsOriginalCallerArgument(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}
	native, aliases := nativeToolSchema("skill", gate.ClientV2, schema)
	if !reflect.DeepEqual(native["required"], []string{"id"}) || aliases["id"] != "name" {
		t.Fatalf("wrong V2 skill schema: %+v %+v", native, aliases)
	}
	if got := rewriteArgumentKeys(`{"name":"review"}`, aliases, true); got != `{"id":"review"}` {
		t.Fatal(got)
	}
	if got := rewriteArgumentKeys(`{"id":"review"}`, aliases, false); got != `{"name":"review"}` {
		t.Fatal(got)
	}
}

func TestClaudeSchemasAndArgumentsRoundTripThroughNativeTools(t *testing.T) {
	params := map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "old_string": map[string]any{"type": "string"}, "new_string": map[string]any{"type": "string"}}, "required": []string{"file_path", "old_string", "new_string"}, "additionalProperties": false}
	before, _ := json.Marshal(params)
	profile := &GateProfile{ClientFormat: gate.ClientV1, RequiredTools: []string{"edit"}, ToolDescriptions: map[string]string{"edit": gate.NativeToolDescription("edit")}, AcceptsToolSuperset: true}
	tools, inj := PrepareTools([]Tool{{Name: "Edit", Parameters: params}}, profile)
	props := tools[0].Parameters["properties"].(map[string]any)
	if props["filePath"] == nil || props["oldString"] == nil || props["newString"] == nil || props["file_path"] != nil || !reflect.DeepEqual(tools[0].Parameters["required"], []string{"filePath", "oldString", "newString"}) {
		t.Fatal("native schema aliases not normalized")
	}
	name, args, ok := RewriteToolName("edit", inj, `{"filePath":"/tmp/a","oldString":"old","newString":"new"}`)
	var caller map[string]any
	json.Unmarshal([]byte(args), &caller)
	if !ok || name != "Edit" || caller["file_path"] != "/tmp/a" || caller["old_string"] != "old" || caller["new_string"] != "new" || caller["filePath"] != nil {
		t.Fatal("caller argument contract was not restored")
	}
	items := []map[string]any{{"type": "function_call", "name": "edit", "call_id": "call_1", "arguments": args}, {"type": "function_call_output", "call_id": "call_1", "output": "done"}}
	NormalizeToolHistory(items, inj)
	var upstream map[string]any
	json.Unmarshal([]byte(items[0]["arguments"].(string)), &upstream)
	if upstream["filePath"] != "/tmp/a" || upstream["oldString"] != "old" || items[1]["output"] != "done" || items[0]["call_id"] != "call_1" {
		t.Fatal("history normalization lost arguments or pairing")
	}
	after, _ := json.Marshal(params)
	if string(before) != string(after) {
		t.Fatal("caller schema mutated")
	}
}
func TestPlaygroundEmptySchemasGainNativeDefinitions(t *testing.T) {
	body := map[string]any{"tools": []Tool{{Name: "read", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}}}
	out := withToolDescriptions("test-free", body, &GateProfile{ClientFormat: gate.ClientV1, ToolDescriptions: map[string]string{"read": gate.NativeToolDescription("read")}})
	props := out["tools"].([]Tool)[0].Parameters["properties"].(map[string]any)
	if props["filePath"] == nil {
		t.Fatal("empty playground schema still sent")
	}
	if len(body["tools"].([]Tool)[0].Parameters["properties"].(map[string]any)) != 0 {
		t.Fatal("playground source mutated")
	}
}
