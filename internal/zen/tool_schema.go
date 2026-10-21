package zen

import "encoding/json"

// Canonicalize recognized native parameter aliases only. The reverse map
// restores the caller's contract before a tool call reaches its executor.
func nativeToolSchema(name, format string, schema map[string]any) (map[string]any, map[string]string) {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return gateToolParameters(name, format), nil
	}
	path := "filePath"
	if format == "v2" {
		path = "path"
	}
	candidates := map[string][]string{}
	switch name {
	case "read", "edit", "write":
		candidates[path] = []string{"file_path", "path", "filepath", "file", "filename"}
	case "bash", "shell":
		candidates["command"] = []string{"cmd", "script", "command_line", "commandLine", "input", "code"}
	case "skill":
		if format == "v2" {
			candidates["id"] = []string{"name"}
		}
	}
	if name == "edit" {
		candidates["oldString"] = []string{"old_string", "old_text"}
		candidates["newString"] = []string{"new_string", "new_text"}
		candidates["replaceAll"] = []string{"replace_all"}
	}
	if name == "write" {
		candidates["content"] = []string{"text"}
	}
	aliases := map[string]string{}
	cloned := make(map[string]any, len(props))
	for k, v := range props {
		cloned[k] = v
	}
	for canonical, names := range candidates {
		if _, exists := cloned[canonical]; exists {
			continue
		}
		for _, original := range names {
			if value, exists := cloned[original]; exists {
				cloned[canonical] = value
				delete(cloned, original)
				aliases[canonical] = original
				break
			}
		}
	}
	if len(aliases) == 0 {
		return schema, nil
	}
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		out[k] = v
	}
	out["properties"] = cloned
	required := []string{}
	switch list := schema["required"].(type) {
	case []string:
		required = append(required, list...)
	case []any:
		for _, v := range list {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	if schema["required"] != nil {
		for i, name := range required {
			for canonical, original := range aliases {
				if name == original {
					required[i] = canonical
				}
			}
		}
		out["required"] = required
	}
	return out, aliases
}

func rewriteArgumentKeys(args string, aliases map[string]string, upstream bool) string {
	if len(aliases) == 0 {
		return args
	}
	var values map[string]any
	if json.Unmarshal([]byte(args), &values) != nil || values == nil {
		return args
	}
	for canonical, original := range aliases {
		from, to := canonical, original
		if upstream {
			from, to = original, canonical
		}
		if value, exists := values[from]; exists {
			if _, present := values[to]; !present {
				values[to] = value
			}
			delete(values, from)
		}
	}
	data, err := json.Marshal(values)
	if err != nil {
		return args
	}
	return string(data)
}

// History returns through the client in its original schema; normalize it
// again for the next upstream turn without changing call IDs or tool outputs.
func NormalizeToolHistory(items []map[string]any, injection *ToolInjection) {
	if injection == nil {
		return
	}
	for _, item := range items {
		if item["type"] != "function_call" {
			continue
		}
		name, _ := item["name"].(string)
		args, ok := item["arguments"].(string)
		if ok {
			item["arguments"] = rewriteArgumentKeys(args, injection.ArgumentAliases[name], true)
		}
	}
}
