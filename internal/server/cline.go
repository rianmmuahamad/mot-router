package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rian/mot-router/internal/store"
)

const defaultClineBaseURL = "https://api.cline.bot/api/v1"

func (s *Server) clineBaseURL() string {
	if u := s.store.GetSetting("cline_base_url"); u != "" {
		return strings.TrimRight(u, "/")
	}
	if env := os.Getenv("CLINE_BASE_URL"); env != "" {
		return strings.TrimRight(env, "/")
	}
	return defaultClineBaseURL
}

func (s *Server) clineAPIKey() string {
	if k := s.store.GetSetting("cline_api_key"); k != "" {
		return strings.TrimSpace(k)
	}
	if env := os.Getenv("CLINE_API_KEY"); env != "" {
		return strings.TrimSpace(env)
	}
	return ""
}

func (s *Server) clineClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

func (s *Server) handleClineProxy(w http.ResponseWriter, r *http.Request, surface string, raw []byte, model, requestedModel string, key *store.APIKey, clientDesc string) {
	started := time.Now()
	apiKey := s.clineAPIKey()

	base := requestRecord{
		Key:            key,
		Surface:        surface,
		Provider:       ProviderCline,
		Model:          model,
		RequestedModel: requestedModel,
		Started:        started,
		Client:         clientDesc,
	}

	if apiKey == "" {
		base.Status = http.StatusBadGateway
		base.Err = "Cline provider is not configured (missing API key/token)"
		s.recordRequest(base)
		writeAPIError(w, http.StatusBadGateway, "Cline provider is not configured. Please add your Cline token in Providers tab.", "provider_not_configured")
		return
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request JSON: "+err.Error(), "invalid_request_error")
		return
	}

	clientStream := false
	if v, ok := parsed["stream"].(bool); ok && v {
		clientStream = true
	}

	// Always send stream:true upstream: api.cline.bot only answers SSE.
	parsed["stream"] = true
	parsed["model"] = model
	if so, ok := parsed["stream_options"].(map[string]any); !ok || so == nil {
		parsed["stream_options"] = map[string]any{"include_usage": true}
	}

	payloadBytes, err := json.Marshal(parsed)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "encode request error: "+err.Error(), "invalid_request_error")
		return
	}

	destURL := s.clineBaseURL() + "/chat/completions"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, destURL, bytes.NewReader(payloadBytes))
	if err != nil {
		base.Status = http.StatusInternalServerError
		base.Err = "build request error: " + err.Error()
		s.recordRequest(base)
		writeAPIError(w, http.StatusInternalServerError, base.Err, "internal_error")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+apiKey)
	upReq.Header.Set("X-CLIENT-TYPE", "cline-cli")
	upReq.Header.Set("User-Agent", "cline-cli/3.0.61")

	resp, err := s.clineClient().Do(upReq)
	if err != nil {
		base.Status = http.StatusBadGateway
		base.Err = "upstream error: " + err.Error()
		s.recordRequest(base)
		writeAPIError(w, http.StatusBadGateway, base.Err, "upstream_error")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		base.Status = resp.StatusCode
		base.Err = string(detail)
		s.recordRequest(base)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(detail)
		return
	}

	if clientStream {
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

	var inTokens, outTokens int64
	var collected strings.Builder
	var lastID string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}
		if chunk.ID != "" {
			lastID = chunk.ID
		}
		if len(chunk.Choices) > 0 {
			collected.WriteString(chunk.Choices[0].Delta.Content)
		}
		if chunk.Usage != nil {
			inTokens = chunk.Usage.PromptTokens
			outTokens = chunk.Usage.CompletionTokens
		}
	}
	base.Status = http.StatusOK
	base.In = inTokens
	base.Out = outTokens
	s.recordRequest(base)

	if surface == "anthropic" {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":    "msg_" + lastID,
			"type":  "message",
			"role":  "assistant",
			"model": requestedModel,
			"content": []map[string]any{
				{"type": "text", "text": collected.String()},
			},
			"stop_reason": "end_turn",
			"usage": map[string]int64{
				"input_tokens":  inTokens,
				"output_tokens": outTokens,
			},
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lastID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   requestedModel,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": collected.String(),
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int64{
			"prompt_tokens":     inTokens,
			"completion_tokens": outTokens,
			"total_tokens":      inTokens + outTokens,
		},
	})
}

func (s *Server) runClinePlayground(ctx context.Context, model, prompt string) (string, int64, int64, error) {
	apiKey := s.clineAPIKey()
	if apiKey == "" {
		return "", 0, 0, fmt.Errorf("no Cline API token is configured. Please configure it in Providers tab")
	}

	payloadBytes, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
		"stream": true,
	})
	if err != nil {
		return "", 0, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.clineBaseURL()+"/chat/completions", bytes.NewReader(payloadBytes))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-CLIENT-TYPE", "cline-cli")
	req.Header.Set("User-Agent", "cline-cli/3.0.61")

	resp, err := s.clineClient().Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(resp.Body)
		return "", 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(detail))
	}

	var inTokens, outTokens int64
	var collected strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			collected.WriteString(chunk.Choices[0].Delta.Content)
		}
		if chunk.Usage != nil {
			inTokens = chunk.Usage.PromptTokens
			outTokens = chunk.Usage.CompletionTokens
		}
	}
	return collected.String(), inTokens, outTokens, nil
}
