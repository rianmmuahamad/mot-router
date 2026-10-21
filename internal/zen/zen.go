// Package zen implements the upstream OpenCode Zen client, including the
// client-identity and request-body conditions its free tier enforces.
package zen

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/streamwire"
)

// DefaultBaseURL is the API root currently used by the native OpenCode catalog.
const DefaultBaseURL = gate.DefaultBaseURL

// DefaultUserAgent must keep `opencode/<version>` as its leading token. The
// gateway rejects anything else with a FreeTierError, and versions below 1.18.0
// with UpgradeRequired.
const DefaultUserAgent = gate.DefaultUserAgent

// RequiredTools is the genuine OpenCode CLI tool name demanded by the free-tier gate.
var RequiredTools = []string{"read"}

// Client talks to the Zen gateway.
type Client struct {
	BaseURL     string
	UserAgent   string
	UpstreamKey string
	HTTP        *http.Client
	// Gate is the probed request shape the free tier currently requires. When
	// nil the client falls back to the historical hardcoded values.
	Gate         *GateProfile
	GateProvider func() *GateProfile
	// GateVersion is the version token used when building the User-Agent.
	GateVersion string
	catalog     *catalogCache
}

// New builds a client with production defaults.
func New(baseURL, userAgent, upstreamKey string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	return &Client{
		BaseURL:     strings.TrimSuffix(baseURL, "/"),
		UserAgent:   userAgent,
		UpstreamKey: upstreamKey,
		// No client timeout: generations can legitimately run for minutes.
		HTTP:    &http.Client{},
		catalog: newCatalogCache(),
	}
}

// SessionID reuses the same timestamp encoding and stable cache as the prober.
func SessionID(seed string) string {
	return gate.SessionID(seed, "ses_", 12, 14)
}

// IsFreeModel reports whether a model is served by the free tier.
// Both naming styles count: Zen uses "-free", Token Harbor uses ":free".
func IsFreeModel(model string) bool {
	low := strings.ToLower(model)
	return strings.HasSuffix(low, "-free") || strings.HasSuffix(low, ":free")
}

// Headers assembles the upstream request headers, honouring the probed gate
// profile so a client-identity change upstream is picked up automatically.
func (c *Client) Headers(model, sessionSeed string) map[string]string {
	profile := c.CurrentGate()
	ua := c.UserAgent
	policy := gate.DefaultIdentity()
	format := gate.ClientV1
	if profile != nil {
		format = profile.ClientFormat
		policy = profile.Identity.Normalized()
		if profile.UserAgent != "" {
			ua = profile.UserAgent
		}
	}
	session := gate.SessionIDWithPolicy(sessionSeed, policy)
	h := gate.ClientHeaders(ua, session, gate.RequestIDWithPolicy(sessionSeed, policy), format)
	// Free models need no credential; `public` is what the official CLI sends.
	if IsFreeModel(model) {
		h["Authorization"] = "Bearer public"
	} else if c.UpstreamKey != "" {
		h["Authorization"] = "Bearer " + c.UpstreamKey
	}
	return h
}

func (c *Client) CurrentGate() *GateProfile {
	if c.GateProvider != nil {
		return c.GateProvider()
	}
	return c.Gate
}

// EffectiveBaseURL applies a verified migration to free inference and the
// catalog. Explicit third-party endpoints and paid requests keep their URL.
func (c *Client) EffectiveBaseURL(model string) string {
	profile := c.CurrentGate()
	if (model == "" || IsFreeModel(model)) && profile != nil && gate.IsOfficialEndpoint(c.BaseURL) && gate.IsOfficialEndpoint(profile.BaseURL) {
		return profile.BaseURL
	}
	return c.BaseURL
}

// Event is one decoded frame from the upstream SSE stream.
type Event struct {
	Type         string       `json:"type"`
	Error        *StreamError `json:"error,omitempty"`
	FinishReason string       `json:"-"`
	// Chat conversion synthesizes a terminal event on EOF for translation.
	// This flag distinguishes that case from a confirmed stream termination.
	StreamInterrupted bool   `json:"-"`
	Delta             string `json:"delta"`
	ItemID            string `json:"item_id"`
	Item              *Item  `json:"item"`
	Response          *struct {
		Usage *Usage `json:"usage"`
	} `json:"response"`
}

// Item is an entry in the Responses API output array.
type Item struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	ID        string `json:"id"`
	Arguments string `json:"arguments"`
}

// Usage reports token consumption for a completed response.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Stream sends a request and returns the decoded event channel plus the HTTP
// status. The caller must drain the channel; it closes when the body ends.
func (c *Client) Stream(ctx context.Context, model, sessionSeed string, body map[string]any) (<-chan Event, int, error) {
	c = c.requestClient(ctx)
	body = withToolDescriptions(model, body, c.CurrentGate())
	if UsesChatCompletions(model) {
		return c.StreamChatCompletions(ctx, model, sessionSeed, body)
	}
	headers := c.Headers(model, sessionSeed)
	body = withPromptCacheKey(model, body, headers)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.EffectiveBaseURL(model)+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("call upstream: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, resp.StatusCode, upstreamError(resp, detail)
	}

	out := make(chan Event, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		terminal := false
		err := streamwire.Read(resp.Body, 8*1024*1024, func(data []byte) error {
			if string(data) == "[DONE]" {
				return streamwire.Stop
			}
			var envelope struct {
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal(data, &envelope) == nil {
				if failure := parseStreamError(envelope.Error); failure != nil {
					terminal = true
					_ = emitEvent(ctx, out, Event{Type: "error", Error: failure, StreamInterrupted: true})
					return streamwire.Stop
				}
			}
			var ev Event
			if err := json.Unmarshal(data, &ev); err != nil {
				return err
			}
			responseFinishReason(data, &ev)
			if ev.Type == "error" && ev.Error == nil {
				ev.Error = streamFailure("Upstream returned an error event", false)
			}
			if ev.Error != nil {
				terminal = true
				ev.Type = "error"
				ev.StreamInterrupted = true
				_ = emitEvent(ctx, out, ev)
				return streamwire.Stop
			}
			terminal = terminal || ev.Type == "response.completed" || ev.Type == "response.incomplete"
			return emitEvent(ctx, out, ev)
		})
		if err != nil {
			_ = emitEvent(ctx, out, Event{Type: "error", Error: invalidStream(err), StreamInterrupted: true})
		} else if !terminal {
			_ = emitEvent(ctx, out, Event{Type: "error", Error: streamFailure("Upstream stream ended without completion", true), StreamInterrupted: true})
		}
	}()

	return out, resp.StatusCode, nil
}

func withPromptCacheKey(model string, body map[string]any, headers map[string]string) map[string]any {
	if !IsFreeModel(model) || body["prompt_cache_key"] != nil {
		return body
	}
	key := headers["x-opencode-session"]
	if key == "" {
		return body
	}
	// V2 strips the ses_ prefix only for an externally supplied SHA-256 affinity.
	if len(key) == 68 && strings.HasPrefix(key, "ses_") {
		if _, err := hex.DecodeString(key[4:]); err == nil {
			key = key[4:]
		}
	}
	copy := make(map[string]any, len(body)+1)
	for k, v := range body {
		copy[k] = v
	}
	copy["prompt_cache_key"] = key
	return copy
}

// Model is a catalog entry from /models.
type Model struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	Family        string `json:"family,omitempty"`
	Reasoning     *bool  `json:"reasoning,omitempty"`
	MaxOutput     int    `json:"max_output_tokens,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	Status        string `json:"status,omitempty"`
}

// ListModels fetches the upstream model catalog.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	return c.listCachedModels(ctx)
}

// Tool is a function definition in the Responses API format.
type Tool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// PrepareTools lowercases client tool names and injects stubs for any names the
// gate requires that the client did not supply.
//
// The returned injection record must be used on the response side: the model
// will call an injected tool even though the client never declared it, so those
// calls have to be rewritten or dropped before reaching the client.
func PrepareTools(clientTools []Tool, gate *GateProfile) ([]Tool, *ToolInjection) {
	inj := &ToolInjection{
		ArgumentAliases: map[string]map[string]string{},
		Injected:        map[string]*string{},
		Originals:       map[string]string{},
	}

	tools := []Tool{}
	seen := map[string]bool{}

	for _, t := range clientTools {
		if t.Name == "" {
			continue
		}
		low := strings.ToLower(t.Name)
		if seen[low] {
			continue
		}
		seen[low] = true
		inj.Originals[low] = t.Name

		params := t.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		description := t.Description
		if gate != nil && gate.ToolDescriptions[low] != "" {
			description = gate.ToolDescriptions[low]
			params, inj.ArgumentAliases[low] = nativeToolSchema(low, gate.ClientFormat, params)
		}
		tools = append(tools, Tool{
			Type:        "function",
			Name:        low,
			Description: description,
			Parameters:  params,
		})
	}

	for _, required := range requiredToolsFor(gate) {
		low := strings.ToLower(required)
		if seen[low] {
			continue
		}
		seen[low] = true

		// Record what this injected name can be rewritten into on the way back.
		// A nil value means the client has no equivalent, so calls to it must be
		// dropped rather than forwarded as an unknown tool.
		var equiv *string
		if e := equivalentClientTool(low, inj.Originals); e != "" {
			equiv = &e
		}
		inj.Injected[low] = equiv

		params := map[string]any{
			"type":       "object",
			"properties": map[string]any{"pattern": map[string]any{"type": "string"}},
			"required":   []string{"pattern"},
		}
		if equiv != nil {
			params = map[string]any{
				"type":       "object",
				"properties": map[string]any{"command": map[string]any{"type": "string"}},
				"required":   []string{"command"},
			}
			// V2 renamed these tools. Keep the caller's argument contract when
			// adding an alias, so the response can be renamed back unchanged.
			if gate != nil && gate.ClientFormat == "v2" && (low == "shell" || low == "subagent") {
				for _, original := range clientTools {
					if original.Name == *equiv && original.Parameters != nil {
						params = original.Parameters
						break
					}
				}
			}
		}

		description := stubDescription(low, equiv)
		if gate != nil && gate.ToolDescriptions[low] != "" {
			description = gate.ToolDescriptions[low]
			params = gateToolParameters(low, gate.ClientFormat)
			if equiv != nil {
				for _, original := range clientTools {
					if original.Name == *equiv && original.Parameters != nil {
						params = original.Parameters
						break
					}
				}
			}
			params, inj.ArgumentAliases[low] = nativeToolSchema(low, gate.ClientFormat, params)
		}
		tools = append(tools, Tool{
			Type:        "function",
			Name:        low,
			Description: description,
			Parameters:  params,
		})
	}

	return tools, inj
}

// RewriteToolName resolves an upstream tool name back to what the client
// expects.
//
// Returns ok=false when the call must be dropped: the tool was injected by the
// router and the client has no equivalent, so forwarding it would hand the
// client a call it cannot execute.
func RewriteToolName(low string, inj *ToolInjection, argsJSON string) (name, args string, ok bool) {
	if inj == nil {
		return low, argsJSON, true
	}
	argsJSON = rewriteArgumentKeys(argsJSON, inj.ArgumentAliases[low], false)

	// A tool the client declared: restore its original casing.
	if original, present := inj.Originals[low]; present {
		return original, argsJSON, true
	}

	// A tool we injected: rewrite it onto a client tool, or drop it.
	if equiv, injected := inj.Injected[low]; injected {
		if equiv == nil {
			return "", "", false
		}
		return *equiv, reshapeArgsJSON(low, argsJSON), true
	}

	// Unknown to both sides: pass through, since we cannot prove it is unsafe.
	return low, argsJSON, true
}

// reshapeArgsJSON adapts an injected tool's arguments to the client tool shape.
func reshapeArgsJSON(injected, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON
	}
	reshaped := reshapeArgs(injected, args)
	out, err := json.Marshal(reshaped)
	if err != nil {
		return argsJSON
	}
	return string(out)
}

// WithHTTP returns a shallow copy of the client that uses the supplied HTTP
// client. Used to route a single request through a relay without disturbing the
// shared client.
func (c *Client) WithHTTP(h *http.Client) *Client {
	clone := *c
	if h != nil {
		clone.HTTP = h
	}
	return &clone
}

// requiredToolsFor returns the gate's required names, falling back to the
// historical set when no profile has been probed yet.
func requiredToolsFor(gate *GateProfile) []string {
	if gate != nil && len(gate.RequiredTools) > 0 {
		return gate.RequiredTools
	}
	return RequiredTools
}
