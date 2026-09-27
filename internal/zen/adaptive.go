package zen

import (
	"sort"
	"strings"

	"github.com/rian/mot-router/internal/gate"
)

// GateProfile is the request shape the upstream gate currently accepts. The
// router applies it to every free-tier request so a client that changes its
// User-Agent, session format, or tool roster keeps working unchanged.
type GateProfile struct {
	UserAgent      string
	SessionPrefix  string
	SessionHex     int
	SessionAlnum   int
	RequiresStream bool
	// RequiredTools are the names that must be present in the body.
	RequiredTools []string
	// AcceptsToolSuperset records whether extra names are tolerated. When false
	// the router must send exactly RequiredTools rather than a superset.
	AcceptsToolSuperset bool
}

// ProfileFromGate converts a probed gate profile into the client's shape.
func ProfileFromGate(p *gate.Profile) *GateProfile {
	if p == nil {
		return nil
	}
	return &GateProfile{
		UserAgent:           p.UserAgent,
		SessionPrefix:       p.SessionPrefix,
		SessionHex:          p.SessionHex,
		SessionAlnum:        p.SessionAlnum,
		RequiresStream:      p.RequiresStream,
		RequiredTools:       p.RequiredTools,
		AcceptsToolSuperset: p.AcceptsToolSuperset,
	}
}

// ToolInjection describes a tool the router added to satisfy the gate.
//
// This matters because the model will happily call an injected tool even when
// the client never declared it (verified against the live gateway). The client
// would then receive a tool call it cannot execute, so the response translator
// must rewrite such calls into something the client understands.
type ToolInjection struct {
	// Injected maps lowercased injected names to the client tool they map onto.
	// A nil value means there is no natural equivalent and the call should be
	// dropped rather than forwarded as an unknown tool.
	Injected map[string]*string
	// Originals maps lowercased client tool names back to the client's casing.
	Originals map[string]string
}

// stubDescription explains the injected tool to the model.
func stubDescription(name string, equiv *string) string {
	if equiv != nil {
		return "Alias for the client's " + *equiv + " tool. Use it exactly like " + *equiv + "."
	}
	switch name {
	case "glob":
		return "Find files matching a glob pattern."
	case "grep":
		return "Search file contents for a regex pattern."
	default:
		return "Required by the upstream gateway."
	}
}

// equivalentClientTool finds a client tool that can stand in for an injected
// name. It returns "" when there is no sensible equivalent.
//
// Matching is substring-based rather than an exact-name table, because clients
// name the same capability many ways: `execute_command`, `run_terminal_cmd`,
// `shell_exec`, `BashTool`, and so on. Each injected name has an ordered list of
// substrings; the first client tool containing any of them wins.
//
// The mapping stays conservative: a candidate must contain a distinctive
// substring, so unrelated tools are never matched by accident.
func equivalentClientTool(injected string, originals map[string]string) string {
	// Ordered most-specific first, so `read_file` beats a bare `file`.
	patterns := map[string][]string{
		"bash": {"execute_command", "run_terminal", "run_command", "shell_exec", "run_shell",
			"terminal", "command", "shell", "exec", "bash", "run"},
		"read": {"read_file", "readfile", "get_file", "view_file", "open_file",
			"cat", "read", "view", "open"},
		"edit": {"str_replace", "apply_patch", "write_file", "edit_file", "create_file",
			"write", "edit", "patch", "modify"},
		"glob": {"list_files", "search_files", "find_files", "glob", "find", "list"},
		"grep": {"find_in_files", "search_files", "ripgrep", "grep", "search"},
	}
	candidates, ok := patterns[injected]
	if !ok {
		return ""
	}

	// Iterate client tools in sorted order so the choice is deterministic: an
	// unstable mapping would break prompt caching and make behaviour unrepeatable.
	names := make([]string, 0, len(originals))
	for low := range originals {
		names = append(names, low)
	}
	sort.Strings(names)

	for _, pat := range candidates {
		for _, low := range names {
			if toolNameMatches(low, pat) {
				return originals[low]
			}
		}
	}
	return ""
}

// toolNameMatches reports whether a client tool name corresponds to a pattern.
//
// Names are compared on token boundaries, not raw substrings, so `ls` does not
// match `tools` and `run` does not match `truncate`. A multi-word pattern such
// as `execute_command` may appear either as a contiguous substring or as
// separate tokens (`executeCommand`, `execute-command`).
func toolNameMatches(name, pattern string) bool {
	if name == pattern {
		return true
	}
	tokens := tokenizeToolName(name)
	if strings.Contains(pattern, "_") {
		// Multi-word pattern: require the words to appear in order and adjacent.
		parts := strings.Split(pattern, "_")
		for i := 0; i+len(parts) <= len(tokens); i++ {
			match := true
			for j, p := range parts {
				if tokens[i+j] != p {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
		return false
	}
	// Single-word pattern: any token may match.
	for _, t := range tokens {
		if t == pattern {
			return true
		}
	}
	return false
}

// tokenizeToolName splits a tool name into lowercase word tokens, handling
// snake_case, kebab-case, spaces, and camelCase.
func tokenizeToolName(name string) []string {
	var tokens []string
	var cur strings.Builder
	runes := []rune(name)

	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}

	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == ' ' || r == '.':
			flush()
		case r >= 'A' && r <= 'Z':
			// A new token starts at an uppercase letter, unless the whole run is
			// uppercase (an acronym like "URL").
			prevLower := i > 0 && runes[i-1] >= 'a' && runes[i-1] <= 'z'
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if prevLower || (nextLower && cur.Len() > 0) {
				flush()
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

// reshapeArgs adapts an injected tool's arguments to the client tool's shape.
//
// Injected glob/grep stubs carry `pattern`; the client tool they map onto is
// usually a shell runner expecting `command`, so the pattern becomes a command.
func reshapeArgs(injected string, args map[string]any) map[string]any {
	if _, hasCommand := args["command"]; hasCommand {
		return args
	}
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return args
	}

	switch injected {
	case "glob":
		if strings.Contains(pattern, "/") {
			return map[string]any{"command": "find . -path " + shellQuote("./"+pattern)}
		}
		return map[string]any{"command": "find . -name " + shellQuote(pattern)}
	case "grep":
		return map[string]any{"command": "grep -rn -e " + shellQuote(pattern) + " ."}
	default:
		return args
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gateSessionID mints a session id using the probed shape. It delegates to the
// gate package so the router and the prober always agree on the format.
func gateSessionID(seed, prefix string, hexLen, alnumLen int) string {
	return gate.SessionID(seed, prefix, hexLen, alnumLen)
}
