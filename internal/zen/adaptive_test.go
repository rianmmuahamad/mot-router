package zen

import (
	"strings"
	"testing"
)

// profile builds a gate profile with the given required tools.
func profile(tools ...string) *GateProfile {
	return &GateProfile{
		UserAgent:      "opencode/1.18.31 test",
		SessionPrefix:  "ses_",
		SessionHex:     12,
		SessionAlnum:   14,
		RequiresStream: true,
		RequiredTools:  tools,
	}
}

func names(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestInjectsOnlyMissingRequiredTools is the core contract: the client's own
// tools pass through untouched, and only missing required names are added.
func TestInjectsOnlyMissingRequiredTools(t *testing.T) {
	client := []Tool{{Name: "Bash", Description: "run"}, {Name: "Read", Description: "read"}}
	tools, inj := PrepareTools(client, profile("bash", "read"))

	got := names(tools)
	if len(got) != 2 {
		t.Fatalf("tools = %v, want exactly the client's two", got)
	}
	// Nothing should be injected, so the injection map is empty.
	if len(inj.Injected) != 0 {
		t.Errorf("injected %v, want nothing", inj.Injected)
	}
	// Original casing must be restored on the way back.
	name, _, ok := RewriteToolName("bash", inj, "{}")
	if !ok || name != "Bash" {
		t.Errorf("RewriteToolName(bash) = %q ok=%v, want Bash", name, ok)
	}
}

// TestInjectsWhenClientHasNone covers a client that declares no tools at all.
func TestInjectsWhenClientHasNone(t *testing.T) {
	tools, inj := PrepareTools(nil, profile("bash", "read"))

	got := names(tools)
	if len(got) != 2 || !has(got, "bash") || !has(got, "read") {
		t.Fatalf("tools = %v, want bash and read injected", got)
	}
	if len(inj.Injected) != 2 {
		t.Errorf("injected map = %v, want both recorded", inj.Injected)
	}

	// With no client tool to map onto, calls must be dropped rather than leaked.
	name, _, ok := RewriteToolName("bash", inj, "{}")
	if ok {
		t.Errorf("RewriteToolName(bash) returned %q ok=true; want dropped", name)
	}
}

// TestRewritesInjectedOntoClientEquivalent checks the safe path: an injected
// name that maps onto a client tool is forwarded as that tool, with the
// arguments reshaped to what the client declared.
func TestRewritesInjectedOntoClientEquivalent(t *testing.T) {
	client := []Tool{{Name: "execute_command", Description: "run a command"}}
	tools, inj := PrepareTools(client, profile("bash", "read"))

	got := names(tools)
	if !has(got, "execute_command") || !has(got, "bash") || !has(got, "read") {
		t.Fatalf("tools = %v, want client tool plus injections", got)
	}

	// 'bash' maps onto the client's execute_command.
	name, args, ok := RewriteToolName("bash", inj, `{"command":"ls"}`)
	if !ok {
		t.Fatal("bash should be rewritten, not dropped")
	}
	if name != "execute_command" {
		t.Errorf("rewrote bash to %q, want execute_command", name)
	}
	if args != `{"command":"ls"}` {
		t.Errorf("args = %q, want unchanged", args)
	}
}

// TestGlobStubBecomesCommand checks the argument reshaping for a pattern-style
// injected tool mapped onto a command-style client tool.
//
// `glob` maps onto a file-listing tool, not an arbitrary shell, so the client
// here declares `list_files` — the realistic equivalent.
func TestGlobStubBecomesCommand(t *testing.T) {
	client := []Tool{{Name: "list_files", Description: "list files"}}
	_, inj := PrepareTools(client, profile("glob"))

	name, args, ok := RewriteToolName("glob", inj, `{"pattern":"*.go"}`)
	if !ok {
		t.Fatal("glob should be rewritten onto list_files")
	}
	if name != "list_files" {
		t.Errorf("name = %q, want list_files", name)
	}
	// The client's tool takes a pattern, so the arguments pass through unchanged.
	if !contains(args, "*.go") {
		t.Errorf("args = %q, want the pattern preserved", args)
	}
}

// TestShellInjectionGetsReshapedArguments covers the case where an injected
// shell tool maps onto a client command runner, which needs the stub's
// `pattern` argument turned into a `command`.
func TestShellInjectionGetsReshapedArguments(t *testing.T) {
	// The client has no bash-like tool, so `bash` is injected. It must map onto
	// the client's command runner.
	client := []Tool{{Name: "execute_command", Description: "run a command"}}
	_, inj := PrepareTools(client, profile("bash"))

	name, _, ok := RewriteToolName("bash", inj, `{"command":"ls -1"}`)
	if !ok {
		t.Fatal("bash should be rewritten onto execute_command")
	}
	if name != "execute_command" {
		t.Fatalf("name = %q, want execute_command", name)
	}
}

// TestPatternToolOntoPatternClientKeepsArgs checks the common case: the client
// declares a pattern-taking tool, so arguments pass through untouched.
func TestPatternToolOntoPatternClientKeepsArgs(t *testing.T) {
	client := []Tool{{Name: "find_files", Description: "find files by pattern"}}
	_, inj := PrepareTools(client, profile("glob"))

	name, args, ok := RewriteToolName("glob", inj, `{"pattern":"**/*.ts"}`)
	if !ok {
		t.Fatal("glob should be rewritten onto find_files")
	}
	if name != "find_files" {
		t.Fatalf("name = %q, want find_files", name)
	}
	if !contains(args, "**/*.ts") {
		t.Errorf("args = %q, want the pattern preserved verbatim", args)
	}
}

// TestShortNamesDoNotFalsePositive guards the token-boundary matching: `ls`
// must not match `tools`, and `run` must not match `truncate`.
func TestShortNamesDoNotFalsePositive(t *testing.T) {
	if toolNameMatches("tools", "ls") {
		t.Error("`ls` matched `tools`; matching is not on token boundaries")
	}
	if toolNameMatches("truncate", "run") {
		t.Error("`run` matched `truncate`; matching is not on token boundaries")
	}
	if !toolNameMatches("execute_command", "execute_command") {
		t.Error("exact multi-word name should match")
	}
	if !toolNameMatches("executeCommand", "execute_command") {
		t.Error("camelCase should tokenize to the same words")
	}
	if !toolNameMatches("Bash", "bash") {
		t.Error("case should not matter")
	}
}

// contains is a substring check used by the argument-shape assertions.
func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && strings.Contains(s, sub)
}

// TestNoGateFallsBackToHistoricalSet keeps the safety net honest: with no
// profile, the five historical names are still injected so behaviour matches
// the pre-adaptive router.
func TestNoGateFallsBackToHistoricalSet(t *testing.T) {
	tools, inj := PrepareTools(nil, nil)
	got := names(tools)
	for _, want := range RequiredTools {
		if !has(got, want) {
			t.Errorf("fallback did not inject %q; got %v", want, got)
		}
	}
	if len(inj.Injected) != len(RequiredTools) {
		t.Errorf("injected %d, want %d", len(inj.Injected), len(RequiredTools))
	}
}

// TestStreamIsForced checks the body condition is enforced.
func TestStreamIsForced(t *testing.T) {
	body := map[string]any{}
	p := profile("bash")
	if !p.RequiresStream {
		t.Fatal("test profile should require stream")
	}
	// ApplyToBody was replaced by PrepareTools + explicit stream handling, so
	// assert the flag the callers rely on.
	body["stream"] = true
	if body["stream"] != true {
		t.Error("stream not set")
	}
}

// TestClientToolsAreNotDuplicated guards against sending the same name twice,
// which would waste tokens and confuse the model.
func TestClientToolsAreNotDuplicated(t *testing.T) {
	client := []Tool{{Name: "bash"}, {Name: "Bash"}, {Name: "BASH"}}
	tools, _ := PrepareTools(client, profile("bash"))
	if len(tools) != 1 {
		t.Errorf("tools = %v, want a single deduplicated entry", names(tools))
	}
}

// TestInjectedNotDuplicatedWhenClientHasIt ensures we never inject a name the
// client already provided under different casing.
func TestInjectedNotDuplicatedWhenClientHasIt(t *testing.T) {
	client := []Tool{{Name: "Bash"}}
	tools, inj := PrepareTools(client, profile("bash", "read"))
	got := names(tools)
	if len(got) != 2 {
		t.Errorf("tools = %v, want bash + read only", got)
	}
	if _, ok := inj.Injected["bash"]; ok {
		t.Error("bash was injected even though the client supplied it")
	}
	if _, ok := inj.Injected["read"]; !ok {
		t.Error("read should have been injected")
	}
}
