package zen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// StreamError is a provider failure, not a successful empty completion.
type StreamError struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	Status    int    `json:"-"`
	Retryable bool   `json:"-"`
}

func (e *StreamError) Error() string { return e.Message }
func (e *StreamError) HTTPStatus() int {
	if e.Status >= 400 && e.Status <= 599 {
		return e.Status
	}
	return http.StatusBadGateway
}
func streamFailure(message string, retryable bool) *StreamError {
	return &StreamError{Type: "upstream_error", Message: message, Status: 502, Retryable: retryable}
}
func parseStreamError(raw json.RawMessage) *StreamError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var e StreamError
	if json.Unmarshal(raw, &e) != nil || e.Message == "" {
		var message string
		if json.Unmarshal(raw, &message) != nil {
			message = "Upstream reported an in-stream failure"
		}
		e.Message = message
	}
	if e.Type == "" {
		e.Type = "upstream_error"
	}
	switch e.Type {
	case "FreeTierError", "permission_error":
		e.Status = 403
	case "authentication_error":
		e.Status = 401
	case "FreeUsageLimitError", "rate_limit_error":
		e.Status = 429
	default:
		e.Status = 502
	}
	return &e
}

// StreamOutcome is read by a handler only after its observed channel is drained.
type StreamOutcome struct {
	Failure  *StreamError
	Usage    *Usage
	Terminal bool
}

func ObserveStream(ctx context.Context, in <-chan Event) (<-chan Event, *StreamOutcome) {
	out := make(chan Event, 16)
	result := &StreamOutcome{}
	go func() {
		defer close(out)
		for ev := range in {
			if ev.Error != nil {
				result.Failure = ev.Error
			}
			if ev.StreamInterrupted && result.Failure == nil {
				result.Failure = streamFailure("Upstream stream was interrupted", false)
			}
			if ev.Type == "response.completed" || ev.Type == "response.incomplete" {
				result.Terminal = true
			}
			if ev.Response != nil && ev.Response.Usage != nil {
				result.Usage = ev.Response.Usage
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				result.Failure = streamFailure("Client disconnected before completion", false)
				return
			}
		}
		if !result.Terminal && result.Failure == nil {
			result.Failure = streamFailure("Upstream ended without a completion event", false)
			select {
			case out <- Event{Type: "error", Error: result.Failure, StreamInterrupted: true}:
			case <-ctx.Done():
			}
		}
	}()
	return out, result
}

func emitEvent(ctx context.Context, out chan<- Event, ev Event) error {
	select {
	case out <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func responseFinishReason(data []byte, ev *Event) {
	var metadata struct {
		Response struct {
			Status     string          `json:"status"`
			Error      json.RawMessage `json:"error"`
			Incomplete struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &metadata) != nil {
		return
	}
	if e := parseStreamError(metadata.Response.Error); e != nil {
		ev.Error = e
		ev.Type = "error"
	}
	if ev.Type == "response.failed" && ev.Error == nil {
		ev.Error = streamFailure("Upstream response failed", false)
		ev.Type = "error"
	}
	if ev.Type == "response.incomplete" || metadata.Response.Status == "incomplete" {
		ev.FinishReason = "length"
		if metadata.Response.Incomplete.Reason == "content_filter" {
			ev.FinishReason = "content_filter"
		}
	}
}

// ErrorBody uses the same error envelope on streaming and JSON surfaces.
func ErrorBody(e *StreamError) map[string]any {
	if e == nil {
		e = streamFailure("Unknown upstream failure", false)
	}
	return map[string]any{"error": e}
}

func invalidStream(err error) *StreamError {
	return streamFailure(fmt.Sprintf("Invalid upstream stream: %v", err), false)
}
