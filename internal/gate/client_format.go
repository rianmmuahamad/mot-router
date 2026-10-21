package gate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

var v2ProjectID = func() string {
	wd, _ := os.Getwd()
	digest := sha256.Sum256([]byte("mot-router:" + wd))
	return hex.EncodeToString(digest[:20])
}()

const (
	ClientV1          = "v1"
	ClientV2          = "v2"
	DefaultBaseURL    = "https://opencode.ai/inference/v1"
	LegacyBaseURL     = "https://opencode.ai/zen/v1"
	DefaultProbeModel = "deepseek-v4.1-flash-free"
)

// Keep inference and diagnostic endpoint selection in one place.
//
// Every live free model speaks the chat lane: upstream's catalog marks them
// `@ai-sdk/openai-compatible`, and the upstream gateway answers a chat request
// for all of them.
//
// A model that upstream only serves over the Responses API is named in
// ZEN_RESPONSES_MODELS (comma-separated ids), which mirrors the opencode-free
// bridge's RESPONSES_MODELS. The previous rule keyed off a model-name
// substring for a family that no longer exists, so it could only ever route a
// retired model to a lane the local bridge does not serve.
func UsesChatCompletions(model string) bool {
	return !responsesModels()[strings.ToLower(strings.TrimSpace(model))]
}

// responsesModels reports the model ids that need the Responses lane.
func responsesModels() map[string]bool {
	out := map[string]bool{}
	for _, id := range strings.Split(os.Getenv("ZEN_RESPONSES_MODELS"), ",") {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			out[id] = true
		}
	}
	return out
}

// Endpoint recovery is restricted to the two known official API roots.
func IsOfficialEndpoint(base string) bool {
	base = strings.TrimRight(base, "/")
	return base == DefaultBaseURL || base == LegacyBaseURL
}

func validClientFormat(format string) bool {
	return format == "" || format == ClientV1 || format == ClientV2
}

// ClientHeaders is shared by probes and actual inference. A verified V2
// profile must not be replayed with V1 request-ID or SDK attribution headers.
func ClientHeaders(ua, session, request, format string) map[string]string {
	h := map[string]string{
		"Content-Type": "application/json", "Accept": "*/*",
		"User-Agent": ua, "x-opencode-project": "global",
		"x-opencode-session": session, "x-opencode-session-id": session,
	}
	if format == ClientV2 {
		h["x-opencode-project"] = v2ProjectID
		parts := strings.Split(ua, "/")
		client := "opencode"
		if len(parts) == 4 && parts[3] != "" {
			client = parts[3]
		}
		h["x-opencode-client"] = client
		h["x-session-affinity"] = session
		h["X-Session-Id"] = session
		// Correlation IDs belong to this request; never replay captured IDs.
		var trace [32]byte
		if _, err := rand.Read(trace[:]); err == nil {
			id, span, parent := hex.EncodeToString(trace[:16]), hex.EncodeToString(trace[16:24]), hex.EncodeToString(trace[24:])
			h["traceparent"] = "00-" + id + "-" + span + "-01"
			h["b3"] = id + "-" + span + "-1-" + parent
		}
	} else {
		h["Accept"] = "*/*"
		h["x-opencode-client"] = "cli"
		h["x-opencode-request"] = request
	}
	return h
}

func nativeToolNames(format string) []string {
	return []string{"read"}
}

func (p *Prober) probeTools() []string {
	p.mu.RLock()
	format := p.format
	p.mu.RUnlock()
	if format == ClientV2 {
		return nativeToolNames(ClientV2)
	}
	return append([]string(nil), p.ToolPool...)
}

// ProbeTool uses required parameter names observed in the official clients.
// These definitions are only for diagnostic requests: they grant no ability
// to run tools or change the caller's execution permissions.
func ProbeTool(name, format string) map[string]any {
	if format == ClientV2 {
		if native := nativeV2Tool(name); native != nil {
			return native
		}
	}
	props := map[string]any{}
	required := []string{}
	add := func(key, kind string, needed bool) {
		p := map[string]any{"type": kind}
		if kind == "array" {
			p["items"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		props[key] = p
		if needed {
			required = append(required, key)
		}
	}
	path := "filePath"
	if format == ClientV2 {
		path = "path"
	}
	switch name {
	case "bash", "shell":
		add("command", "string", true)
		add("timeout", "number", false)
		add("workdir", "string", false)
	case "read":
		add(path, "string", true)
		add("offset", "number", false)
		add("limit", "number", false)
	case "edit":
		add(path, "string", true)
		add("oldString", "string", true)
		add("newString", "string", true)
		add("replaceAll", "boolean", false)
	case "write":
		add(path, "string", true)
		add("content", "string", true)
	case "glob", "grep":
		add("pattern", "string", true)
		add("path", "string", false)
	case "skill":
		if format == ClientV2 {
			add("id", "string", true)
		} else {
			add("name", "string", true)
		}
	case "execute":
		add("code", "string", true)
	case "task", "subagent":
		add("description", "string", true)
		add("prompt", "string", true)
		if format == ClientV2 {
			add("agent", "string", true)
		} else {
			add("subagent_type", "string", true)
		}
	case "todowrite":
		add("todos", "array", true)
	case "webfetch":
		add("url", "string", true)
	case "websearch":
		add("query", "string", true)
	case "question":
		add("questions", "array", true)
	default:
		add("input", "string", false)
	}
	description := "Diagnostic definition of " + name + ". Do not invoke during this connectivity check."
	if native := NativeToolDescription(name); native != "" && format != ClientV2 {
		description = native
	}
	return map[string]any{
		"type": "function", "name": name,
		"description": description,
		"parameters":  map[string]any{"type": "object", "properties": props, "required": required},
	}
}
