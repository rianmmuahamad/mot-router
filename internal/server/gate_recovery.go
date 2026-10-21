package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/translate"
	"github.com/rian/mot-router/internal/zen"
)

type gateRebuildKey struct{}

func (s *Server) translateZenBody(raw []byte, surface, model string) (map[string]any, *zen.ToolInjection, error) {
	return s.translateZenBodyWithProfile(raw, surface, model, s.zen.CurrentGate())
}

func (s *Server) translateZenBodyWithProfile(raw []byte, surface, model string, profile *zen.GateProfile) (map[string]any, *zen.ToolInjection, error) {
	if surface == "anthropic" {
		var req translate.AnthropicRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, nil, fmt.Errorf("invalid JSON body: %w", err)
		}
		result, err := translate.AnthropicToZen(&req, model, profile)
		if err != nil {
			return nil, nil, err
		}
		return result.Body, result.ToolMap, nil
	}
	var req translate.OpenAIChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	result, err := translate.OpenAIToZen(&req, model, profile)
	if err != nil {
		return nil, nil, err
	}
	return result.Body, result.ToolMap, nil
}

// Recover before emitting any SSE bytes, then replay at most once. Quota,
// region errors, and arbitrary HTML 403s do not trigger identity discovery.
func (s *Server) mihomoStreamZen(ctx context.Context, model, seed string, body map[string]any, rec requestRecord) (<-chan zen.Event, int, error, string) {
	profile := zen.GateSnapshot(ctx, s.zen.CurrentGate)
	ctx = zen.WithGateSnapshot(ctx, profile)
	events, status, err, group := s.mihomoStreamZenOnce(ctx, model, seed, body, rec)
	if err == nil || s.gate == nil || !zen.IsFreeModel(model) || !gate.IsIdentityRejection(status, err.Error()) {
		return events, status, err, group
	}
	s.prepareGate()
	if !s.gate.RecoverModel(ctx, model) {
		return events, status, err, group
	}
	nextProfile := s.zen.CurrentGate()
	if zen.SameGateProfile(profile, nextProfile) {
		return events, status, err, group
	}
	profile = nextProfile
	ctx = zen.WithGateSnapshot(ctx, profile)
	if rebuild, ok := ctx.Value(gateRebuildKey{}).(func(*zen.GateProfile) (map[string]any, error)); ok {
		body, err = rebuild(profile)
		if err != nil {
			return nil, status, err, group
		}
	} else if rebuild, ok := ctx.Value(gateRebuildKey{}).(func() (map[string]any, error)); ok {
		body, err = rebuild()
		if err != nil {
			return nil, status, err, group
		}
	} else {
		// Playground/test requests have no client-side tool execution mapping.
		copy := make(map[string]any, len(body)+2)
		for key, value := range body {
			copy[key] = value
		}
		body = copy
		if profile != nil {
			var tools []zen.Tool
			if encoded, e := json.Marshal(body["tools"]); e == nil {
				json.Unmarshal(encoded, &tools)
			}
			tools, _ = zen.PrepareTools(tools, profile)
			body["tools"] = tools
			if profile.RequiresStream {
				body["stream"] = true
			}
		}
	}
	return s.mihomoStreamZenOnce(ctx, model, seed, body, rec)
}
