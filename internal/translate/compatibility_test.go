package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rian/mot-router/internal/zen"
)

func TestRequestControlsAndImageRoundTrip(t *testing.T) {
	var req OpenAIChatRequest
	if err := json.Unmarshal([]byte(`{"model":"fixture","temperature":0,"top_p":0.7,"max_completion_tokens":128,"tool_choice":"none","parallel_tool_calls":false,"stop":["END"],"response_format":{"type":"json_object"},"reasoning_effort":"low","messages":[{"role":"user","content":[{"type":"text","text":"image"},{"type":"image_url","image_url":{"url":"data:image/png;base64,fixture","detail":"low"}}]}]}`), &req); err != nil {
		t.Fatal(err)
	}
	translated, err := OpenAIToZen(&req, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := zen.ConvertToChatPayload("fixture", translated.Body)
	if body["max_tokens"] != 128 || body["temperature"] != float64(0) || body["tool_choice"] != "none" || body["parallel_tool_calls"] != false || body["reasoning_effort"] != "low" {
		t.Fatalf("caller controls lost: %+v", body)
	}
	for _, key := range []string{"top_p", "stop", "response_format"} {
		if body[key] == nil {
			t.Fatalf("lost %s", key)
		}
	}
	wire, _ := json.Marshal(body["messages"])
	if !strings.Contains(string(wire), "data:image/png;base64,fixture") || !strings.Contains(string(wire), `"detail":"low"`) {
		t.Fatalf("image lost: %s", wire)
	}
}
func TestAnthropicControlsAndURLImage(t *testing.T) {
	var req AnthropicRequest
	json.Unmarshal([]byte(`{"max_tokens":64,"temperature":0,"stop_sequences":["END"],"tool_choice":{"type":"tool","name":"Read","disable_parallel_tool_use":true},"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://fixture.invalid/image.png"}}]}]}`), &req)
	translated, err := AnthropicToZen(&req, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := zen.ConvertToChatPayload("fixture", translated.Body)
	if body["max_tokens"] != 64 || body["temperature"] != float64(0) || body["parallel_tool_calls"] != false {
		t.Fatal(body)
	}
	wire, _ := json.Marshal(body)
	if !strings.Contains(string(wire), `"function":{"name":"read"}`) || !strings.Contains(string(wire), "https://fixture.invalid/image.png") {
		t.Fatalf("tool choice or image lost: %s", wire)
	}
}

func terminalEvents(failure bool, usage bool) <-chan zen.Event {
	ch := make(chan zen.Event, 3)
	ch <- zen.Event{Type: "response.output_text.delta", Delta: "partial"}
	if failure {
		ch <- zen.Event{Type: "error", Error: &zen.StreamError{Type: "fixture_error", Message: "LOCAL_FAILURE", Status: 502}, StreamInterrupted: true}
	} else {
		final := zen.Event{Type: "response.incomplete", FinishReason: "length"}
		if usage {
			final.Response = &struct {
				Usage *zen.Usage `json:"usage"`
			}{Usage: &zen.Usage{InputTokens: 30, OutputTokens: 8}}
		}
		ch <- final
	}
	close(ch)
	return ch
}
func TestFailureNeverFinishesAsSuccess(t *testing.T) {
	result := CollectOpenAI(terminalEvents(true, false), "fixture", nil)
	if result["error"] == nil || result["usage"] != nil || result["choices"] != nil {
		t.Fatal(result)
	}
	var out strings.Builder
	StreamOpenAI(&out, terminalEvents(true, false), "fixture", nil)
	if !strings.Contains(out.String(), "LOCAL_FAILURE") || strings.Contains(out.String(), "[DONE]") || strings.Contains(out.String(), `"finish_reason":"stop"`) {
		t.Fatal(out.String())
	}
	out.Reset()
	StreamAnthropic(terminalEvents(true, false), NewAnthropicStreamWriter(&out, "fixture"), nil)
	if !strings.Contains(out.String(), "event: error") || strings.Contains(out.String(), "message_stop") {
		t.Fatal(out.String())
	}
}
func TestIncompleteFinishAndMissingUsage(t *testing.T) {
	result := CollectOpenAI(terminalEvents(false, false), "fixture", nil)
	if result["usage"] != nil || result["choices"].([]map[string]any)[0]["finish_reason"] != "length" {
		t.Fatal(result)
	}
	var out strings.Builder
	StreamAnthropic(terminalEvents(false, false), NewAnthropicStreamWriter(&out, "fixture"), nil)
	if !strings.Contains(out.String(), `"stop_reason":"max_tokens"`) {
		t.Fatal(out.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, `"type":"message_delta"`) && strings.Contains(line, `"usage"`) {
			t.Fatal("missing measured usage was replaced with zero")
		}
	}
}
