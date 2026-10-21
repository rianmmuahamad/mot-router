package zen

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/rian/mot-router/internal/streamwire"
)

func decodeChatStream(ctx context.Context, resp *http.Response) <-chan Event {
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		type toolCall struct {
			Index    int    `json:"index"`
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		type delta struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			Reasoning        string     `json:"reasoning"`
			ToolCalls        []toolCall `json:"tool_calls"`
		}
		type chunk struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta        delta   `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Input  *int64 `json:"prompt_tokens"`
				Output *int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		tools := map[int]*Item{}
		totalArguments := 0
		terminal, thinking := false, false
		finish := "stop"
		var failure *StreamError
		var usage *Usage
		usageFinal := false
		err := streamwire.Read(resp.Body, 8*1024*1024, func(data []byte) error {
			if string(data) == "[DONE]" {
				terminal = true
				return streamwire.Stop
			}
			var c chunk
			if err := json.Unmarshal(data, &c); err != nil {
				return err
			}
			if failure = parseStreamError(c.Error); failure != nil {
				return streamwire.Stop
			}
			finalInChunk := terminal
			for _, choice := range c.Choices {
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					finalInChunk = true
				}
			}
			if c.Usage != nil && c.Usage.Input != nil && c.Usage.Output != nil && *c.Usage.Input >= 0 && *c.Usage.Output >= 0 {
				// Initial all-zero placeholders cannot establish measured usage.
				if *c.Usage.Input > 0 || *c.Usage.Output > 0 || (terminal && len(c.Choices) == 0) {
					if usage == nil {
						usage = &Usage{}
					}
					usage.InputTokens = max(usage.InputTokens, *c.Usage.Input)
					usage.OutputTokens = max(usage.OutputTokens, *c.Usage.Output)
					usageFinal = usageFinal || finalInChunk
				}
			}
			for _, choice := range c.Choices {
				d := choice.Delta
				reasoning := d.ReasoningContent
				if reasoning == "" {
					reasoning = d.Reasoning
				}
				if reasoning != "" {
					thinking = true
					if err := emitEvent(ctx, out, Event{Type: "response.reasoning_summary_text.delta", Delta: reasoning, ItemID: "thinking"}); err != nil {
						return err
					}
				}
				if d.Content != "" {
					if thinking {
						thinking = false
						if err := emitEvent(ctx, out, Event{Type: "response.reasoning_summary_text.done"}); err != nil {
							return err
						}
					}
					if err := emitEvent(ctx, out, Event{Type: "response.output_text.delta", Delta: d.Content, ItemID: "msg_content"}); err != nil {
						return err
					}
				}
				for _, tc := range d.ToolCalls {
					if tc.Index < 0 || tc.Index > 127 {
						failure = streamFailure("Invalid upstream tool index", false)
						return streamwire.Stop
					}
					item := tools[tc.Index]
					if item == nil {
						item = &Item{Type: "function_call"}
						tools[tc.Index] = item
					}
					if tc.ID != "" {
						item.CallID = tc.ID
					}
					item.Name += tc.Function.Name
					totalArguments += len(tc.Function.Arguments)
					item.Arguments += tc.Function.Arguments
					if totalArguments > 8*1024*1024 || len(item.Name) > 1024 {
						failure = streamFailure("Upstream tool arguments exceed 8 MiB", false)
						return streamwire.Stop
					}
				}
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					terminal = true
					finish = *choice.FinishReason
				}
			}
			return nil
		})
		if failure == nil && err != nil {
			failure = invalidStream(err)
		}
		if failure == nil && !terminal {
			failure = streamFailure("Upstream stream ended before a final event", true)
		}
		if failure != nil {
			_ = emitEvent(ctx, out, Event{Type: "error", Error: failure, StreamInterrupted: true})
			return
		}
		if thinking {
			if emitEvent(ctx, out, Event{Type: "response.reasoning_summary_text.done"}) != nil {
				return
			}
		}
		indices := make([]int, 0, len(tools))
		for index := range tools {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, index := range indices {
			if emitEvent(ctx, out, Event{Type: "response.output_item.done", Item: tools[index]}) != nil {
				return
			}
		}
		final := Event{Type: "response.completed", FinishReason: finish}
		if usage != nil && usageFinal {
			final.Response = &struct {
				Usage *Usage `json:"usage"`
			}{Usage: usage}
		}
		_ = emitEvent(ctx, out, final)
	}()
	return out
}
