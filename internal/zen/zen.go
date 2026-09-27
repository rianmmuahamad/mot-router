// Package zen implements the upstream OpenCode Zen client, including the
// client-identity and request-body conditions its free tier enforces.
package zen

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the Zen API root. Only /responses is functional upstream;
// /chat/completions and /messages return 500.
const DefaultBaseURL = "https://opencode.ai/zen/v1"

// DefaultUserAgent must keep `opencode/<version>` as its leading token. The
// gateway rejects anything else with a FreeTierError, and versions below 1.18.0
// with UpgradeRequired.
const DefaultUserAgent = "opencode/1.18.31"

// RequiredTools are the tool names the free-tier gate demands in the request
// body. Schemas and descriptions are ignored; only the names are checked.
var RequiredTools = []string{"bash", "edit", "glob", "grep", "read"}

// Client talks to the Zen gateway.
type Client struct {
	BaseURL     string
	UserAgent   string
	UpstreamKey string
	HTTP        *http.Client
	// Gate is the probed request shape the free tier currently requires. When
	// nil the client falls back to the historical hardcoded values.
	Gate *GateProfile
	// GateVersion is the version token used when building the User-Agent.
	GateVersion string
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
		HTTP: &http.Client{},
	}
}

// SessionID derives the `ses_<12 lowercase hex><14 alnum>` identifier the
// gateway shape-checks. Deriving it from a stable seed keeps it constant across
// a conversation, which matters because Zen pins routing and prompt caching on
// this value.
func SessionID(seed string) string {
	sum := md5.Sum([]byte(seed))
	hex12 := hex.EncodeToString(sum[:])[:12]

	sum2 := sha256.Sum256([]byte(seed))
	alnum14 := hex.EncodeToString(sum2[:])[:14]

	return "ses_" + hex12 + alnum14
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
	ua := c.UserAgent
	prefix, hexLen, alnumLen := "ses_", 12, 14
	if c.Gate != nil {
		if c.Gate.UserAgent != "" {
			ua = c.Gate.UserAgent
		}
		if c.Gate.SessionPrefix != "" {
			prefix = c.Gate.SessionPrefix
		}
		if c.Gate.SessionHex > 0 {
			hexLen = c.Gate.SessionHex
		}
		if c.Gate.SessionAlnum > 0 {
			alnumLen = c.Gate.SessionAlnum
		}
	}
	h := map[string]string{
		"Content-Type":       "application/json",
		"Accept":             "text/event-stream",
		"User-Agent":         ua,
		"x-opencode-client":  "cli",
		"x-opencode-project": "global",
		"x-opencode-session": gateSessionID(sessionSeed, prefix, hexLen, alnumLen),
	}
	// Free models need no credential; `public` is what the official CLI sends.
	if IsFreeModel(model) {
		h["Authorization"] = "Bearer public"
	} else if c.UpstreamKey != "" {
		h["Authorization"] = "Bearer " + c.UpstreamKey
	}
	return h
}

// Event is one decoded frame from the upstream SSE stream.
type Event struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	ItemID   string `json:"item_id"`
	Item     *Item  `json:"item"`
	Response *struct {
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
	if UsesChatCompletions(model) {
		return c.StreamChatCompletions(ctx, model, sessionSeed, body)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	for k, v := range c.Headers(model, sessionSeed) {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("call upstream: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, resp.StatusCode, fmt.Errorf("%s", strings.TrimSpace(string(detail)))
	}

	out := make(chan Event, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
				continue // keepalive or partial frame
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, resp.StatusCode, nil
}

// Model is a catalog entry from /models.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ListModels fetches the upstream model catalog.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("x-opencode-session", SessionID("models"))

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := c.HTTP.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models: upstream returned %d", resp.StatusCode)
	}

	var payload struct {
		Data []Model `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	return payload.Data, nil
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
		Injected:  map[string]*string{},
		Originals: map[string]string{},
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
		tools = append(tools, Tool{
			Type:        "function",
			Name:        low,
			Description: t.Description,
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
		}

		tools = append(tools, Tool{
			Type:        "function",
			Name:        low,
			Description: stubDescription(low, equiv),
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
