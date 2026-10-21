package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func decodeGatewayJSON(r *http.Request, dst any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(dst)
}

// handleListProviders returns Token Harbor and Cline status plus the default.
func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	thKeys := s.tokenHarborKeys()
	now := time.Now()

	var thKeyViews []string
	activeCount := 0
	penalizedCount := 0

	thMu.Lock()
	for _, k := range thKeys {
		thKeyViews = append(thKeyViews, redactKey(k))
		if exp, penalized := thPenalties[k]; penalized && now.Before(exp) {
			penalizedCount++
		} else {
			activeCount++
		}
	}
	thMu.Unlock()

	clineKey := s.clineAPIKey()
	codeBuddyKey := s.codeBuddyAPIKey()
	codeBuddyAccounts, _ := s.listCodeBuddyAccountViews()
	codeBuddyOAuthConnected := len(codeBuddyAccounts) > 0
	codeBuddyConnected := codeBuddyOAuthConnected || codeBuddyKey != ""

	qTotal, qActive, qPenalized, _ := s.qwenPoolStatus()

	writeJSON(w, http.StatusOK, map[string]any{
		"tokenharbor": map[string]any{
			"configured":     len(thKeys) > 0,
			"total_keys":     len(thKeys),
			"active_keys":    activeCount,
			"penalized_keys": penalizedCount,
			"api_keys":       thKeyViews,
			"base_url":       s.tokenHarborBaseURL(),
			"qwen_pool": map[string]any{
				"total_keys":     qTotal,
				"active_keys":    qActive,
				"penalized_keys": qPenalized,
				"dedicated":      s.tokenHarborQwenDedicated(),
			},
		},
		"cline": map[string]any{
			"configured": clineKey != "",
			"api_key":    redactKey(clineKey),
			"base_url":   s.clineBaseURL(),
		},
		"codebuddy": map[string]any{
			"configured":         codeBuddyConnected,
			"oauth_connected":    codeBuddyOAuthConnected,
			"accounts":           codeBuddyAccounts,
			"api_key_configured": codeBuddyKey != "",
			"api_key":            redactKey(codeBuddyKey),
		},
		"default": s.configuredDefaultProvider(),
	})
}

// handleSetTokenHarbor updates the Token Harbor API keys. When qwen_only is
// true the keys go to the dedicated Qwen pool instead of the main pool.
func (s *Server) handleSetTokenHarbor(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey   string `json:"api_key"`
		BaseURL  string `json:"base_url"`
		QwenOnly bool   `json:"qwen_only"`
	}
	if err := decodeGatewayJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON", "bad_request")
		return
	}

	if cleanedKeys := strings.TrimSpace(body.APIKey); cleanedKeys != "" {
		if body.QwenOnly {
			if err := s.store.SetSetting("tokenharbor_qwen_api_key", cleanedKeys); err != nil {
				writeAPIError(w, http.StatusInternalServerError, err.Error(), "store_error")
				return
			}
			thQwenMu.Lock()
			thQwenPenalties = make(map[string]time.Time)
			thQwenMu.Unlock()
		} else {
			if err := s.store.SetSetting("tokenharbor_api_key", cleanedKeys); err != nil {
				writeAPIError(w, http.StatusInternalServerError, err.Error(), "store_error")
				return
			}
			thMu.Lock()
			thPenalties = make(map[string]time.Time)
			thMu.Unlock()
		}
	}

	if cleanedURL := strings.TrimSpace(body.BaseURL); cleanedURL != "" {
		if err := s.store.SetSetting("tokenharbor_base_url", cleanedURL); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error(), "store_error")
			return
		}
	}

	msg := "Token Harbor configuration saved"
	if body.QwenOnly {
		msg = "Token Harbor Qwen-only key saved"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": msg,
	})
}

// handleSetCline updates the Cline API key and Base URL.
func (s *Server) handleSetCline(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey  string `json:"api_key"`
		BaseURL string `json:"base_url"`
	}
	if err := decodeGatewayJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON", "bad_request")
		return
	}

	if cleanedKey := strings.TrimSpace(body.APIKey); cleanedKey != "" {
		if err := s.store.SetSetting("cline_api_key", cleanedKey); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error(), "store_error")
			return
		}
	}

	if cleanedURL := strings.TrimSpace(body.BaseURL); cleanedURL != "" {
		if err := s.store.SetSetting("cline_base_url", cleanedURL); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error(), "store_error")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": "Cline configuration saved",
	})
}

// handleSetCodeBuddy stores an optional CodeBuddy Intl API key.
func (s *Server) handleSetCodeBuddy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := decodeGatewayJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON", "bad_request")
		return
	}
	if key := strings.TrimSpace(body.APIKey); key != "" {
		if err := s.store.SetSetting("codebuddy_api_key", key); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error(), "store_error")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "CodeBuddy Intl API key saved"})
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "tokenharbor" {
		_ = s.store.SetSetting("tokenharbor_api_key", "")
		thMu.Lock()
		thPenalties = make(map[string]time.Time)
		thMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
		return
	}
	if id == "tokenharbor-qwen" {
		_ = s.store.SetSetting("tokenharbor_qwen_api_key", "")
		thQwenMu.Lock()
		thQwenPenalties = make(map[string]time.Time)
		thQwenMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
		return
	}
	if id == "cline" {
		_ = s.store.SetSetting("cline_api_key", "")
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
		return
	}
	if id == ProviderCodeBuddy {
		_ = s.store.SetSetting("codebuddy_api_key", "")
		_ = s.store.SetSetting("codebuddy_accounts", "[]")
		_ = s.store.SetSetting("codebuddy_access_token", "")
		_ = s.store.SetSetting("codebuddy_refresh_token", "")
		_ = s.store.SetSetting("codebuddy_expires_at", "")
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
		return
	}
	writeAPIError(w, http.StatusNotFound, "provider account not found", "not_found")
}

func (s *Server) handleSetDefaultProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
	}
	if err := decodeGatewayJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON", "bad_request")
		return
	}
	switch body.Provider {
	case ProviderZen, ProviderTokenHarbor, ProviderCline, ProviderCodeBuddy:
	default:
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("unknown provider %q (valid: %s, %s, %s, %s)", body.Provider, ProviderZen, ProviderTokenHarbor, ProviderCline, ProviderCodeBuddy), "bad_provider")
		return
	}
	if err := s.store.SetSetting("default_provider", body.Provider); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "store_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": body.Provider})
}

func (s *Server) configuredDefaultProvider() string {
	if v := s.store.GetSetting("default_provider"); v != "" {
		return v
	}
	return ProviderZen
}

func (s *Server) effectiveProvider() string {
	return s.configuredDefaultProvider()
}

func (s *Server) gatewayModels() []map[string]any {
	models := []map[string]any{
		{
			"id":         "cline/glm-5.2",
			"name":       "GLM 5.2 (Z.ai via Cline Free)",
			"provider":   ProviderCline,
			"free":       true,
			"context":    1048576,
			"max_output": 8192,
		},
		{
			"id":         "cline-free/gemini-3.8-flash",
			"name":       "Gemini 3.8 Flash (Cline Free)",
			"provider":   ProviderCline,
			"free":       true,
			"context":    1048576,
			"max_output": 8192,
		},
		{
			"id":         "cline-free/deepseek-v4.1-flash",
			"name":       "DeepSeek V4.1 Flash (Cline Free)",
			"provider":   ProviderCline,
			"free":       true,
			"context":    1048576,
			"max_output": 8192,
		},
		{
			"id":         "tokenharbor/qwen3.8-flash:free",
			"name":       "Qwen3.8 Flash (Limited Free)",
			"provider":   ProviderTokenHarbor,
			"free":       true,
			"context":    983616,
			"max_output": 8192,
		},
		{
			"id":         "tokenharbor/deepseek-v4.1-flash:free",
			"name":       "DeepSeek V4.1 Flash (Free)",
			"provider":   ProviderTokenHarbor,
			"free":       true,
			"context":    1048576,
			"max_output": 8192,
		},
	}
	for _, model := range codeBuddyModels {
		models = append(models, map[string]any{
			"id":       "codebuddy/" + model.ID,
			"name":     model.Name + " (CodeBuddy Intl)",
			"provider": ProviderCodeBuddy,
		})
	}
	return models
}
