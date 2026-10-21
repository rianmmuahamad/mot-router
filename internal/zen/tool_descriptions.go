package zen

import "strings"

// Apply the verified description contract at the final outbound boundary too:
// playground and passthrough routes may not use PrepareTools. Parameters,
// names, unrelated tools and the caller's body remain intact.
func withToolDescriptions(model string, body map[string]any, profile *GateProfile) map[string]any {
	if !IsFreeModel(model) || profile == nil || len(profile.ToolDescriptions) == 0 {
		return body
	}
	lookup := func(name string) string { return profile.ToolDescriptions[strings.ToLower(name)] }
	copyTool := func(tool map[string]any) map[string]any {
		name, _ := tool["name"].(string)
		description := lookup(name)
		if description == "" {
			return tool
		}
		out := make(map[string]any, len(tool))
		for k, v := range tool {
			out[k] = v
		}
		out["description"] = description
		return out
	}
	var tools any
	switch list := body["tools"].(type) {
	case []Tool:
		out := append([]Tool(nil), list...)
		for i := range out {
			if description := lookup(out[i].Name); description != "" {
				out[i].Description = description
				if props, _ := out[i].Parameters["properties"].(map[string]any); len(props) == 0 {
					out[i].Parameters = gateToolParameters(strings.ToLower(out[i].Name), profile.ClientFormat)
				}
			}
		}
		tools = out
	case []any:
		out := append([]any(nil), list...)
		for i, item := range out {
			if tool, ok := item.(map[string]any); ok {
				if fn, ok := tool["function"].(map[string]any); ok {
					cloned := make(map[string]any, len(tool))
					for k, v := range tool {
						cloned[k] = v
					}
					cloned["function"] = copyTool(fn)
					out[i] = cloned
				} else {
					out[i] = copyTool(tool)
				}
			}
		}
		tools = out
	default:
		return body
	}
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out["tools"] = tools
	return out
}
