package zen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// UsesChatCompletions reports whether a model must be served by Zen's
// /chat/completions endpoint instead of /responses.
func UsesChatCompletions(model string) bool {
	low := strings.ToLower(model)
	if strings.Contains(low, "muse") {
		return false
	}
	return true
}

// ConvertToChatPayload adapts the Responses-style body into a standard OpenAI Chat Completions payload.
func ConvertToChatPayload(model string, body map[string]any) map[string]any {
	maxTokens := 262144
	if mt, ok := body["max_output_tokens"].(int); ok && mt > 0 {
		maxTokens = mt
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

			var text string
			if c, ok := it["content"].(string); ok {
				text = c
			} else if parts, ok := it["content"].([]map[string]any); ok {
				for _, p := range parts {
					if t, ok := p["text"].(string); ok {
						text += t
					}
				}
			}

			if role == "assistant" && len(msgs) > 0 && msgs[len(msgs)-1]["role"] == "assistant" {
				last := msgs[len(msgs)-1]
				if existingContent, ok := last["content"].(string); ok && existingContent != "" {
					last["content"] = existingContent + "\n" + text
				} else {
					last["content"] = text
				}
			} else {
				msgs = append(msgs, map[string]any{"role": role, "content": text})
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

	return chatPayload
}

// StreamChatCompletions sends a request to /zen/v1/chat/completions and decodes
// standard OpenAI chunk frames into Zen Event instances.
func (c *Client) StreamChatCompletions(ctx context.Context, model, sessionSeed string, body map[string]any) (<-chan Event, int, error) {
	chatBody := ConvertToChatPayload(model, body)
	payload, err := json.Marshal(chatBody)
	if err != nil {
		return nil, 0, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(payload))
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
		return nil, resp.StatusCode, fmt.Errorf("%s", strings.TrimSpace(string(detail)))
	}

	out := make(chan Event, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()

		type chatUsage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		}

		type rawDelta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}

		type rawChunk struct {
			Choices []struct {
				Delta        rawDelta `json:"delta"`
				FinishReason *string  `json:"finish_reason"`
			} `json:"choices"`
			Usage *chatUsage `json:"usage"`
		}

		type toolAccum struct {
			ID        string
			Name      string
			Arguments string
		}

		toolMap := make(map[int]*toolAccum)
		reasoningActive := false
		toolsEmitted := false
		var lastUsage *Usage

		emitToolsOnce := func() {
			if toolsEmitted {
				return
			}
			toolsEmitted = true
			for i := range len(toolMap) {
				if acc, ok := toolMap[i]; ok {
					out <- Event{
						Type: "response.output_item.done",
						Item: &Item{
							Type:      "function_call",
							CallID:    acc.ID,
							Name:      acc.Name,
							Arguments: acc.Arguments,
						},
					}
				}
			}
			toolMap = make(map[int]*toolAccum)
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimSpace(line[6:])
			if data == "[DONE]" {
				break
			}

			var chunk rawChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}

			if chunk.Usage != nil {
				inTok := chunk.Usage.PromptTokens
				outTok := chunk.Usage.CompletionTokens
				if inTok > 0 || outTok > 0 {
					lastUsage = &Usage{
						InputTokens:  inTok,
						OutputTokens: outTok,
					}
				}
			}

			for _, choice := range chunk.Choices {
				reasoningText := choice.Delta.ReasoningContent
				if reasoningText == "" {
					reasoningText = choice.Delta.Reasoning
				}
				if reasoningText != "" {
					reasoningActive = true
					out <- Event{
						Type:   "response.reasoning_summary_text.delta",
						Delta:  reasoningText,
						ItemID: "thinking",
					}
				}

				if choice.Delta.Content != "" {
					if reasoningActive {
						reasoningActive = false
						out <- Event{Type: "response.reasoning_summary_text.done"}
					}
					out <- Event{
						Type:   "response.output_text.delta",
						Delta:  choice.Delta.Content,
						ItemID: "msg_content",
					}
				}

				for _, tc := range choice.Delta.ToolCalls {
					acc, exists := toolMap[tc.Index]
					if !exists {
						acc = &toolAccum{}
						toolMap[tc.Index] = acc
					}
					if tc.ID != "" {
						acc.ID = tc.ID
					}
					if tc.Function.Name != "" {
						acc.Name += tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						acc.Arguments += tc.Function.Arguments
					}
				}

				if choice.FinishReason != nil && *choice.FinishReason != "" {
					if reasoningActive {
						reasoningActive = false
						out <- Event{Type: "response.reasoning_summary_text.done"}
					}
					emitToolsOnce()
				}
			}
		}

		if reasoningActive {
			out <- Event{Type: "response.reasoning_summary_text.done"}
		}
		emitToolsOnce()

		if lastUsage != nil {
			out <- Event{
				Type: "response.completed",
				Response: &struct {
					Usage *Usage `json:"usage"`
				}{Usage: lastUsage},
			}
		} else {
			out <- Event{Type: "response.completed"}
		}
	}()

	return out, resp.StatusCode, nil
}
