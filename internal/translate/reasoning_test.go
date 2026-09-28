package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rian/mot-router/internal/zen"
)

// reasoningEvents is a Zen stream shaped like the real one: the reasoning
// summary arrives first, then the answer text.
func reasoningEvents() <-chan zen.Event {
	ch := make(chan zen.Event, 4)
	ch <- zen.Event{Type: "response.reasoning_summary_text.delta", Delta: "menghitung 17x23", ItemID: "rs_1"}
	ch <- zen.Event{Type: "response.reasoning_summary_text.done"}
	ch <- zen.Event{Type: "response.output_text.delta", Delta: "391", ItemID: "msg_1"}
	close(ch)
	return ch
}

// TestCollectOpenAIExposesReasoning is the regression guard for reasoning being
// discarded. Zen emits reasoning_summary_text.delta events; the router only
// handled output_text.delta, so a client that asks for reasoning received none.
func TestCollectOpenAIExposesReasoning(t *testing.T) {
	body := CollectOpenAI(reasoningEvents(), "test-model", nil)

	choices, ok := body["choices"].([]map[string]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("unexpected choices shape: %T", body["choices"])
	}
	msg, ok := choices[0]["message"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected message shape: %T", choices[0]["message"])
	}

	if got, _ := msg["reasoning_content"].(string); got != "menghitung 17x23" {
		t.Errorf("reasoning_content = %q, want %q", got, "menghitung 17x23")
	}
	if got, _ := msg["content"].(string); got != "391" {
		t.Errorf("content = %q, want %q", got, "391")
	}
}

// TestCollectOpenAIOmitsReasoningWhenAbsent keeps the field out of responses
// that have no reasoning, so clients do not see an empty property.
func TestCollectOpenAIOmitsReasoningWhenAbsent(t *testing.T) {
	ch := make(chan zen.Event, 1)
	ch <- zen.Event{Type: "response.output_text.delta", Delta: "hi"}
	close(ch)

	body := CollectOpenAI(ch, "test-model", nil)
	choices := body["choices"].([]map[string]any)
	msg := choices[0]["message"].(map[string]any)
	if _, present := msg["reasoning_content"]; present {
		t.Error("reasoning_content must be absent when there is no reasoning")
	}
}

// TestStreamOpenAIEmitsReasoningBeforeContent checks ordering: a client renders
// reasoning as it arrives, so it must precede the answer.
func TestStreamOpenAIEmitsReasoningBeforeContent(t *testing.T) {
	var buf bytes.Buffer
	StreamOpenAI(&buf, reasoningEvents(), "test-model", nil)

	var chunks []OpenAIChunk
	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			var chunk OpenAIChunk
			if err := json.Unmarshal([]byte(line[6:]), &chunk); err == nil {
				chunks = append(chunks, chunk)
			}
		}
	}

	firstReasoning, firstContent := -1, -1
	var reasoning strings.Builder
	for i, c := range chunks {
		if len(c.Choices) == 0 {
			continue
		}
		raw, _ := json.Marshal(c.Choices[0].Delta)
		var d openAIDelta
		_ = json.Unmarshal(raw, &d)
		if d.ReasoningContent != "" {
			if firstReasoning < 0 {
				firstReasoning = i
			}
			reasoning.WriteString(d.ReasoningContent)
		}
		if d.Content != "" && firstContent < 0 {
			firstContent = i
		}
	}

	if firstReasoning < 0 {
		t.Fatal("no chunk carried reasoning_content")
	}
	if firstContent < 0 {
		t.Fatal("no chunk carried content")
	}
	if firstReasoning > firstContent {
		t.Errorf("reasoning chunk (%d) must come before content chunk (%d)", firstReasoning, firstContent)
	}
	if reasoning.String() != "menghitung 17x23" {
		t.Errorf("reasoning = %q, want %q", reasoning.String(), "menghitung 17x23")
	}
}

// TestStreamAnthropicEmitsThinkingBlock covers the Anthropic surface, where
// reasoning belongs in a dedicated thinking block rather than in the text.
func TestStreamAnthropicEmitsThinkingBlock(t *testing.T) {
	var out strings.Builder
	w := NewAnthropicStreamWriter(&out, "test-model")
	StreamAnthropic(reasoningEvents(), w, nil)

	sse := out.String()
	if !strings.Contains(sse, `"type":"thinking"`) {
		t.Error("expected a thinking content block")
	}
	if !strings.Contains(sse, `"type":"thinking_delta"`) {
		t.Error("expected thinking_delta events")
	}
	// The answer must be its own block, not appended to the thinking block.
	if !strings.Contains(sse, `"type":"text"`) || !strings.Contains(sse, `"type":"text_delta"`) {
		t.Error("expected the answer in a separate text block")
	}
	if !strings.Contains(sse, "menghitung 17x23") {
		t.Error("reasoning text missing from the stream")
	}
	if !strings.Contains(sse, "391") {
		t.Error("answer text missing from the stream")
	}
}
