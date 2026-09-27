package server

import (
	"context"
	"net/http"
	"time"
)

// timeoutContext derives a bounded context from the request.
func timeoutContext(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
