package translate

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/rian/mot-router/internal/zen"
)

// OpenAIChatRequest is the subset of the OpenAI Chat Completions API used here.
type OpenAIChatRequest struct {
	Model               string          `json:"model"`
	Messages            []OpenAIMessage `json:"messages"`
	Tools               []OpenAITool    `json:"tools"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	Stream              *bool           `json:"stream"`
}

// OpenAIMessage is one conversation turn.
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`
}

// OpenAITool is a tool declaration in OpenAI format.
type OpenAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// OpenAIToolCall is a tool invocation requested by the model.
type OpenAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// OpenAIResult is the translated request body plus the tool-name mapping.
type OpenAIResult struct {
	Body    map[string]any
	ToolMap *zen.ToolInjection
}

// OpenAIToZen translates a Chat Completions request into a Zen Responses request.
func OpenAIToZen(req *OpenAIChatRequest, model string, gate *zen.GateProfile) (*OpenAIResult, error) {
	items := []map[string]any{}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system", "developer":
			items = append(items, map[string]any{
				"role":    "developer",
				"content": contentToString(msg.Content),
			})
			continue

		case "tool":
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": msg.ToolCallID,
				"output":  contentToString(msg.Content),
			})
			continue
		}

		role := "user"
		if msg.Role == "assistant" {
			role = "assistant"
		}

		// Assistant turns may carry tool calls alongside (or instead of) text.
		if len(msg.ToolCalls) > 0 {
			for _, call := range msg.ToolCalls {
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   call.ID,
					"name":      lower(call.Function.Name),
					"arguments": call.Function.Arguments,
				})
			}
			if text := contentToString(msg.Content); text != "" {
				items = append(items, map[string]any{
					"role":    "assistant",
					"content": []map[string]any{{"type": "output_text", "text": text}},
				})
			}
			continue
		}

		// Plain string content.
		var plain string
		if err := json.Unmarshal(msg.Content, &plain); err == nil {
			items = append(items, textItem(role, plain))
			continue
		}

		// Structured content parts.
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
			case "image_url":
				url, _ := block["image_url"].(map[string]any)
				parts = append(parts, map[string]any{
					"type":      "input_image",
					"image_url": asString(url["url"]),
				})
			}
		}
		if len(parts) > 0 {
			items = append(items, map[string]any{"role": role, "content": parts})
		}
	}

	clientTools := make([]zen.Tool, 0, len(req.Tools))
	for _, t := range req.Tools {
		params := t.Function.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		clientTools = append(clientTools, zen.Tool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  params,
		})
	}
	tools, toolMap := zen.PrepareTools(clientTools, gate)

	maxTokens := req.MaxCompletionTokens
	if maxTokens == 0 {
		maxTokens = req.MaxTokens
	}
	if maxTokens == 0 {
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
	if req.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": req.ReasoningEffort, "summary": "auto"}
	}

	return &OpenAIResult{Body: body, ToolMap: toolMap}, nil
}

func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	out := ""
	for _, b := range blocks {
		out += asString(b["text"])
	}
	return out
}

// OpenAIToolCallDelta is the streaming representation of a tool call.
type OpenAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// OpenAIUsage is the token accounting block.
type OpenAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// OpenAIChunk is one streaming frame.
type OpenAIChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int `json:"index"`
		Delta        any `json:"delta"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
	Usage *OpenAIUsage `json:"usage,omitempty"`
}

type openAIDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// ReasoningContent carries the model's chain-of-thought summary.
	//
	// Zen emits it as separate reasoning_summary_text.delta events. Dropping
	// them silently discarded the model's reasoning: clients that display it
	// (DeepSeek-style UIs, several agent harnesses) showed nothing. The field
	// name follows the convention those clients already parse.
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCallDelta `json:"tool_calls,omitempty"`
}

func newChunk(id, model string, created int64, delta any, finish any) OpenAIChunk {
	c := OpenAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
	}
	c.Choices = []struct {
		Index        int `json:"index"`
		Delta        any `json:"delta"`
		FinishReason any `json:"finish_reason"`
	}{{Index: 0, Delta: delta, FinishReason: finish}}
	return c
}

// StreamOpenAI translates a Zen event channel into Chat Completions SSE chunks and writes them directly to w.
// Chunks are flushed immediately as events arrive from upstream, preventing client-side first-event timeouts.
func StreamOpenAI(w io.Writer, events <-chan zen.Event, model string, toolMap *zen.ToolInjection) *OpenAIUsage {
	id := "chatcmpl-" + randomHex(12)
	created := time.Now().Unix()

	flusher, _ := w.(interface{ Flush() })
	emitChunk := func(delta openAIDelta, finishReason any, usage *OpenAIUsage) {
		chunk := newChunk(id, model, created, delta, finishReason)
		chunk.Usage = usage
		payload, err := json.Marshal(chunk)
		if err == nil {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}

	roleSent := false
	var finishReason any = nil
	var usage *OpenAIUsage

	for ev := range events {
		switch ev.Type {
		case "response.reasoning_summary_text.delta":
			delta := openAIDelta{ReasoningContent: ev.Delta}
			if !roleSent {
				delta.Role = "assistant"
				roleSent = true
			}
			emitChunk(delta, nil, nil)

		case "response.output_text.delta":
			delta := openAIDelta{Content: ev.Delta}
			if !roleSent {
				delta.Role = "assistant"
				roleSent = true
			}
			emitChunk(delta, nil, nil)

		case "response.output_item.done":
			if ev.Item == nil || ev.Item.Type != "function_call" {
				continue
			}
			if !roleSent {
				roleSent = true
				emitChunk(openAIDelta{Role: "assistant"}, nil, nil)
			}
			callID := ev.Item.CallID
			if callID == "" {
				callID = ev.Item.ID
			}
			name, args, ok := zen.RewriteToolName(lower(ev.Item.Name), toolMap, ev.Item.Arguments)
			if !ok {
				continue
			}

			call := OpenAIToolCallDelta{Index: 0, ID: callID, Type: "function"}
			call.Function.Name = name
			call.Function.Arguments = args
			finishReason = "tool_calls"
			emitChunk(openAIDelta{ToolCalls: []OpenAIToolCallDelta{call}}, nil, nil)

		case "response.completed":
			if ev.Response != nil && ev.Response.Usage != nil {
				usage = &OpenAIUsage{
					PromptTokens:     ev.Response.Usage.InputTokens,
					CompletionTokens: ev.Response.Usage.OutputTokens,
					TotalTokens:      ev.Response.Usage.InputTokens + ev.Response.Usage.OutputTokens,
				}
			}
		}
	}

	if finishReason == nil {
		finishReason = "stop"
	}
	emitChunk(openAIDelta{}, finishReason, usage)

	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	return usage
}

// CollectOpenAI aggregates a Zen stream into a non-streaming completion body.
func CollectOpenAI(events <-chan zen.Event, model string, toolMap *zen.ToolInjection) map[string]any {
	text := ""
	reasoning := ""
	toolCalls := []map[string]any{}
	var usage *OpenAIUsage

	for ev := range events {
		switch ev.Type {
		case "response.reasoning_summary_text.delta":
			reasoning += ev.Delta

		case "response.output_text.delta":
			text += ev.Delta

		case "response.output_item.done":
			if ev.Item == nil || ev.Item.Type != "function_call" {
				continue
			}
			callID := ev.Item.CallID
			if callID == "" {
				callID = ev.Item.ID
			}
			name, args, ok := zen.RewriteToolName(lower(ev.Item.Name), toolMap, ev.Item.Arguments)
			if !ok {
				// Injected by the router with no client equivalent: drop it rather
				// than hand the client a tool it never declared.
				continue
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			})

		case "response.completed":
			if ev.Response != nil && ev.Response.Usage != nil {
				usage = &OpenAIUsage{
					PromptTokens:     ev.Response.Usage.InputTokens,
					CompletionTokens: ev.Response.Usage.OutputTokens,
					TotalTokens:      ev.Response.Usage.InputTokens + ev.Response.Usage.OutputTokens,
				}
			}
		}
	}

	var content any = text
	if text == "" {
		content = nil
	}
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
	}

	if usage == nil {
		usage = &OpenAIUsage{}
	}

	return map[string]any{
		"id":      "chatcmpl-" + randomHex(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": usage,
	}
}
