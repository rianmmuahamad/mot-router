package server

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) handleUserUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u, err := s.store.LookupUser(r.PathValue("key"))
	if err != nil {
		writeAPIError(w, 500, "unable to load account", "api_error")
		return
	}
	if u == nil {
		writeAPIError(w, 404, "user not found", "not_found_error")
		return
	}
	q := r.URL.Query()
	period := q.Get("period")
	if period == "" {
		period = "24h"
	}
	duration, step := 24*time.Hour, 3*time.Hour
	switch period {
	case "24h":
	case "7d":
		duration = 7 * 24 * time.Hour
		step = 24 * time.Hour
	default:
		writeAPIError(w, 400, "period must be 24h or 7d", "invalid_request_error")
		return
	}
	page, limit := 1, 50
	for name, dst := range map[string]*int{"page": &page, "limit": &limit} {
		if v := q.Get(name); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || (name == "page" && n > 10000) || (name == "limit" && n > 200) {
				writeAPIError(w, 400, "invalid pagination", "invalid_request_error")
				return
			}
			*dst = n
		}
	}
	if len(q.Get("model")) > 256 || len(q.Get("search")) > 128 {
		writeAPIError(w, 400, "filter too long", "invalid_request_error")
		return
	}
	now := time.Now().UnixMilli()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.store.UserUsage(ctx, u.ID, now-duration.Milliseconds(), now, step.Milliseconds(), page, limit, q.Get("model"), q.Get("search"))
	if err != nil {
		writeAPIError(w, 500, "unable to load usage", "api_error")
		return
	}
	writeJSON(w, 200, result)
}
