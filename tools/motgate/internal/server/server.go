// Package server implements the motgate portal: an admin console, a per-user
// status page, and an inference endpoint that fronts mot-router.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	"github.com/rian/motgate/internal/store"
)

//go:embed admin.html catalog.html user.html user_entry.html landing.html assets
var pages embed.FS

// Config is the server's runtime configuration.
type Config struct {
	ModelCatalogURL string
	PublicURL       string
	Upstream        string
	AdminPassword   string
	DefaultHours    int
}

// Server holds the portal state.
type Server struct {
	cfg   Config
	store *store.Store

	adminHTML   []byte
	catalogHTML []byte
	userHTML    []byte
	landHTML    []byte
}

// New builds a server.
func New(cfg Config, st *store.Store) (*Server, error) {
	s := &Server{cfg: cfg, store: st}
	var err error
	if s.adminHTML, err = pages.ReadFile("admin.html"); err != nil {
		return nil, err
	}
	if s.catalogHTML, err = pages.ReadFile("catalog.html"); err != nil {
		return nil, err
	}
	if s.userHTML, err = pages.ReadFile("user.html"); err != nil {
		return nil, err
	}
	if s.landHTML, err = pages.ReadFile("landing.html"); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Admin console. The password lives here, not in Caddy, so a second
	// deployment does not need extra proxy configuration.
	mux.HandleFunc("GET /admin", s.requireAdmin(s.handleAdminPage))
	mux.HandleFunc("GET /admin/catalog", s.requireAdmin(s.handleCatalogPage))
	mux.HandleFunc("GET /api/catalog", s.handlePublicCatalog)
	mux.HandleFunc("GET /api/admin/catalog/router-models", s.requireAdmin(s.handleRouterCatalog))
	mux.HandleFunc("GET /api/admin/catalog", s.requireAdmin(s.handleAdminCatalog))
	mux.HandleFunc("PUT /api/admin/catalog", s.requireAdmin(s.handleSaveAdminCatalog))
	mux.HandleFunc("GET /api/admin/users", s.requireAdmin(s.handleListUsers))
	mux.HandleFunc("POST /api/admin/users", s.requireAdmin(s.handleCreateUser))
	mux.HandleFunc("POST /api/admin/users/{id}/extend", s.requireAdmin(s.handleExtendUser))
	mux.HandleFunc("POST /api/admin/users/{id}/topup", s.requireAdmin(s.handleTopUpUser))
	mux.HandleFunc("POST /api/admin/quota-requests/{requestId}/resolve", s.requireAdmin(s.handleResolveUsage))
	mux.HandleFunc("POST /api/admin/users/{id}/revoke", s.requireAdmin(s.handleRevokeUser))
	mux.HandleFunc("POST /api/admin/users/{id}/restore", s.requireAdmin(s.handleRestoreUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", s.requireAdmin(s.handleDeleteUser))
	mux.HandleFunc("GET /api/admin/overview", s.requireAdmin(s.handleOverview))
	mux.HandleFunc("GET /api/admin/usage", s.requireAdmin(s.handleAdminUsage))

	// User page and status.
	mux.HandleFunc("GET /u/{key}", s.handleUserPage)
	mux.HandleFunc("GET /api/user/{key}", s.handleUserStatus)
	mux.HandleFunc("GET /api/user/{key}/usage", s.handleUserUsage)
	assets, _ := fs.Sub(pages, "assets")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	// Let users open their existing account using the key provided by admin.
	mux.HandleFunc("GET /u", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		body, _ := pages.ReadFile("user_entry.html")
		s.writeHTML(w, body)
	})

	// Inference surface, mirroring mot-router's routes.
	mux.HandleFunc("POST /v1/chat/completions", s.handleInference)
	mux.HandleFunc("POST /v1/messages", s.handleInference)
	mux.HandleFunc("POST /v1/systemone", s.handleInference)
	mux.HandleFunc("POST /zen/v1/systemone", s.handleInference)
	mux.HandleFunc("GET /v1/models", s.handleInference)

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s.writeHTML(w, s.landHTML)
	})
	// The pricing section lives on the single landing page; keep /pricing as a
	// redirect so existing links do not 404.
	mux.HandleFunc("GET /pricing", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/#harga", http.StatusFound)
	})

	return mux
}

// writeHTML serves an embedded page.
func (s *Server) writeHTML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}

// adminAuthorized accepts `Authorization: Bearer <password>` or HTTP Basic with
// the password as the credential.
func (s *Server) adminAuthorized(r *http.Request) bool {
	if pw := s.cfg.AdminPassword; pw != "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			if strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")) == pw {
				return true
			}
		}
		if _, pass, ok := r.BasicAuth(); ok && pass == pw {
			return true
		}
	}
	return false
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !s.adminAuthorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="motgate admin"`)
			writeAPIError(w, http.StatusUnauthorized, "admin authentication required",
				"authentication_error")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"upstream": s.cfg.Upstream,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, message, errType string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

// userView decorates a store.User with derived fields for the UI.
func (s *Server) userView(u store.User) map[string]any {
	now := nowMillis()
	expired := u.ExpiresAt != 0 && now >= u.ExpiresAt
	left := int64(0)
	if !expired && u.ExpiresAt != 0 {
		left = (u.ExpiresAt - now) / 1000
	}
	var issue any
	if u.QuotaPending != "" {
		if q, err := s.store.GetQuotaIssue(context.Background(), u.QuotaPending); err == nil {
			issue = q
		}
	}
	return map[string]any{
		"server_time":              now,
		"id":                       u.ID,
		"name":                     u.Name,
		"key":                      u.Key,
		"created_at":               u.CreatedAt,
		"expires_at":               u.ExpiresAt,
		"revoked":                  u.Revoked,
		"request_count":            u.RequestCount,
		"tokens_in":                u.TokensIn,
		"tokens_out":               u.TokensOut,
		"tokens_total":             u.TokensIn + u.TokensOut,
		"plan_type":                u.PlanType,
		"token_limit":              u.TokenLimit,
		"quota_used":               u.QuotaUsed,
		"quota_remaining":          max64(0, u.TokenLimit-u.QuotaUsed),
		"quota_state":              u.QuotaState,
		"quota_pending_request_id": u.QuotaPending,
		"quota_issue":              issue,
		"metering":                 map[string]any{"mode": "reported_usage_only", "models": []string{"deepseek-v4.1-flash-free", "deepseek-v4.1-flash", "codebuddy/deepseek-v4.1-flash"}, "status": "verified_openai"},
		"last_used_at":             u.LastUsedAt,
		"time_left_seconds":        left,
		"expired":                  expired,
		"url":                      s.userURL(u.Key),
		"api_base":                 strings.TrimRight(s.cfg.PublicURL, "/") + "/v1",
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Server) userURL(key string) string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/u/" + key
}
