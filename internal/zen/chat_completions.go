package zen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/rian/mot-router/internal/gate"
)

// UsesChatCompletions reports whether a model must be served by Zen's
// /chat/completions endpoint instead of /responses.
func UsesChatCompletions(model string) bool {
	return gate.UsesChatCompletions(model)
}

// ConvertToChatPayload adapts the Responses-style body into a standard OpenAI Chat Completions payload.
func ConvertToChatPayload(model string, body map[string]any) map[string]any {
	var maxTokens any = 32000
	if value, ok := body["max_output_tokens"]; ok {
		maxTokens = value
	}

	chatPayload := map[string]any{
		"model":          model,
		"max_tokens":     maxTokens,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}

	if rawInput, ok := body["input"].([]map[string]any); ok {
		var msgs []map[string]any
		for _, it := range rawInput {
			role, _ := it["role"].(string)
			if role == "" {
				role = "user"
			}
			if role == "developer" {
				role = "system"
			}

			if t, _ := it["type"].(string); t == "function_call_output" {
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": it["call_id"],
					"content":      it["output"],
				})
				continue
			}

			if t, _ := it["type"].(string); t == "function_call" {
				call := map[string]any{
					"id":   it["call_id"],
					"type": "function",
					"function": map[string]any{
						"name":      it["name"],
						"arguments": it["arguments"],
					},
				}
				if len(msgs) > 0 && msgs[len(msgs)-1]["role"] == "assistant" {
					last := msgs[len(msgs)-1]
					if existingCalls, ok := last["tool_calls"].([]map[string]any); ok {
						last["tool_calls"] = append(existingCalls, call)
					} else {
						last["tool_calls"] = []map[string]any{call}
					}
				} else {
					msgs = append(msgs, map[string]any{
						"role":       "assistant",
						"tool_calls": []map[string]any{call},
					})
				}
				continue
			}

			content := chatContent(it["content"])
			if role == "assistant" && len(msgs) > 0 && msgs[len(msgs)-1]["role"] == "assistant" {
				last := msgs[len(msgs)-1]
				if previous, ok := last["content"].(string); ok && previous != "" {
					if text, ok := content.(string); ok {
						last["content"] = previous + "\n" + text
					} else {
						last["content"] = append([]map[string]any{{"type": "text", "text": previous}}, content.([]map[string]any)...)
					}
				} else {
					last["content"] = content
				}
			} else {
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			}

		}
		chatPayload["messages"] = msgs
	}

	var chatTools []map[string]any
	if toolsList, ok := body["tools"].([]Tool); ok {
		for _, t := range toolsList {
			chatTools = append(chatTools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
	} else if rawTools, ok := body["tools"].([]any); ok {
		for _, raw := range rawTools {
			if tm, ok := raw.(map[string]any); ok {
				name, _ := tm["name"].(string)
				desc, _ := tm["description"].(string)
				params := tm["parameters"]
				if params == nil {
					params = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				chatTools = append(chatTools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        name,
						"description": desc,
						"parameters":  params,
					},
				})
			}
		}
	}
	if len(chatTools) > 0 {
		chatPayload["tools"] = chatTools
		chatPayload["tool_choice"] = "auto"
	}

	for _, key := range []string{"temperature", "top_p", "stop", "response_format", "parallel_tool_calls", "seed"} {
		if value, ok := body[key]; ok {
			chatPayload[key] = value
		}
	}
	if value, ok := body["tool_choice"]; ok {
		if named, ok := value.(map[string]any); ok && named["type"] == "function" && named["name"] != nil {
			value = map[string]any{"type": "function", "function": map[string]any{"name": named["name"]}}
		}
		chatPayload["tool_choice"] = value
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok && reasoning["effort"] != nil {
		chatPayload["reasoning_effort"] = reasoning["effort"]
	}
	return chatPayload
}

// StreamChatCompletions sends a request to /zen/v1/chat/completions and decodes
// standard OpenAI chunk frames into Zen Event instances.
func (c *Client) StreamChatCompletions(ctx context.Context, model, sessionSeed string, body map[string]any) (<-chan Event, int, error) {
	c = c.requestClient(ctx)
	body = withToolDescriptions(model, body, c.CurrentGate())
	chatBody := ConvertToChatPayload(model, body)
	if profile := c.CurrentGate(); profile != nil && profile.ClientFormat == gate.ClientV2 {
		// V2's native Chat client sends this parameter, including DeepSeek.
		chatBody["max_completion_tokens"] = chatBody["max_tokens"]
		delete(chatBody, "max_tokens")
	}
	payload, err := json.Marshal(chatBody)
	if err != nil {
		return nil, 0, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.EffectiveBaseURL(model)+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	for k, v := range c.Headers(model, sessionSeed) {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("call upstream: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, resp.StatusCode, upstreamError(resp, detail)
	}

	return decodeChatStream(ctx, resp), resp.StatusCode, nil
}

// Text-only messages remain strings. Mixed content retains its image URL and detail.
func chatContent(value any) any {
	if text, ok := value.(string); ok {
		return text
	}
	var parts []map[string]any
	switch v := value.(type) {
	case []map[string]any:
		parts = v
	case []any:
		for _, p := range v {
			if part, ok := p.(map[string]any); ok {
				parts = append(parts, part)
			}
		}
	}
	converted := []map[string]any{}
	text := ""
	mixed := false
	for _, part := range parts {
		switch part["type"] {
		case "input_text", "output_text", "text":
			if t, ok := part["text"].(string); ok {
				text += t
				converted = append(converted, map[string]any{"type": "text", "text": t})
			}
		case "input_image":
			image := map[string]any{"url": part["image_url"]}
			if detail, ok := part["detail"].(string); ok && detail != "" {
				image["detail"] = detail
			}
			converted = append(converted, map[string]any{"type": "image_url", "image_url": image})
			mixed = true
		case "image_url":
			converted = append(converted, part)
			mixed = true
		}
	}
	if mixed {
		return converted
	}
	return text
}
