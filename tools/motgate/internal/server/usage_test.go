package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUsageScannerOpenAIShape(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	sc := newUsageScanner(io.NopCloser(strings.NewReader(body)))
	out, _ := io.ReadAll(sc)
	if string(out) != body {
		t.Fatal("scanner altered the stream")
	}
	in, got, _, complete := sc.totals()
	if in != 11 || got != 7 {
		t.Fatalf("openai usage: got in=%d out=%d want 11/7", in, got)
	}
	if !complete {
		t.Fatal("complete OpenAI usage pair was not recognized")
	}
}

func TestQuotaModelListIsFiltered(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"deepseek-v4.1-flash"},{"id":"deepseek-v4.1-flash-free"},{"id":"other-model"},{"id":"mimo-v2.6-flash-free"}]}`)), Header: make(http.Header)}
	if err := filterQuotaModels(resp); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "other-model") || strings.Contains(text, "mimo-v2.6-flash-free") || !strings.Contains(text, "deepseek-v4.1-flash-free") {
		t.Fatalf("unexpected filtered list: %s", text)
	}
}

func TestDeepSeekQuotaMeteringUsesFreeModelIdentity(t *testing.T) {
	for _, model := range []string{"deepseek-v4.1-flash", "deepseek-v4.1-flash-free", "opencode-zen/deepseek-v4.1-flash", "opencode-zen/deepseek-v4.1-flash-free"} {
		if got := quotaModelExpected(model, "openai"); got != "deepseek-v4.1-flash-free" {
			t.Fatalf("%s expects %s instead of the free DeepSeek model", model, got)
		}
	}
	if quotaModelAllowed("deepseek-v4.1-flash-free", "anthropic") {
		t.Fatal("unverified endpoint was enabled")
	}
}

func TestStreamingRequestIncludesUsageWithoutChangingOtherFields(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hello"}],"stream_options":{"include_usage":false,"vendor_flag":"keep"}}`)
	updated, err := ensureStreamUsage(body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err = json.Unmarshal(updated, &got); err != nil {
		t.Fatal(err)
	}
	var opts map[string]json.RawMessage
	if err = json.Unmarshal(got["stream_options"], &opts); err != nil {
		t.Fatal(err)
	}
	var include bool
	_ = json.Unmarshal(opts["include_usage"], &include)
	if !include || string(opts["vendor_flag"]) != "\"keep\"" || string(got["messages"]) == "null" {
		t.Fatalf("usage option not enabled while preserving request fields: %s", updated)
	}
}

func TestUsageScannerAnthropicShape(t *testing.T) {
	body := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":23,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":23,\"output_tokens\":44}}\n\n"
	sc := newUsageScanner(io.NopCloser(strings.NewReader(body)))
	if _, err := io.ReadAll(sc); err != nil {
		t.Fatal(err)
	}
	in, got, _, complete := sc.totals()
	if in != 23 || got != 44 {
		t.Fatalf("anthropic usage: got in=%d out=%d want 23/44", in, got)
	}
	if !complete {
		t.Fatal("final Anthropic usage pair was not recognized")
	}
}

// A plain JSON body (non-stream) must still be counted.
func TestUsageScannerPlainJSON(t *testing.T) {
	body := `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`
	sc := newUsageScanner(io.NopCloser(strings.NewReader(body)))
	if _, err := io.ReadAll(sc); err != nil {
		t.Fatal(err)
	}
	in, got, _, complete := sc.totals()
	if in != 5 || got != 9 {
		t.Fatalf("plain usage: got in=%d out=%d want 5/9", in, got)
	}
	if !complete {
		t.Fatal("plain JSON usage pair was not recognized")
	}
}

func TestUsageScannerIgnoresInitialZeroUsage(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\n"
	sc := newUsageScanner(io.NopCloser(strings.NewReader(body)))
	_, _ = io.ReadAll(sc)
	_, _, _, complete := sc.totals()
	if complete {
		t.Fatal("initial zero usage was incorrectly treated as a final report")
	}
}

func TestUsageScannerCumulativeReportsUseLatestMaximum(t *testing.T) {
	body := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":25,\"completion_tokens\":9}}\n\n"
	sc := newUsageScanner(io.NopCloser(strings.NewReader(body)))
	_, _ = io.ReadAll(sc)
	in, out, _, complete := sc.totals()
	if !complete || in != 25 || out != 9 {
		t.Fatalf("cumulative usage: got %d/%d complete=%v", in, out, complete)
	}
}

func TestUsageScannerNoUsage(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"only text\"}}]}\n\n"
	sc := newUsageScanner(io.NopCloser(strings.NewReader(body)))
	if _, err := io.ReadAll(sc); err != nil {
		t.Fatal(err)
	}
	in, got, _, complete := sc.totals()
	if in != 0 || got != 0 {
		t.Fatalf("no-usage body must not invent counts: got %d/%d", in, got)
	}
	if complete {
		t.Fatal("missing usage was marked complete")
	}
}
