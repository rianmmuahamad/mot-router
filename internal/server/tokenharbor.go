package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rian/mot-router/internal/store"
	"github.com/rian/mot-router/internal/zen"
)

var (
	thMu        sync.Mutex
	thPenalties = make(map[string]time.Time)
	thKeyIndex  int

	thQwenMu        sync.Mutex
	thQwenPenalties = make(map[string]time.Time)
	thQwenKeyIndex  int
)

const (
	defaultTokenHarborBase = "https://tokenharbor.ai/v1"
	thPenaltyDuration      = 30 * time.Minute
)

func (s *Server) tokenHarborBaseURL() string {
	if u := s.store.GetSetting("tokenharbor_base_url"); u != "" {
		return strings.TrimRight(u, "/")
	}
	if env := os.Getenv("TOKENHARBOR_BASE_URL"); env != "" {
		return strings.TrimRight(env, "/")
	}
	return defaultTokenHarborBase
}

func parseBulkKeys(val string) []string {
	raw := strings.ReplaceAll(val, ",", "\n")
	lines := strings.Split(raw, "\n")
	var keys []string
	seen := make(map[string]bool)
	for _, l := range lines {
		k := strings.TrimSpace(l)
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

func (s *Server) tokenHarborKeys() []string {
	val := s.store.GetSetting("tokenharbor_api_key")
	if val == "" {
		val = os.Getenv("TOKENHARBOR_API_KEY")
	}
	if val == "" {
		return nil
	}
	return parseBulkKeys(val)
}

func (s *Server) tokenHarborQwenKeys() []string {
	val := s.store.GetSetting("tokenharbor_qwen_api_key")
	if val == "" {
		val = os.Getenv("TOKENHARBOR_QWEN_API_KEY")
	}
	if val == "" {
		return s.tokenHarborKeys()
	}
	return parseBulkKeys(val)
}

func (s *Server) tokenHarborQwenDedicated() bool {
	val := s.store.GetSetting("tokenharbor_qwen_api_key")
	if val == "" {
		val = os.Getenv("TOKENHARBOR_QWEN_API_KEY")
	}
	return strings.TrimSpace(val) != ""
}

func isQwenModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "qwen")
}

func pickKeyRoundRobin(keys []string, penalties map[string]time.Time, keyIndex *int, excludeKey string) (string, bool, time.Duration) {
	if len(keys) == 0 {
		return "", false, 0
	}

	now := time.Now()
	for k, exp := range penalties {
		if now.After(exp) {
			delete(penalties, k)
		}
	}

	for i := range len(keys) {
		idx := (*keyIndex + i) % len(keys)
		candidate := keys[idx]
		if candidate == excludeKey && len(keys) > 1 {
			continue
		}
		if exp, penalized := penalties[candidate]; !penalized || now.After(exp) {
			*keyIndex = (idx + 1) % len(keys)
			return candidate, true, 0
		}
	}

	var bestKey string
	var earliestExp time.Time
	for _, candidate := range keys {
		if candidate == excludeKey && len(keys) > 1 {
			continue
		}
		exp := penalties[candidate]
		if bestKey == "" || exp.Before(earliestExp) {
			bestKey = candidate
			earliestExp = exp
		}
	}

	if bestKey != "" {
		wait := time.Until(earliestExp)
		if wait < 0 {
			wait = 0
		}
		return bestKey, false, wait
	}

	return keys[0], false, 0
}

func (s *Server) pickTokenHarborKey(excludeKey string) (string, bool, time.Duration) {
	thMu.Lock()
	defer thMu.Unlock()
	return pickKeyRoundRobin(s.tokenHarborKeys(), thPenalties, &thKeyIndex, excludeKey)
}

func (s *Server) pickTokenHarborQwenKey(excludeKey string) (string, bool, time.Duration) {
	thQwenMu.Lock()
	defer thQwenMu.Unlock()
	return pickKeyRoundRobin(s.tokenHarborQwenKeys(), thQwenPenalties, &thQwenKeyIndex, excludeKey)
}

func penalizeTokenHarborKey(key string) {
	if key == "" {
		return
	}
	thMu.Lock()
	defer thMu.Unlock()
	thPenalties[key] = time.Now().Add(thPenaltyDuration)
	log.Printf("[tokenharbor] key %s penalized for 30m due to rate limit (429)", redactKey(key))
}

func penalizeTokenHarborQwenKey(key string) {
	if key == "" {
		return
	}
	thQwenMu.Lock()
	defer thQwenMu.Unlock()
	thQwenPenalties[key] = time.Now().Add(thPenaltyDuration)
	log.Printf("[tokenharbor-qwen] key %s penalized for 30m due to rate limit (429)", redactKey(key))
}

func clearTokenHarborPenalty(key string) {
	if key == "" {
		return
	}
	thMu.Lock()
	defer thMu.Unlock()
	delete(thPenalties, key)
}

func clearTokenHarborQwenPenalty(key string) {
	if key == "" {
		return
	}
	thQwenMu.Lock()
	defer thQwenMu.Unlock()
	delete(thQwenPenalties, key)
}

func (s *Server) tokenHarborClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 nil,
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

func isCohortPause(status int, detail []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	return strings.Contains(string(detail), "cohort_refusal_breaker")
}

// isKeyRevokedOrDisabled reports whether an error response means this specific API key
// cannot be used (consent changed, free models disabled, key revoked, unauthorized).
// These errors are key-specific and MUST trigger failover to the next key.
func isKeyRevokedOrDisabled(status int, detail []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if status == http.StatusForbidden {
		s := string(detail)
		if strings.Contains(s, "free_models_disabled") ||
			strings.Contains(s, "Free-model consent") ||
			strings.Contains(s, "consent has changed") ||
			strings.Contains(s, "invalid_api_key") ||
			strings.Contains(s, "account_suspended") ||
			strings.Contains(s, "request was refused") ||
			strings.Contains(s, "contact support from the email address") ||
			strings.Contains(s, "terms_violation") ||
			strings.Contains(s, "account_restricted") {
			return true
		}
	}
	return false
}
func (s *Server) handleTokenHarborProxy(w http.ResponseWriter, r *http.Request, surface string, raw []byte, model, requestedModel string, key *store.APIKey, clientDesc string) {
	started := time.Now()

	qwenOnly := isQwenModel(model)
	var keys []string
	if qwenOnly {
		keys = s.tokenHarborQwenKeys()
	} else {
		keys = s.tokenHarborKeys()
	}
	if len(keys) == 0 {
		base := requestRecord{
			Key:            key,
			Surface:        surface,
			Provider:       ProviderTokenHarbor,
			Model:          model,
			RequestedModel: requestedModel,
			Started:        started,
			Status:         http.StatusBadGateway,
			Err:            "Token Harbor is not configured (missing API key)",
			Client:         clientDesc,
		}
		s.recordRequest(base)
		writeAPIError(w, http.StatusBadGateway, "Token Harbor is not configured. Please add an API key in the Providers tab.", "provider_not_configured")
		return
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request JSON: "+err.Error(), "invalid_request_error")
		return
	}

	var payloadBytes []byte
	isStream := false

	if _, hasInput := parsed["input"]; hasInput {
		chatPayload := zen.ConvertToChatPayload(model, parsed)
		chatPayload["model"] = model
		b, err := json.Marshal(chatPayload)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "convert request error: "+err.Error(), "invalid_request_error")
			return
		}
		payloadBytes = b
		isStream = true
	} else {
		parsed["model"] = model
		if v, ok := parsed["stream"].(bool); ok && v {
			isStream = true
			if so, ok := parsed["stream_options"].(map[string]any); !ok || so == nil {
				parsed["stream_options"] = map[string]any{"include_usage": true}
			}
		}
		b, err := json.Marshal(parsed)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "encode request error: "+err.Error(), "invalid_request_error")
			return
		}
		payloadBytes = b
	}

	destURL := s.tokenHarborBaseURL() + "/chat/completions"
	base := requestRecord{
		Key:            key,
		Surface:        surface,
		Provider:       ProviderTokenHarbor,
		Model:          model,
		RequestedModel: requestedModel,
		Started:        started,
		Client:         clientDesc,
	}

	maxAttempts := len(keys)
	if maxAttempts > 6 {
		maxAttempts = 6
	}

	var lastResp *http.Response
	var lastDetail []byte
	var lastKey string

	for range maxAttempts {
		var currentKey string
		if qwenOnly {
			currentKey, _, _ = s.pickTokenHarborQwenKey(lastKey)
		} else {
			currentKey, _, _ = s.pickTokenHarborKey(lastKey)
		}
		lastKey = currentKey

		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, destURL, bytes.NewReader(payloadBytes))
		if err != nil {
			base.Status = http.StatusInternalServerError
			base.Err = "build request error: " + err.Error()
			s.recordRequest(base)
			writeAPIError(w, http.StatusInternalServerError, base.Err, "internal_error")
			return
		}

		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+currentKey)
		upReq.Header.Set("User-Agent", "mot-router/1.0 (TokenHarbor-Gateway)")

		resp, err := s.tokenHarborClient().Do(upReq)
		if err != nil {
			base.Status = http.StatusBadGateway
			base.Err = "upstream error: " + err.Error()
			s.recordRequest(base)
			writeAPIError(w, http.StatusBadGateway, base.Err, "upstream_error")
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastDetail = detail
			if qwenOnly {
				penalizeTokenHarborQwenKey(currentKey)
				log.Printf("[tokenharbor-qwen] key %s limit reached, failing over to next Qwen key...", redactKey(currentKey))
			} else {
				penalizeTokenHarborKey(currentKey)
				log.Printf("[tokenharbor] key %s limit reached, failing over to next key...", redactKey(currentKey))
			}
			continue
		}

		if qwenOnly {
			clearTokenHarborQwenPenalty(currentKey)
		} else {
			clearTokenHarborPenalty(currentKey)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			resp.Body.Close()
			if isKeyRevokedOrDisabled(resp.StatusCode, detail) {
				lastDetail = detail
				if qwenOnly {
					penalizeTokenHarborQwenKey(currentKey)
					log.Printf("[tokenharbor-qwen] key %s consent/auth failure (HTTP %d), failing over...", redactKey(currentKey), resp.StatusCode)
				} else {
					penalizeTokenHarborKey(currentKey)
					log.Printf("[tokenharbor] key %s consent/auth failure (HTTP %d), failing over...", redactKey(currentKey), resp.StatusCode)
				}
				continue
			}
			if isCohortPause(resp.StatusCode, detail) {
				base.Status = resp.StatusCode
				base.Err = string(detail)
				s.recordRequest(base)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				w.Write(detail)
				return
			}
			base.Status = resp.StatusCode
			base.Err = string(detail)
			s.recordRequest(base)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(detail)
			return
		}

		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)

			var inTokens, outTokens int64
			scanner := bufio.NewScanner(resp.Body)
			flusher, _ := w.(http.Flusher)

			for scanner.Scan() {
				line := scanner.Text()
				fmt.Fprintf(w, "%s\n", line)
				if flusher != nil {
					flusher.Flush()
				}

				if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
					var chunk struct {
						Usage *struct {
							PromptTokens     int64 `json:"prompt_tokens"`
							CompletionTokens int64 `json:"completion_tokens"`
						} `json:"usage"`
					}
					if err := json.Unmarshal([]byte(line[6:]), &chunk); err == nil && chunk.Usage != nil {
						if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
							inTokens = chunk.Usage.PromptTokens
							outTokens = chunk.Usage.CompletionTokens
						}
					}
				}
			}
			resp.Body.Close()
			fmt.Fprintf(w, "\n")
			if flusher != nil {
				flusher.Flush()
			}

			base.Status = http.StatusOK
			base.In = inTokens
			base.Out = outTokens
			s.recordRequest(base)
			return
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			base.Status = http.StatusBadGateway
			base.Err = "read upstream response error: " + err.Error()
			s.recordRequest(base)
			writeAPIError(w, http.StatusBadGateway, base.Err, "upstream_error")
			return
		}

		var inTokens, outTokens int64
		var completion struct {
			Usage struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(bodyBytes, &completion); err == nil {
			inTokens = completion.Usage.PromptTokens
			outTokens = completion.Usage.CompletionTokens
		}

		base.Status = http.StatusOK
		base.In = inTokens
		base.Out = outTokens

		if surface == "anthropic" {
			var openAIResp struct {
				ID      string `json:"id"`
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
				Usage struct {
					PromptTokens     int64 `json:"prompt_tokens"`
					CompletionTokens int64 `json:"completion_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(bodyBytes, &openAIResp); err == nil && len(openAIResp.Choices) > 0 {
				writeJSON(w, http.StatusOK, map[string]any{
					"id":    "msg_" + openAIResp.ID,
					"type":  "message",
					"role":  "assistant",
					"model": requestedModel,
					"content": []map[string]any{
						{"type": "text", "text": openAIResp.Choices[0].Message.Content},
					},
					"stop_reason": "end_turn",
					"usage": map[string]int64{
						"input_tokens":  openAIResp.Usage.PromptTokens,
						"output_tokens": openAIResp.Usage.CompletionTokens,
					},
				})
				s.recordRequest(base)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(bodyBytes)
		s.recordRequest(base)
		return
	}

	status := http.StatusTooManyRequests
	if lastResp != nil && lastResp.StatusCode != 0 {
		status = lastResp.StatusCode
	}
	base.Status = status
	if len(lastDetail) == 0 {
		lastDetail = []byte(`{"error":{"message":"All Token Harbor API keys are rate limited. Please add more keys in Providers.","type":"rate_limit_exhausted"}}`)
	}
	base.Err = string(lastDetail)
	s.recordRequest(base)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(lastDetail)
}

func (s *Server) runTokenHarborPlayground(ctx context.Context, model, prompt string) (string, int64, int64, error) {
	var keys []string
	qwenOnly := isQwenModel(model)
	if qwenOnly {
		keys = s.tokenHarborQwenKeys()
	} else {
		keys = s.tokenHarborKeys()
	}
	if len(keys) == 0 {
		return "", 0, 0, fmt.Errorf("no Token Harbor API key is configured. Please configure it in Providers tab")
	}

	payloadBytes, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return "", 0, 0, err
	}

	destURL := s.tokenHarborBaseURL() + "/chat/completions"

	maxAttempts := len(keys)
	if maxAttempts > 6 {
		maxAttempts = 6
	}

	var lastErr error
	var lastKey string

	for range maxAttempts {
		var currentKey string
		if qwenOnly {
			currentKey, _, _ = s.pickTokenHarborQwenKey(lastKey)
		} else {
			currentKey, _, _ = s.pickTokenHarborKey(lastKey)
		}
		lastKey = currentKey

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, destURL, bytes.NewReader(payloadBytes))
		if err != nil {
			return "", 0, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+currentKey)

		client := &http.Client{
			Timeout: 45 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			if qwenOnly {
				penalizeTokenHarborQwenKey(currentKey)
			} else {
				penalizeTokenHarborKey(currentKey)
			}
			lastErr = fmt.Errorf("rate limited on key %s", redactKey(currentKey))
			continue
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if isKeyRevokedOrDisabled(resp.StatusCode, bodyBytes) {
				if qwenOnly {
					penalizeTokenHarborQwenKey(currentKey)
				} else {
					penalizeTokenHarborKey(currentKey)
				}
				lastErr = fmt.Errorf("key %s disabled/consent error (HTTP %d)", redactKey(currentKey), resp.StatusCode)
				continue
			}
			if isCohortPause(resp.StatusCode, bodyBytes) {
				return "", 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
			}
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(bodyBytes))
			continue
		}

		var completion struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(bodyBytes, &completion); err == nil && len(completion.Choices) > 0 {
			return completion.Choices[0].Message.Content, completion.Usage.PromptTokens, completion.Usage.CompletionTokens, nil
		}

		return string(bodyBytes), 0, 0, nil
	}

	return "", 0, 0, fmt.Errorf("all Token Harbor keys exhausted: %v", lastErr)
}

func redactKey(k string) string {
	if len(k) <= 8 {
		if k == "" {
			return ""
		}
		return "***"
	}
	return k[:6] + "..." + k[len(k)-4:]
}
