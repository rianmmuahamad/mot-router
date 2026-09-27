// Package translate converts between client protocols and the OpenAI Responses
// format that the Zen gateway actually serves.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rian/mot-router/internal/zen"
)

// AnthropicRequest is the subset of the Anthropic Messages API this router uses.
type AnthropicRequest struct {
	Model     string             `json:"model"`
	System    json.RawMessage    `json:"system"`
	Messages  []AnthropicMessage `json:"messages"`
	Tools     []AnthropicTool    `json:"tools"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
	Thinking  *struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking"`
}

// AnthropicMessage is one conversation turn.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// AnthropicTool is a tool declaration in Anthropic format.
type AnthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// AnthropicResult is the translated request body plus the tool-name mapping.
type AnthropicResult struct {
	Body    map[string]any
	ToolMap *zen.ToolInjection
}

// systemText flattens the system field, which may be a string or block array.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}

	parts := []string{}
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// AnthropicToZen translates an Anthropic Messages request into a Zen Responses
// request.
func AnthropicToZen(req *AnthropicRequest, model string, gate *zen.GateProfile) (*AnthropicResult, error) {
	items := []map[string]any{}

	for _, msg := range req.Messages {
		role := "user"
		if msg.Role == "assistant" {
			role = "assistant"
		}

		// Content may be a bare string.
		var plain string
		if err := json.Unmarshal(msg.Content, &plain); err == nil {
			items = append(items, textItem(role, plain))
			continue
		}

		var blocks []map[string]any
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			continue
		}

		parts := []map[string]any{}
		for _, block := range blocks {
			switch block["type"] {
			case "text":
				parts = append(parts, map[string]any{
					"type": textPartType(role),
					"text": asString(block["text"]),
				})

			case "image":
				source, _ := block["source"].(map[string]any)
				if asString(source["type"]) == "base64" {
					parts = append(parts, map[string]any{
						"type": "input_image",
						"image_url": fmt.Sprintf("data:%s;base64,%s",
							asString(source["media_type"]), asString(source["data"])),
					})
				}

			case "tool_use":
				if role != "assistant" {
					continue
				}
				input, _ := json.Marshal(block["input"])
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   asString(block["id"]),
					"name":      strings.ToLower(asString(block["name"])),
					"arguments": string(input),
				})

			case "tool_result":
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": asString(block["tool_use_id"]),
					"output":  flattenToolResult(block["content"]),
				})
			}
		}
		if len(parts) > 0 {
			items = append(items, map[string]any{"role": role, "content": parts})
		}
	}

	clientTools := make([]zen.Tool, 0, len(req.Tools))
	for _, t := range req.Tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		clientTools = append(clientTools, zen.Tool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		})
	}
	tools, toolMap := zen.PrepareTools(clientTools, gate)

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 32000
	}

	body := map[string]any{
		"model":             model,
		"input":             items,
		"max_output_tokens": maxTokens,
		"store":             false,
		"stream":            true,  // required by the free-tier gate
		"tools":             tools, // required by the free-tier gate
		"tool_choice":       "auto",
	}

	if instructions := systemText(req.System); instructions != "" {
		body["instructions"] = instructions
	}
	if req.Thinking != nil && req.Thinking.Type == "enabled" {
		// Budget is in tokens; upstream takes a named effort. The thresholds
		// give clients a way to reach every level the upstream accepts,
		// including xhigh — the highest it honours. `max` is rejected by
		// upstream (HTTP 400), so it is never produced here.
		effort := "medium"
		switch {
		case req.Thinking.BudgetTokens >= 32000:
			effort = "xhigh"
		case req.Thinking.BudgetTokens >= 16000:
			effort = "high"
		}
		body["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}

	return &AnthropicResult{Body: body, ToolMap: toolMap}, nil
}

func textPartType(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

func textItem(role, text string) map[string]any {
	return map[string]any{
		"role":    role,
		"content": []map[string]any{{"type": textPartType(role), "text": text}},
	}
}

func flattenToolResult(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, ok := content.([]any)
	if !ok {
		raw, _ := json.Marshal(content)
		return string(raw)
	}

	var sb strings.Builder
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok {
			sb.WriteString(asString(m["text"]))
		}
	}
	return sb.String()
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
