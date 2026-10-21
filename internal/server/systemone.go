package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// handleSystemOne routes structured decision/evaluation requests to Zen's
// /systemone endpoint (specifically for TypeSafe AI's Jev model).
func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	keyStr := extractKey(r)
	if keyStr == "" {
		writeAPIError(w, http.StatusUnauthorized, "missing API key", "authentication_error")
		return
	}
	key, err := s.store.FindValidKey(keyStr)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	if key == nil {
		writeAPIError(w, http.StatusUnauthorized, "invalid API key", "authentication_error")
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "cannot read request body", "invalid_request_error")
		return
	}

	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request_error")
		return
	}

	model, _ := req["model"].(string)
	if model == "" || strings.Contains(strings.ToLower(model), "jev") {
		model = "jev-1.13-free"
		req["model"] = model
	}

	payload, err := json.Marshal(req)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "encode error: "+err.Error(), "invalid_request_error")
		return
	}

	upstreamURL := s.zen.EffectiveBaseURL(model) + "/systemone"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "build request error: "+err.Error(), "internal_error")
		return
	}

	sessionSeed := fmt.Sprintf("%d:%s:%s", key.ID, r.Header.Get("User-Agent"), model)
	for k, v := range s.zen.Headers(model, sessionSeed) {
		upReq.Header.Set(k, v)
	}

	client := s.zen.HTTP
	if s.mihomoEnabled() {
		client = s.mihomoClient().ProxyClient()
	}

	started := time.Now()
	resp, err := client.Do(upReq)

	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "upstream error: "+err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "read upstream error: "+err.Error(), "upstream_error")
		return
	}

	var inTokens, outTokens int64
	var parsed struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err == nil {
		inTokens = parsed.Usage.InputTokens
		outTokens = parsed.Usage.OutputTokens
	}

	s.recordRequest(requestRecord{
		Key:            key,
		Surface:        "systemone",
		Provider:       ProviderZen,
		Model:          model,
		RequestedModel: model,
		Started:        started,
		Status:         resp.StatusCode,
		In:             inTokens,
		Out:            outTokens,
		Client:         describeClient(r),
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}
