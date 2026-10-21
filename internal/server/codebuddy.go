package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rian/mot-router/internal/store"
)

const (
	codeBuddyBaseURL   = "https://www.codebuddy.ai"
	codeBuddyUserAgent = "IDE/2.108.1 CodeBuddy/2.108.1"
)

type codeBuddyModel struct {
	ID   string
	Name string
}

// This catalog follows the CodeBuddy Intl provider in the installed 9Router.
var codeBuddyModels = []codeBuddyModel{
	{ID: "glm-5.2", Name: "GLM-5.2"},
	{ID: "glm-5.1", Name: "GLM-5.1"},
	{ID: "glm-5.0", Name: "GLM-5.0"},
	{ID: "glm-5.0-turbo", Name: "GLM-5.0-Turbo"},
	{ID: "glm-5v-turbo", Name: "GLM-5v-Turbo"},
	{ID: "glm-4.7", Name: "GLM-4.7"},
	{ID: "minimax-m3", Name: "MiniMax-M3"},
	{ID: "minimax-m2.7", Name: "MiniMax-M2.7"},
	{ID: "kimi-k2.7", Name: "Kimi-K2.7-Code"},
	{ID: "kimi-k2.6", Name: "Kimi-K2.6"},
	{ID: "kimi-k2.5", Name: "Kimi-K2.5"},
	{ID: "hy3-preview", Name: "Hy3 Preview"},
	{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro"},
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek-V4.1-Flash"},
	{ID: "deepseek-v3-2-volc", Name: "DeepSeek-V3.2"},
}

var codeBuddyRefreshMu sync.Mutex

type codeBuddyAccount struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	Active       bool   `json:"active"`
	AddedAt      int64  `json:"added_at"`
}

type codeBuddyAccountView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Email   string `json:"email,omitempty"`
	Active  bool   `json:"active"`
	AddedAt int64  `json:"added_at"`
}

type codeBuddyStateResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	} `json:"data"`
}

type codeBuddyTokenResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		TokenType    string `json:"tokenType"`
		ExpiresIn    int64  `json:"expiresIn"`
	} `json:"data"`
}

func codeBuddyAuthHeaders(domain string) http.Header {
	h := make(http.Header)
	h.Set("Accept", "application/json")
	h.Set("User-Agent", codeBuddyUserAgent)
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-Domain", domain)
	h.Set("X-Product", "SaaS")
	return h
}

func (s *Server) handleCodeBuddyLogin(w http.ResponseWriter, r *http.Request) {
	form := url.Values{"platform": {"ide"}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, codeBuddyBaseURL+"/v2/plugin/auth/state?"+form.Encode(), strings.NewReader("{}"))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "oauth_error")
		return
	}
	req.Header = codeBuddyAuthHeaders("www.codebuddy.ai")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-User-Id", "true")
	resp, err := s.codeBuddyHTTPClient().Do(req)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "CodeBuddy login request failed: "+err.Error(), "oauth_error")
		return
	}
	defer resp.Body.Close()
	var result codeBuddyStateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		writeAPIError(w, http.StatusBadGateway, "invalid CodeBuddy login response", "oauth_error")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Code != 0 || result.Data.State == "" || result.Data.AuthURL == "" {
		message := result.Msg
		if message == "" {
			message = fmt.Sprintf("CodeBuddy login could not start (HTTP %d)", resp.StatusCode)
		}
		writeAPIError(w, http.StatusBadGateway, message, "oauth_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "waiting", "state": result.Data.State,
		"auth_url": result.Data.AuthURL, "interval": 5,
	})
}

func (s *Server) handleCodeBuddyPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	if err := decodeGatewayJSON(r, &body); err != nil || strings.TrimSpace(body.State) == "" {
		writeAPIError(w, http.StatusBadRequest, "missing CodeBuddy login state", "bad_request")
		return
	}
	endpoint := codeBuddyBaseURL + "/v2/plugin/auth/token?state=" + url.QueryEscape(body.State)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "oauth_error")
		return
	}
	req.Header = codeBuddyAuthHeaders("www.codebuddy.ai")
	for _, name := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		req.Header.Set(name, "true")
	}
	resp, err := s.codeBuddyHTTPClient().Do(req)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "CodeBuddy login polling failed: "+err.Error(), "oauth_error")
		return
	}
	defer resp.Body.Close()
	var result codeBuddyTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		writeAPIError(w, http.StatusBadGateway, "invalid CodeBuddy token response", "oauth_error")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeAPIError(w, http.StatusBadGateway, fmt.Sprintf("CodeBuddy login polling failed (HTTP %d)", resp.StatusCode), "oauth_error")
		return
	}
	if result.Code == 11217 {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	if result.Code != 0 || result.Data.AccessToken == "" {
		message := result.Msg
		if message == "" {
			message = "CodeBuddy did not return an access token"
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": message})
		return
	}
	account, err := s.addCodeBuddyAccount(result.Data.AccessToken, result.Data.RefreshToken, result.Data.ExpiresIn)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save CodeBuddy credentials: "+err.Error(), "store_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "connected", "account": codeBuddyAccountViewOf(account)})
}

func (s *Server) codeBuddyAccountsLocked() ([]codeBuddyAccount, error) {
	raw := s.store.GetSetting("codebuddy_accounts")
	if raw != "" {
		var accounts []codeBuddyAccount
		if err := json.Unmarshal([]byte(raw), &accounts); err != nil {
			return nil, fmt.Errorf("read CodeBuddy accounts: %w", err)
		}
		return accounts, nil
	}
	access := strings.TrimSpace(s.store.GetSetting("codebuddy_access_token"))
	if access == "" {
		return nil, nil
	}
	expiresAt, _ := strconv.ParseInt(s.store.GetSetting("codebuddy_expires_at"), 10, 64)
	return []codeBuddyAccount{{
		ID: "legacy", Name: "CodeBuddy account 1", AccessToken: access,
		RefreshToken: strings.TrimSpace(s.store.GetSetting("codebuddy_refresh_token")),
		ExpiresAt:    expiresAt, Active: true, AddedAt: time.Now().Unix(),
	}}, nil
}

func (s *Server) saveCodeBuddyAccountsLocked(accounts []codeBuddyAccount) error {
	data, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	if err := s.store.SetSetting("codebuddy_accounts", string(data)); err != nil {
		return err
	}
	for _, key := range []string{"codebuddy_access_token", "codebuddy_refresh_token", "codebuddy_expires_at"} {
		if err := s.store.SetSetting(key, ""); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) addCodeBuddyAccount(access, refresh string, expiresIn int64) (codeBuddyAccount, error) {
	codeBuddyRefreshMu.Lock()
	defer codeBuddyRefreshMu.Unlock()
	accounts, err := s.codeBuddyAccountsLocked()
	if err != nil {
		return codeBuddyAccount{}, err
	}
	id, err := newCodeBuddyAccountID()
	if err != nil {
		return codeBuddyAccount{}, err
	}
	if expiresIn <= 0 {
		expiresIn = 24 * 60 * 60
	}
	email := codeBuddyTokenEmail(access)
	account := codeBuddyAccount{
		ID: id, Email: email, Name: firstNonEmpty(email, fmt.Sprintf("CodeBuddy account %d", len(accounts)+1)),
		AccessToken: access, RefreshToken: refresh,
		ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(), AddedAt: time.Now().Unix(),
		Active: len(accounts) == 0,
	}
	accounts = append(accounts, account)
	if err := s.saveCodeBuddyAccountsLocked(accounts); err != nil {
		return codeBuddyAccount{}, err
	}
	return account, nil
}

func newCodeBuddyAccountID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func codeBuddyTokenEmail(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	for _, key := range []string{"email", "preferred_username", "user_name"} {
		if value, ok := claims[key].(string); ok && strings.Contains(value, "@") {
			return value
		}
	}
	return ""
}

func codeBuddyAccountViewOf(account codeBuddyAccount) codeBuddyAccountView {
	return codeBuddyAccountView{ID: account.ID, Name: account.Name, Email: account.Email, Active: account.Active, AddedAt: account.AddedAt}
}

func (s *Server) listCodeBuddyAccountViews() ([]codeBuddyAccountView, error) {
	codeBuddyRefreshMu.Lock()
	defer codeBuddyRefreshMu.Unlock()
	accounts, err := s.codeBuddyAccountsLocked()
	if err != nil {
		return nil, err
	}
	views := make([]codeBuddyAccountView, 0, len(accounts))
	for _, account := range accounts {
		views = append(views, codeBuddyAccountViewOf(account))
	}
	return views, nil
}

func (s *Server) selectCodeBuddyAccount(id string) error {
	codeBuddyRefreshMu.Lock()
	defer codeBuddyRefreshMu.Unlock()
	accounts, err := s.codeBuddyAccountsLocked()
	if err != nil {
		return err
	}
	found := false
	for i := range accounts {
		accounts[i].Active = accounts[i].ID == id
		found = found || accounts[i].Active
	}
	if !found {
		return errors.New("CodeBuddy account not found")
	}
	return s.saveCodeBuddyAccountsLocked(accounts)
}

func (s *Server) deleteCodeBuddyAccount(id string) error {
	codeBuddyRefreshMu.Lock()
	defer codeBuddyRefreshMu.Unlock()
	accounts, err := s.codeBuddyAccountsLocked()
	if err != nil {
		return err
	}
	filtered := make([]codeBuddyAccount, 0, len(accounts))
	found, removedActive := false, false
	for _, account := range accounts {
		if account.ID == id {
			found, removedActive = true, account.Active
			continue
		}
		filtered = append(filtered, account)
	}
	if !found {
		return errors.New("CodeBuddy account not found")
	}
	if removedActive && len(filtered) > 0 {
		filtered[0].Active = true
	}
	return s.saveCodeBuddyAccountsLocked(filtered)
}

func (s *Server) handleCodeBuddyAccounts(w http.ResponseWriter, r *http.Request) {
	views, err := s.listCodeBuddyAccountViews()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "store_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views})
}

func (s *Server) handleSelectCodeBuddyAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.selectCodeBuddyAccount(r.PathValue("id")); err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "CodeBuddy account not found" {
			status = http.StatusNotFound
		}
		writeAPIError(w, status, err.Error(), "account_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "active_account": r.PathValue("id")})
}

func (s *Server) handleDeleteCodeBuddyAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteCodeBuddyAccount(r.PathValue("id")); err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "CodeBuddy account not found" {
			status = http.StatusNotFound
		}
		writeAPIError(w, status, err.Error(), "account_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

type codeBuddyQuotaBucket struct {
	Used      float64    `json:"used"`
	Total     float64    `json:"total"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	Recurring bool       `json:"recurring"`
	Unlimited bool       `json:"unlimited"`
}

type codeBuddyQuotaAccount struct {
	ID      string                          `json:"id"`
	Name    string                          `json:"name"`
	Email   string                          `json:"email,omitempty"`
	Active  bool                            `json:"active"`
	Plan    string                          `json:"plan,omitempty"`
	Quotas  map[string]codeBuddyQuotaBucket `json:"quotas,omitempty"`
	Message string                          `json:"message,omitempty"`
}

func (s *Server) handleCodeBuddyQuota(w http.ResponseWriter, r *http.Request) {
	codeBuddyRefreshMu.Lock()
	accounts, err := s.codeBuddyAccountsLocked()
	codeBuddyRefreshMu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "store_error")
		return
	}
	results := make([]codeBuddyQuotaAccount, 0, len(accounts))
	for _, account := range accounts {
		view := codeBuddyQuotaAccount{ID: account.ID, Name: account.Name, Email: account.Email, Active: account.Active}
		token, id, tokenErr := s.codeBuddyTokenForAccount(r.Context(), account.ID)
		if tokenErr != nil {
			view.Message = tokenErr.Error()
			results = append(results, view)
			continue
		}
		quota, message, err := s.fetchCodeBuddyQuota(r.Context(), token)
		if err != nil {
			view.Message = err.Error()
		} else {
			view.Plan, view.Quotas, view.Message = quota.Plan, quota.Quotas, message
		}
		if id != "" && view.ID == "" {
			view.ID = id
		}
		results = append(results, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": results, "updated_at": time.Now().UTC()})
}

type codeBuddyQuotaReport struct {
	Plan   string                          `json:"plan"`
	Quotas map[string]codeBuddyQuotaBucket `json:"quotas"`
}

func (s *Server) fetchCodeBuddyQuota(ctx context.Context, token string) (codeBuddyQuotaReport, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codeBuddyBaseURL+"/v2/billing/meter/get-user-resource", strings.NewReader("{}"))
	if err != nil {
		return codeBuddyQuotaReport{}, "", err
	}
	req.Header = codeBuddyAuthHeaders("www.codebuddy.ai")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-IDE-Type", "IDE")
	req.Header.Set("X-IDE-Name", "IDE")
	req.Header.Set("X-CodeBuddy-Request", "1")
	resp, err := s.codeBuddyHTTPClient().Do(req)
	if err != nil {
		return codeBuddyQuotaReport{}, "", fmt.Errorf("CodeBuddy quota request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return codeBuddyQuotaReport{}, "", errors.New("CodeBuddy credential invalid or expired")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return codeBuddyQuotaReport{}, "", fmt.Errorf("CodeBuddy quota API error (%d)", resp.StatusCode)
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []map[string]any `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result); err != nil {
		return codeBuddyQuotaReport{}, "", fmt.Errorf("invalid CodeBuddy quota response: %w", err)
	}
	if result.Code != 0 {
		return codeBuddyQuotaReport{}, "", fmt.Errorf("CodeBuddy quota error: %s", firstNonEmpty(result.Msg, "unknown"))
	}
	packages := result.Data.Response.Data.Accounts
	if len(packages) == 0 {
		return codeBuddyQuotaReport{}, "CodeBuddy connected. No credit package found.", nil
	}
	var recurring, bonus []map[string]any
	for _, item := range packages {
		end := codeBuddyTime(item["CycleEndTime"])
		deductionEnd := codeBuddyTime(item["DeductionEndTime"])
		if end != nil && deductionEnd != nil && end.Sub(*deductionEnd) > 48*time.Hour {
			recurring = append(recurring, item)
		} else {
			bonus = append(bonus, item)
		}
	}
	sort.SliceStable(recurring, func(i, j int) bool { return codeBuddySortTime(recurring[i]) < codeBuddySortTime(recurring[j]) })
	sort.SliceStable(bonus, func(i, j int) bool { return codeBuddySortTime(bonus[i]) < codeBuddySortTime(bonus[j]) })
	quotas := make(map[string]codeBuddyQuotaBucket)
	countByPeriod := make(map[string]int)
	for _, item := range recurring {
		name := codeBuddyPeriod(item)
		countByPeriod[name]++
		if countByPeriod[name] > 1 {
			name = fmt.Sprintf("%s %d", name, countByPeriod[name])
		}
		quotas[name] = codeBuddyQuotaBucket{
			Used:    codeBuddyNumber(item["CycleCapacityUsedPrecise"], item["CycleCapacityUsed"]),
			Total:   codeBuddyNumber(item["CycleCapacitySizePrecise"], item["CycleCapacitySize"]),
			ResetAt: codeBuddyTime(item["CycleEndTime"]), Recurring: true,
		}
	}
	for i, item := range bonus {
		quotas[fmt.Sprintf("Bonus Pack %d", i+1)] = codeBuddyQuotaBucket{
			Used:    codeBuddyNumber(item["CapacityUsedPrecise"], item["CapacityUsed"]),
			Total:   codeBuddyNumber(item["CapacitySizePrecise"], item["CapacitySize"]),
			ResetAt: codeBuddyTime(item["CycleEndTime"]),
		}
	}
	plan := "CodeBuddy"
	if first := firstNonEmpty(codeBuddyString(recurring, "PackageName"), codeBuddyString(recurring, "SubProductName"), codeBuddyString(bonus, "PackageName"), codeBuddyString(bonus, "SubProductName")); first != "" {
		plan = first
	}
	return codeBuddyQuotaReport{Plan: plan, Quotas: quotas}, "", nil
}

func codeBuddyNumber(preferred, fallback any) float64 {
	for _, value := range []any{preferred, fallback} {
		if value == nil {
			continue
		}
		if n, ok := value.(float64); ok {
			return n
		}
		if n, err := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(value)), 64); err == nil {
			return n
		}
	}
	return 0
}

func codeBuddyTime(value any) *time.Time {
	if value == nil {
		return nil
	}
	if n, ok := value.(float64); ok {
		if n > 1e12 {
			n /= 1000
		}
		t := time.Unix(int64(n), 0).UTC()
		return &t
	}
	str := strings.TrimSpace(fmt.Sprint(value))
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, str); err == nil {
			return &parsed
		}
	}
	if n, err := strconv.ParseInt(str, 10, 64); err == nil {
		if n > 1e12 {
			n /= 1000
		}
		t := time.Unix(n, 0).UTC()
		return &t
	}
	return nil
}

func codeBuddySortTime(item map[string]any) int64 {
	if t := codeBuddyTime(item["CycleEndTime"]); t != nil {
		return t.Unix()
	}
	return int64(^uint64(0) >> 1)
}

func codeBuddyPeriod(item map[string]any) string {
	start, end := codeBuddyTime(item["CycleStartTime"]), codeBuddyTime(item["CycleEndTime"])
	if start != nil && end != nil {
		days := end.Sub(*start).Hours() / 24
		if days <= 1.5 {
			return "Daily"
		}
		if days <= 10 {
			return "Weekly"
		}
	}
	return "Monthly"
}

func codeBuddyString(items []map[string]any, key string) string {
	if len(items) == 0 {
		return ""
	}
	value, ok := items[0][key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func (s *Server) handleCodeBuddyModels(w http.ResponseWriter, r *http.Request) {
	models := make([]map[string]any, 0, len(codeBuddyModels))
	for _, model := range codeBuddyModels {
		models = append(models, map[string]any{
			"id": "codebuddy/" + model.ID, "name": model.Name, "provider": ProviderCodeBuddy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "count": len(models), "source": "CodeBuddy Intl catalog from 9Router"})
}

func (s *Server) codeBuddyHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *Server) codeBuddyAPIKey() string {
	if k := s.store.GetSetting("codebuddy_api_key"); k != "" {
		return strings.TrimSpace(k)
	}
	return strings.TrimSpace(os.Getenv("CODEBUDDY_API_KEY"))
}

func (s *Server) codeBuddyAccessToken(ctx context.Context) (string, string, error) {
	return s.codeBuddyTokenForAccount(ctx, "")
}

func (s *Server) runCodeBuddyPlayground(ctx context.Context, model, prompt string) (string, int64, int64, error) {
	token, _, err := s.codeBuddyAccessToken(ctx)
	if err != nil {
		return "", 0, 0, err
	}

	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": prompt},
		},
		"stream": true,
	})
	if err != nil {
		return "", 0, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codeBuddyBaseURL+"/v2/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", codeBuddyUserAgent)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-IDE-Type", "IDE")
	req.Header.Set("X-IDE-Name", "IDE")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("CodeBuddy Intl request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return "", 0, 0, fmt.Errorf("CodeBuddy Intl returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var inTokens, outTokens int64
	var collected strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content any `json:"content"`
				} `json:"delta"`
				Message struct {
					Content any `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			content := chunk.Choices[0].Delta.Content
			if content == nil {
				content = chunk.Choices[0].Message.Content
			}
			switch value := content.(type) {
			case string:
				collected.WriteString(value)
			case []any:
				for _, part := range value {
					if obj, ok := part.(map[string]any); ok {
						if text, ok := obj["text"].(string); ok {
							collected.WriteString(text)
						}
					}
				}
			}
		}
		if chunk.Usage != nil {
			inTokens = chunk.Usage.PromptTokens
			outTokens = chunk.Usage.CompletionTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return "", inTokens, outTokens, fmt.Errorf("read CodeBuddy Intl stream: %w", err)
	}
	return collected.String(), inTokens, outTokens, nil
}

func (s *Server) codeBuddyTokenForAccount(ctx context.Context, accountID string) (string, string, error) {
	codeBuddyRefreshMu.Lock()
	defer codeBuddyRefreshMu.Unlock()

	accounts, err := s.codeBuddyAccountsLocked()
	if err != nil {
		return "", "", err
	}
	var account *codeBuddyAccount
	for i := range accounts {
		if (accountID != "" && accounts[i].ID == accountID) || (accountID == "" && accounts[i].Active) {
			account = &accounts[i]
			break
		}
	}
	if accountID != "" && account == nil {
		return "", "", errors.New("CodeBuddy account not found")
	}
	if account == nil && len(accounts) > 0 {
		account = &accounts[0]
	}
	if account == nil {
		if key := s.codeBuddyAPIKey(); key != "" {
			return key, "apikey", nil
		}
		return "", "", fmt.Errorf("CodeBuddy belum login; sambungkan akun CodeBuddy Intl dari tab Providers")
	}
	if account.AccessToken != "" && (account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-300) {
		return account.AccessToken, account.ID, nil
	}
	if account.RefreshToken == "" {
		if key := s.codeBuddyAPIKey(); key != "" {
			return key, "apikey", nil
		}
		return "", account.ID, fmt.Errorf("CodeBuddy account expired; reconnect this account")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codeBuddyBaseURL+"/v2/plugin/auth/token/refresh", strings.NewReader("{}"))
	if err != nil {
		return "", account.ID, err
	}
	req.Header = codeBuddyAuthHeaders("www.codebuddy.ai")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Refresh-Token", account.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	resp, err := s.codeBuddyHTTPClient().Do(req)
	if err != nil {
		return "", account.ID, fmt.Errorf("CodeBuddy token refresh failed: %w", err)
	}
	defer resp.Body.Close()
	var result codeBuddyTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", account.ID, fmt.Errorf("invalid CodeBuddy refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Code != 0 || result.Data.AccessToken == "" {
		message := result.Msg
		if message == "" {
			message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", account.ID, fmt.Errorf("CodeBuddy token refresh failed (%s); reconnect the account", message)
	}
	account.AccessToken = result.Data.AccessToken
	account.RefreshToken = firstNonEmpty(result.Data.RefreshToken, account.RefreshToken)
	if result.Data.ExpiresIn <= 0 {
		result.Data.ExpiresIn = 24 * 60 * 60
	}
	account.ExpiresAt = time.Now().Add(time.Duration(result.Data.ExpiresIn) * time.Second).Unix()
	for i := range accounts {
		if accounts[i].ID == account.ID {
			accounts[i] = *account
		}
	}
	if err := s.saveCodeBuddyAccountsLocked(accounts); err != nil {
		return "", account.ID, err
	}
	return result.Data.AccessToken, account.ID, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (s *Server) handleCodeBuddyProxy(w http.ResponseWriter, r *http.Request, surface string, raw []byte, model, requestedModel string, key *store.APIKey, clientDesc string) {
	started := time.Now()
	base := requestRecord{
		Key: key, Surface: surface, Provider: ProviderCodeBuddy, Model: model,
		RequestedModel: requestedModel, Started: started, Client: clientDesc,
		Relay: "codebuddy-intl",
	}
	if surface != "openai" {
		s.codeBuddyError(w, base, http.StatusBadRequest, fmt.Errorf("CodeBuddy Intl saat ini mendukung surface OpenAI-compatible /v1/chat/completions"))
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request JSON: "+err.Error(), "invalid_request_error")
		return
	}
	body["model"] = model
	payload, err := json.Marshal(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "encode request error: "+err.Error(), "invalid_request_error")
		return
	}
	token, accountID, err := s.codeBuddyAccessToken(r.Context())
	if err != nil {
		s.codeBuddyError(w, base, http.StatusUnauthorized, err)
		return
	}
	base.Relay = "codebuddy-intl:" + accountID
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, codeBuddyBaseURL+"/v2/chat/completions", bytes.NewReader(payload))
	if err != nil {
		s.codeBuddyError(w, base, http.StatusInternalServerError, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", codeBuddyUserAgent)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-IDE-Type", "IDE")
	req.Header.Set("X-IDE-Name", "IDE")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		s.codeBuddyError(w, base, http.StatusBadGateway, fmt.Errorf("CodeBuddy Intl request failed: %w", err))
		return
	}
	defer resp.Body.Close()
	base.Status = resp.StatusCode
	for _, name := range []string{"Content-Type", "Cache-Control", "X-Accel-Buffering"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		base.Err = string(detail)
		_, _ = w.Write(detail)
		s.recordRequest(base)
		return
	}
	if err := copyAndFlush(w, resp.Body); err != nil {
		base.Err = "stream copy error: " + err.Error()
	}
	s.recordRequest(base)
}

func (s *Server) codeBuddyError(w http.ResponseWriter, base requestRecord, status int, err error) {
	base.Status = status
	base.Err = err.Error()
	s.recordRequest(base)
	writeAPIError(w, status, base.Err, "upstream_error")
}

func copyAndFlush(dst http.ResponseWriter, src io.Reader) error {
	buf := make([]byte, 32*1024)
	flusher, _ := dst.(http.Flusher)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
