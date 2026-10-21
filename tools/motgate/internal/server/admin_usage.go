package server

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	if period := r.URL.Query().Get("period"); period != "" && period != "7d" {
		writeAPIError(w, 400, "period must be 7d", "invalid_request_error")
		return
	}
	// Seven calendar days, including today, so dates agree with the admin UI.
	wib := time.FixedZone("Asia/Jakarta", 7*3600)
	now := time.Now().In(wib)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, wib).AddDate(0, 0, -6)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.store.AdminUsage(ctx, start.UnixMilli(), now.UnixMilli()+1, 86400000)
	if err != nil {
		writeAPIError(w, 500, "unable to load portal usage", "api_error")
		return
	}
	writeJSON(w, 200, result)
}
