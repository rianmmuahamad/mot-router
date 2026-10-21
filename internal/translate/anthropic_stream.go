package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/rian/mot-router/internal/zen"
)

// AnthropicStreamWriter emits Anthropic Messages SSE. It owns content-block
// index numbering so callers never have to track it.
type AnthropicStreamWriter struct {
	w         io.Writer
	model     string
	msgID     string
	index     int
	openIndex int
	usedTool  bool
	// thinkingOpen tracks whether the currently open block is a thinking block,
	// so a following text block starts its own instead of appending to it.
	thinkingOpen  bool
	inputTokens   int64
	outputTokens  int64
	measured      bool
	finishReason  string
	textIndexByID map[string]int
}

// NewAnthropicStreamWriter prepares a writer for one response.
func NewAnthropicStreamWriter(w io.Writer, model string) *AnthropicStreamWriter {
	return &AnthropicStreamWriter{
		w:             w,
		model:         model,
		msgID:         "msg_" + randomHex(12),
		index:         -1,
		openIndex:     -1,
		textIndexByID: map[string]int{},
	}
}

// Usage returns token counts observed from the upstream stream.
func (s *AnthropicStreamWriter) Usage() (input, output int64) {
	return s.inputTokens, s.outputTokens
}

func (s *AnthropicStreamWriter) emit(event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, payload)
	if f, ok := s.w.(interface{ Flush() }); ok {
		f.Flush()
	}
}

func (s *AnthropicStreamWriter) closeOpen() {
	if s.openIndex >= 0 {
		s.emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": s.openIndex,
		})
		s.openIndex = -1
		s.thinkingOpen = false
	}
}

// Start emits message_start.
func (s *AnthropicStreamWriter) Start() {
	s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// Text streams a text delta, opening a block on first use for the item.
func (s *AnthropicStreamWriter) Text(itemID, delta string) {
	idx, seen := s.textIndexByID[itemID]
	if !seen {
		s.closeOpen()
		s.index++
		idx = s.index
		s.openIndex = idx
		s.textIndexByID[itemID] = idx
		s.emit("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         idx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": "text_delta", "text": delta},
	})
}

// EndText closes the currently open text block.
func (s *AnthropicStreamWriter) EndText() { s.closeOpen() }

// Thinking streams a reasoning summary as an Anthropic thinking block.
//
// Zen reports reasoning as reasoning_summary_text.delta events. Anthropic's
// surface has a first-class block type for it, so dropping those events lost
// the model's reasoning on this surface entirely.
func (s *AnthropicStreamWriter) Thinking(itemID, delta string) {
	if !s.thinkingOpen {
		s.closeOpen()
		s.index++
		s.openIndex = s.index
		s.thinkingOpen = true
		s.emit("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         s.index,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		})
	}
	s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
	})
}

// EndThinking closes the thinking block so the answer starts a new one.
func (s *AnthropicStreamWriter) EndThinking() {
	if s.thinkingOpen {
		s.closeOpen()
		s.thinkingOpen = false
	}
}

// Tool emits a complete tool_use block.
func (s *AnthropicStreamWriter) Tool(id, name, args string) {
	s.closeOpen()
	s.index++
	s.usedTool = true
	s.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": s.index,
		"content_block": map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
		},
	})
	s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	})
	s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index})
}

// SetUsage records upstream token counts.
func (s *AnthropicStreamWriter) SetUsage(input, output int64) {
	s.inputTokens = input
	s.outputTokens = output
	s.measured = true
}

// Finish emits message_delta and message_stop.
func (s *AnthropicStreamWriter) Finish() {
	s.closeOpen()
	stopReason := "end_turn"
	if s.usedTool {
		stopReason = "tool_use"
	}
	if s.finishReason == "length" {
		stopReason = "max_tokens"
	}
	if s.finishReason == "content_filter" {
		stopReason = "refusal"
	}
	final := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		// Motgate quota accounting requires a complete input/output pair in
		// the final usage event. The initial message_start usage is only a
		// placeholder and cannot be used for billing.
	}
	if s.measured {
		final["usage"] = map[string]any{"input_tokens": s.inputTokens, "output_tokens": s.outputTokens}
	}
	s.emit("message_delta", final)
	s.emit("message_stop", map[string]any{"type": "message_stop"})
}

// StreamAnthropic translates a Zen event channel into an Anthropic SSE stream.
func StreamAnthropic(events <-chan zen.Event, w *AnthropicStreamWriter, toolMap *zen.ToolInjection) {
	w.Start()
	failed := false

	for ev := range events {
		if failed {
			continue
		}
		if failure := eventFailure(ev); failure != nil {
			failed = true
			w.closeOpen()
			w.emit("error", map[string]any{"type": "error", "error": failure})
			continue
		}
		if ev.FinishReason != "" {
			w.finishReason = ev.FinishReason
		}
		switch ev.Type {
		case "response.reasoning_summary_text.delta":
			itemID := ev.ItemID
			if itemID == "" {
				itemID = "thinking"
			}
			w.Thinking(itemID, ev.Delta)

		case "response.reasoning_summary_text.done":
			w.EndThinking()

		case "response.output_text.delta":
			// The answer must not be appended to the thinking block.
			w.EndThinking()
			itemID := ev.ItemID
			if itemID == "" {
				itemID = "text"
			}
			w.Text(itemID, ev.Delta)

		case "response.output_text.done":
			w.EndText()

		case "response.output_item.done":
			if ev.Item == nil || ev.Item.Type != "function_call" {
				continue
			}
			low := lower(ev.Item.Name)
			callID := ev.Item.CallID
			if callID == "" {
				callID = ev.Item.ID
			}
			name, args, ok := zen.RewriteToolName(low, toolMap, ev.Item.Arguments)
			if !ok {
				// The tool was injected by the router and the client has no
				// equivalent, so forwarding it would hand the client a call it
				// cannot execute. Dropping it is safer than leaking it.
				continue
			}
			w.Tool(callID, name, args)

		case "response.completed", "response.incomplete":
			if ev.Response != nil && ev.Response.Usage != nil {
				w.SetUsage(ev.Response.Usage.InputTokens, ev.Response.Usage.OutputTokens)
			}
		}
	}

	if !failed {
		w.Finish()
	}
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(buf)
}
