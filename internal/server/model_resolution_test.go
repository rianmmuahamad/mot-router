package server

import (
	"testing"

	"github.com/rian/mot-router/internal/zen"
)

func TestExplicitModelNeverFallsBackToDefault(t *testing.T) {
	for _, tc := range []struct{ name, requested, key, want string }{
		{"legacy DeepSeek selects free variant", "deepseek-v4.1-flash", "", "deepseek-v4.1-flash-free"},
		{"canonical free ID stays exact", "deepseek-v4.1-flash-free", "", "deepseek-v4.1-flash-free"},
		{"paid ID with credential stays exact", "deepseek-v4.1-flash", "fixture-key", "deepseek-v4.1-flash"},
		{"unknown explicit model stays exact", "unavailable-model", "", "unavailable-model"},
		{"paid explicit model stays exact", "deepseek-v4-pro", "", "deepseek-v4-pro"},
		{"omitted model uses default", "", "", "deepseek-v4.1-flash-free"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{store: openTestStore(t), zen: zen.New("", "", tc.key), cfg: Config{DefaultModel: "deepseek-v4.1-flash-free"}}
			if got := s.resolveModel(t.Context(), tc.requested); got != tc.want {
				t.Fatalf("requested %q resolved to %q; want %q", tc.requested, got, tc.want)
			}
		})
	}
}

func TestExplicitZenPrefixAndConfiguredAlias(t *testing.T) {
	s := &Server{store: openTestStore(t), zen: zen.New("", "", "")}
	provider, bare := s.providerFor(t.Context(), "opencode-zen/deepseek-v4.1-flash")
	if provider != ProviderZen || s.resolveModel(t.Context(), bare) != "deepseek-v4.1-flash-free" {
		t.Fatalf("explicit Zen route lost DeepSeek selection: %s/%s", provider, bare)
	}
	if err := s.store.SetSetting("alias:custom-name", "deepseek-v4.1-flash-free"); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveModel(t.Context(), "custom-name"); got != "deepseek-v4.1-flash-free" {
		t.Fatalf("configured alias ignored: %s", got)
	}
}
