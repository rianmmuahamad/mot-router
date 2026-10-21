package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/rian/motgate/internal/store"
)

// maxExtendHours bounds a single extend call so a typo cannot hand out a decade.
const maxExtendHours = 24 * 365

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.adminHTML)
}

func (s *Server) handleUserPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	key := r.PathValue("key")
	u, err := s.store.LookupUser(key)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if u == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.userHTML)
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	views := make([]map[string]any, 0, len(users))
	for _, u := range users {
		views = append(views, s.userView(u))
	}
	writeJSON(w, http.StatusOK, views)
}

// createUserRequest is the admin "add user" form body. The key and the expiry
// are produced together, which is the whole point of the single form.
type createUserRequest struct {
	Name       string          `json:"name"`
	Hours      json.RawMessage `json:"hours"`
	Days       json.RawMessage `json:"days"`
	PlanType   string          `json:"plan_type"`
	TokenLimit int64           `json:"token_limit"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body createUserRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	if body.Name == "" {
		body.Name = "user"
	}
	if len(body.Hours) > 0 && len(body.Days) > 0 {
		writeAPIError(w, http.StatusBadRequest, "send hours or days, not both", "invalid_request_error")
		return
	}
	// An omitted duration takes the default; an explicit non-positive value is
	// rejected rather than silently defaulted.
	var expiresAt int64
	if len(body.Days) > 0 {
		var days int
		if err := json.Unmarshal(body.Days, &days); err != nil {
			writeAPIError(w, http.StatusBadRequest, "days must be an integer", "invalid_request_error")
			return
		}
		if days < 1 || days > 365 {
			writeAPIError(w, http.StatusBadRequest, "days must be between 1 and 365", "invalid_request_error")
			return
		}
		expiresAt = nowMillis() + int64(days)*86400000
	} else {
		hours := s.cfg.DefaultHours
		if len(body.Hours) > 0 {
			if err := json.Unmarshal(body.Hours, &hours); err != nil {
				writeAPIError(w, http.StatusBadRequest, "hours must be an integer", "invalid_request_error")
				return
			}
		}
		if hours <= 0 || hours > maxExtendHours {
			writeAPIError(w, http.StatusBadRequest, "hours must be between 1 and "+strconv.Itoa(maxExtendHours), "invalid_request_error")
			return
		}
		expiresAt = nowMillis() + int64(hours)*3600000
	}
	plan := body.PlanType
	if plan == "" {
		plan = "unlimited"
	}
	if plan != "unlimited" && plan != "token" {
		writeAPIError(w, http.StatusBadRequest, "plan_type must be unlimited or token", "invalid_request_error")
		return
	}
	if plan == "token" && (body.TokenLimit <= 0 || len(body.Days) == 0) {
		writeAPIError(w, http.StatusBadRequest, "token plans require a positive token_limit and days", "invalid_request_error")
		return
	}
	if plan == "unlimited" && body.TokenLimit != 0 {
		writeAPIError(w, http.StatusBadRequest, "unlimited plans cannot set token_limit", "invalid_request_error")
		return
	}
	u, err := s.store.CreateUserWithPlan(body.Name, expiresAt, store.Plan{Type: plan, TokenLimit: body.TokenLimit})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusCreated, s.userView(*u))
}

type topUpRequest struct {
	TokenLimit int64 `json:"token_limit"`
	Days       int   `json:"days"`
}

func (s *Server) handleTopUpUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var body topUpRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, 400, "invalid JSON body", "invalid_request_error")
		return
	}
	if body.TokenLimit < 0 || body.Days < 0 || body.Days > 365 || (body.TokenLimit == 0 && body.Days == 0) {
		writeAPIError(w, 400, "provide token_limit, days, or both; days must be at most 365", "invalid_request_error")
		return
	}
	u, err := s.store.TopUp(r.Context(), id, body.TokenLimit, body.Days, nowMillis())
	if err != nil {
		writeAPIError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, 200, s.userView(*u))
}

type resolveUsageRequest struct {
	Mode         string `json:"mode"`
	Reason       string `json:"reason"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

func (s *Server) handleResolveUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("requestId")
	var body resolveUsageRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, 400, "invalid JSON body", "invalid_request_error")
		return
	}
	if err := s.store.ResolveUnknown(r.Context(), id, body.Mode, body.Reason, body.InputTokens, body.OutputTokens); err != nil {
		writeAPIError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type extendRequest struct {
	Hours int `json:"hours"`
}

func (s *Server) handleExtendUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var body extendRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	if body.Hours <= 0 || body.Hours > maxExtendHours {
		writeAPIError(w, http.StatusBadRequest,
			"hours must be between 1 and "+strconv.Itoa(maxExtendHours), "invalid_request_error")
		return
	}

	u, err := s.store.GetUser(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "user not found", "not_found_error")
		return
	}
	// Extending from the later of now and the current expiry never shortens a
	// still-valid window.
	base := nowMillis()
	if u.ExpiresAt > base {
		base = u.ExpiresAt
	}
	if ok, err := s.store.SetExpiry(id, base+int64(body.Hours)*3600*1000); err != nil || !ok {
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
			return
		}
		writeAPIError(w, http.StatusNotFound, "user not found", "not_found_error")
		return
	}
	s.respondUser(w, id)
}

func (s *Server) handleRevokeUser(w http.ResponseWriter, r *http.Request) {
	s.setRevoked(w, r, true)
}

func (s *Server) handleRestoreUser(w http.ResponseWriter, r *http.Request) {
	s.setRevoked(w, r, false)
}

func (s *Server) setRevoked(w http.ResponseWriter, r *http.Request, revoked bool) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if found, err := s.store.SetRevoked(id, revoked); err != nil || !found {
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
			return
		}
		writeAPIError(w, http.StatusNotFound, "user not found", "not_found_error")
		return
	}
	s.respondUser(w, id)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	found, err := s.store.DeleteUser(id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "user not found", "not_found_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	o, err := s.store.Overview()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*store.Overview
		APIBase    string `json:"api_base"`
		ServerTime int64  `json:"server_time"`
	}{o, strings.TrimRight(s.cfg.PublicURL, "/") + "/v1", nowMillis()})
}

func (s *Server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid user id", "invalid_request_error")
		return 0, false
	}
	return id, true
}

func (s *Server) respondUser(w http.ResponseWriter, id int64) {
	u, err := s.store.GetUser(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "user not found", "not_found_error")
		return
	}
	writeJSON(w, http.StatusOK, s.userView(*u))
}

func (s *Server) handleUserStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	key := r.PathValue("key")
	u, err := s.store.LookupUser(key)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if u == nil {
		writeAPIError(w, http.StatusNotFound, "user not found", "not_found_error")
		return
	}
	writeJSON(w, http.StatusOK, s.userView(*u))
}
