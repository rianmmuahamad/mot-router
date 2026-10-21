package zen

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UpstreamError retains retry policy without retaining headers or credentials.
type UpstreamError struct {
	Detail     string
	RetryAfter time.Duration
}

func (e *UpstreamError) Error() string { return e.Detail }
func upstreamError(resp *http.Response, detail []byte) error {
	wait := time.Duration(0)
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		wait = time.Duration(min(seconds, 86400)) * time.Second
	} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
		wait = min(24*time.Hour, max(0, time.Until(at)))
	}
	return &UpstreamError{Detail: strings.TrimSpace(string(detail)), RetryAfter: wait}
}
