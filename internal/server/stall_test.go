package server

import (
	"context"
	"testing"
	"time"

	"github.com/rian/mot-router/internal/zen"
)

// TestStreamFirstDetectsSilentStall is the regression guard for the bug where a
// node answered with HTTP 200 and then emitted nothing. Stream returns as soon
// as the headers arrive, so the request looked successful and the client simply
// hung: measured as a 17-token reply taking 79 seconds.
func TestStreamFirstDetectsSilentStall(t *testing.T) {
	events := make(chan zen.Event) // never written to
	started := time.Now()

	_, _, err := streamFirst(context.Background(), events, 200*time.Millisecond)
	if err == nil {
		t.Fatal("a stream that emits no event must be reported as a stall")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("stall detection took %s, expected it to respect the short deadline", elapsed)
	}
}

// TestStreamFirstReturnsFirstEvent confirms a healthy stream is not delayed and
// the first event is handed back rather than consumed.
func TestStreamFirstReturnsFirstEvent(t *testing.T) {
	events := make(chan zen.Event, 1)
	want := zen.Event{Type: "response.created"}
	events <- want
	close(events)

	got, rest, err := streamFirst(context.Background(), events, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Type != want.Type {
		t.Errorf("first event = %q, want %q", got.Type, want.Type)
	}
	// The remainder must be drained and closed, not left dangling.
	for range rest {
		t.Error("expected no further events")
	}
}

// TestStreamFirstReportsClosedStream covers a node that closes the connection
// without ever sending a frame.
func TestStreamFirstReportsClosedStream(t *testing.T) {
	events := make(chan zen.Event)
	close(events)

	if _, _, err := streamFirst(context.Background(), events, time.Second); err == nil {
		t.Fatal("an immediately closed stream must be an error, not a success")
	}
}

// TestReplayFirstPreservesFirstEvent checks that waiting for the first event
// does not drop it: the caller must still receive it, in order.
func TestReplayFirstPreservesFirstEvent(t *testing.T) {
	rest := make(chan zen.Event, 1)
	rest <- zen.Event{Type: "response.completed"}
	close(rest)

	out := replayFirst(zen.Event{Type: "response.created"}, rest)

	var got []string
	for ev := range out {
		got = append(got, ev.Type)
	}
	if len(got) != 2 || got[0] != "response.created" || got[1] != "response.completed" {
		t.Errorf("replayed events = %v, want [response.created response.completed]", got)
	}
}
