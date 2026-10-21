package streamwire

import (
	"strings"
	"testing"
)

func TestValidCompletionRequiresOutputAndTermination(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"chat", "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true},
		{"responses", "data:{\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata:{\"type\":\"response.completed\"}\n\n", true},
		{"multiline", "data: {\"choices\": [\n" + "data: {\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n", true},
		{"empty", "data: {}\n\n", false},
		{"disconnected", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", false},
		{"error", "data: {\"error\":{\"message\":\"rejected\"}}\n\ndata: [DONE]\n\n", false},
		{"late error", "data:{\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata:{\"type\":\"response.failed\"}\n\n", false},
		{"usage only", "data: {\"usage\":{\"prompt_tokens\":10}}\n\ndata: [DONE]\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidCompletion([]byte(tc.body)); got != tc.ok {
				t.Fatalf("got %v want %v", got, tc.ok)
			}
		})
	}
}
func TestFrameAndLineBounds(t *testing.T) {
	if err := Read(strings.NewReader("data: "+strings.Repeat("x", 9000)), 8192, func([]byte) error { return nil }); err == nil {
		t.Fatal("unbounded line accepted")
	}
	if err := Read(strings.NewReader("data: "+strings.Repeat("x", 5000)+"\ndata: "+strings.Repeat("x", 5000)+"\n\n"), 8192, func([]byte) error { return nil }); err == nil {
		t.Fatal("unbounded multiline frame accepted")
	}
}
