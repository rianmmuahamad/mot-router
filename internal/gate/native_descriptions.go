package gate

import (
	_ "embed"
	"encoding/json"
)

const NativeV2ReferenceVersion = "2.0.29"

// Tool descriptions captured from the official, installed OpenCode 1.19.0
// with isolated storage and no external plugins or user skills. They contain
// no credentials or user conversation. No tool execution is provided here.
//
//go:embed native_descriptions_v1.json
var nativeDescriptionData []byte

// Public built-in tool definitions captured from installed @opencode/cli
// 2.0.29 against localhost, with isolated state and no user extensions.
//
//go:embed native_tools_v2.json
var nativeV2ToolData []byte

var nativeV2Tools = func() map[string]json.RawMessage {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(nativeV2ToolData, &data); err != nil {
		panic(err)
	}
	return data
}()

func nativeV2Tool(name string) map[string]any {
	data := nativeV2Tools[name]
	if len(data) == 0 {
		return nil
	}
	var tool map[string]any
	if err := json.Unmarshal(data, &tool); err != nil {
		return nil
	}
	tool["type"] = "function"
	return tool
}

var nativeDescriptions = func() map[string]string {
	var data map[string]string
	if err := json.Unmarshal(nativeDescriptionData, &data); err != nil {
		panic(err)
	}
	return data
}()

func NativeToolDescription(name string) string { return nativeDescriptions[name] }

func validToolDescriptions(descriptions map[string]string) bool {
	if len(descriptions) > 64 {
		return false
	}
	total := 0
	for name, desc := range descriptions {
		if len(name) == 0 || len(name) > 64 || len(desc) == 0 || len(desc) > 16384 {
			return false
		}
		total += len(desc)
	}
	return total <= 65536
}
