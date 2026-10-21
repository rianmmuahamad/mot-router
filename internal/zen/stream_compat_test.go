package zen

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamFailuresAndMeasuredUsage(t *testing.T) {
	for _, tc := range []struct {
		name, wire, finish string
		failure            bool
		usage              int64
	}{
		{"error", "data: {\"error\":{\"type\":\"rate_limit_error\",\"message\":\"fixture limit\"}}\n\n", "", true, 0},
		{"partial EOF", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", "", true, 0},
		{"initial zero", "data: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0},\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", "stop", false, 0},
		{"progress not final", "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2},\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", "length", false, 0},
		{"cumulative", "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2},\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: {\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":8},\"choices\":[]}\n\ndata: {\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":8},\"choices\":[]}\n\ndata: [DONE]\n\n", "length", false, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte(tc.wire))
			}))
			defer up.Close()
			c := New(up.URL, "fixture", "")
			events, _, err := c.StreamChatCompletions(t.Context(), "fixture-model", "fixture", map[string]any{})
			if err != nil {
				t.Fatal(err)
			}
			events, outcome := ObserveStream(t.Context(), events)
			finish := ""
			for ev := range events {
				if ev.FinishReason != "" {
					finish = ev.FinishReason
				}
			}
			if (outcome.Failure != nil) != tc.failure || finish != tc.finish {
				t.Fatalf("failure=%v finish=%s", outcome.Failure, finish)
			}
			if tc.usage == 0 && outcome.Usage != nil {
				t.Fatal("unverified usage became billable")
			}
			if tc.usage > 0 && (outcome.Usage == nil || outcome.Usage.InputTokens != tc.usage || outcome.Usage.OutputTokens != 8) {
				t.Fatal("cumulative usage lost or counted twice")
			}
		})
	}
}

func TestResponsesFailureAndIncomplete(t *testing.T) {
	t.Setenv("ZEN_RESPONSES_MODELS", "muse-fixture")
	for _, fail := range []bool{false, true} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if fail {
				w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"fixture failed\"}}}\n\n"))
				return
			}
			w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\n"))
		}))
		c := New(up.URL, "fixture", "")
		events, _, err := c.Stream(t.Context(), "muse-fixture", "fixture", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		var reason string
		var failure bool
		for ev := range events {
			reason += ev.FinishReason
			failure = failure || ev.Error != nil
		}
		up.Close()
		if failure != fail || (!fail && !strings.Contains(reason, "length")) {
			t.Fatal("response terminal state lost")
		}
	}
}
