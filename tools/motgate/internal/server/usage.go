package server

import (
	"encoding/json"
	"io"
	"strconv"
)

// usageScanner forwards a response body unchanged while extracting the model id
// and token usage that mot-router reports.
//
// Two usage shapes appear, because mot-router serves two client surfaces:
//   - OpenAI: `"usage":{"prompt_tokens":N,"completion_tokens":M}`
//   - Anthropic: `"usage":{"input_tokens":N,"output_tokens":M}`
//
// Data is accumulated and inspected per complete line, because a single Read
// may split a JSON object across chunks; inspecting raw chunks would miss it.
type usageScanner struct {
	src      io.ReadCloser
	pending  []byte
	in       int64
	out      int64
	model    string
	seenIn   bool
	seenOut  bool
	complete bool
	event    string
}

const scannerMaxLine = 1 << 20

func newUsageScanner(src io.ReadCloser) *usageScanner {
	return &usageScanner{src: src}
}

func (u *usageScanner) Read(p []byte) (int, error) {
	n, err := u.src.Read(p)
	if n > 0 {
		u.consume(p[:n], false)
	}
	if err != nil {
		u.consume(nil, true)
	}
	return n, err
}

func (u *usageScanner) Close() error { return u.src.Close() }

// consume appends data and inspects every complete line. On flush it also
// inspects whatever remains.
func (u *usageScanner) consume(data []byte, flush bool) {
	u.pending = append(u.pending, data...)
	for {
		idx := indexBytes(u.pending, []byte("\n"))
		if idx < 0 {
			break
		}
		line := u.pending[:idx]
		line = bytesTrimCR(line)
		if len(line) > 7 && string(line[:7]) == "event: " {
			u.event = string(line[7:])
		}
		u.inspect(line)
		if len(line) == 0 {
			u.event = ""
		}
		u.pending = u.pending[idx+1:]
	}
	if len(u.pending) > scannerMaxLine {
		// A pathological line: inspect what we have and drop it so memory is
		// bounded. Usage data is always small and near the line end.
		u.inspect(u.pending)
		u.pending = u.pending[:0]
	}
	if flush && len(u.pending) > 0 {
		u.inspect(u.pending)
		u.pending = u.pending[:0]
	}
}

// inspect extracts the model id and any usage numbers from one line.
func (u *usageScanner) inspect(line []byte) {
	if u.model == "" {
		data := line
		if i := indexBytes(data, []byte("data:")); i >= 0 {
			data = data[i+len("data:"):]
			for len(data) > 0 && (data[0] == ' ' || data[0] == '\t') {
				data = data[1:]
			}
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(data, &object) == nil {
			var model string
			_ = json.Unmarshal(object["model"], &model)
			if model == "" {
				var msg map[string]json.RawMessage
				if json.Unmarshal(object["message"], &msg) == nil {
					_ = json.Unmarshal(msg["model"], &model)
				}
			}
			u.model = model
		}
	}

	idx := indexBytes(line, []byte("\"usage\""))
	for idx >= 0 {
		brace := indexBytes(line[idx:], []byte("{"))
		if brace < 0 {
			return
		}
		start := idx + brace
		end := matchingBrace(line, start)
		if end < 0 {
			return
		}
		var payload struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
			InputTokens      *int64 `json:"input_tokens"`
			OutputTokens     *int64 `json:"output_tokens"`
		}
		if err := json.Unmarshal(line[start:end+1], &payload); err == nil {
			applyCount(&u.in, &u.seenIn, payload.PromptTokens)
			applyCount(&u.in, &u.seenIn, payload.InputTokens)
			applyCount(&u.out, &u.seenOut, payload.CompletionTokens)
			applyCount(&u.out, &u.seenOut, payload.OutputTokens)
			anthropicPair := payload.InputTokens != nil && payload.OutputTokens != nil && u.event == "message_delta"
			openAIPair := payload.PromptTokens != nil && payload.CompletionTokens != nil && u.event == "" &&
				(indexBytes(line, []byte(`"choices":[]`)) >= 0 || (indexBytes(line, []byte(`"finish_reason":`)) >= 0 && indexBytes(line, []byte(`"finish_reason":null`)) < 0))
			if anthropicPair || openAIPair {
				u.complete = true
			}
		}
		rest := idx + 1
		if rest >= len(line) {
			return
		}
		next := indexBytes(line[rest:], []byte("\"usage\""))
		if next < 0 {
			return
		}
		idx = rest + next
	}
}

func bytesTrimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

// applyCount keeps the largest observed value, so a later cumulative total
// replaces an earlier partial one.
func applyCount(dst *int64, seen *bool, v *int64) {
	if v == nil {
		return
	}
	if !*seen || *v > *dst {
		*dst = *v
	}
	*seen = true
}

func (u *usageScanner) totals() (int64, int64, string, bool) {
	return u.in, u.out, u.model, u.complete
}

// indexBytes is a substring search; bytes.Index would do, but this keeps the
// package dependency-free for one helper.
func indexBytes(haystack, needle []byte) int {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return -1
	}
	first := needle[0]
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i] != first {
			continue
		}
		match := true
		for j := 1; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// matchingBrace returns the index of the brace closing the object opened at
// start, or -1 when it is not fully contained.
func matchingBrace(chunk []byte, start int) int {
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(chunk); i++ {
		c := chunk[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func strconvFormatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}
