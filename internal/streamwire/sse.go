// Package streamwire reads bounded SSE frames without interpreting provider identity.
package streamwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var Stop = errors.New("stop reading SSE")

// Read accepts multiline data fields, comments and data fields without a space.
// Both lines and assembled frames are bounded by maxBytes.
func Read(r io.Reader, maxBytes int, visit func([]byte) error) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), maxBytes)
	var frame []byte
	dispatch := func() error {
		if len(frame) == 0 {
			return nil
		}
		err := visit(bytes.TrimSuffix(frame, []byte{'\n'}))
		frame = frame[:0]
		return err
	}
	for s.Scan() {
		line := s.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				if errors.Is(err, Stop) {
					return nil
				}
				return err
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimPrefix(data, " ")
		if len(frame)+len(data)+1 > maxBytes {
			return fmt.Errorf("SSE frame exceeds %d bytes", maxBytes)
		}
		frame = append(frame, data...)
		frame = append(frame, '\n')
	}
	if err := s.Err(); err != nil {
		return err
	}
	err := dispatch()
	if errors.Is(err, Stop) {
		return nil
	}
	return err
}

// ValidCompletion requires actual output and confirmed termination. A 200,
// keepalive, usage-only frame, error or disconnected partial stream is insufficient.
func ValidCompletion(body []byte) bool {
	output, terminal, failed := false, false, false
	inspect := func(data []byte) error {
		if string(data) == "[DONE]" {
			terminal = true
			return nil
		}
		var v struct {
			Type    string          `json:"type"`
			Delta   string          `json:"delta"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content   string            `json:"content"`
					ToolCalls []json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
				Message struct {
					Content   string            `json:"content"`
					ToolCalls []json.RawMessage `json:"tool_calls"`
				} `json:"message"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Item struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"item"`
			Response struct {
				Status string          `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &v) != nil {
			failed = true
			return nil
		}
		if (len(v.Error) > 0 && string(v.Error) != "null") || (len(v.Response.Error) > 0 && string(v.Response.Error) != "null") || v.Type == "error" || v.Type == "response.failed" {
			failed = true
		}
		for _, c := range v.Choices {
			output = output || c.Delta.Content != "" || c.Message.Content != "" || len(c.Delta.ToolCalls) > 0 || len(c.Message.ToolCalls) > 0
			terminal = terminal || (c.FinishReason != nil && *c.FinishReason != "")
		}
		output = output || (v.Type == "response.output_text.delta" && v.Delta != "") || (v.Type == "response.output_item.done" && v.Item.Type == "function_call" && v.Item.Name != "")
		terminal = terminal || v.Type == "response.completed" || v.Type == "response.incomplete"
		return nil
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		_ = inspect(body)
	} else if Read(bytes.NewReader(body), 1<<20, inspect) != nil {
		return false
	}
	return output && terminal && !failed
}
