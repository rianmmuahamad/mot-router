package translate

import (
	"bytes"
	"strings"
	"testing"
)

func TestAnthropicFinalUsageContainsBothTokenCounts(t *testing.T) {
	var out bytes.Buffer
	w := NewAnthropicStreamWriter(&out, "test-model")
	w.Start()
	w.SetUsage(123, 45)
	w.Finish()
	text := out.String()
	idx := strings.LastIndex(text, "event: message_delta")
	if idx < 0 {
		t.Fatal("final message_delta missing")
	}
	final := text[idx:]
	if !strings.Contains(final, `"input_tokens":123`) || !strings.Contains(final, `"output_tokens":45`) {
		t.Fatalf("final usage must contain the reported input and output counts: %s", final)
	}
}
