package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFallbackModelForLimitUsesPublishedSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "availability.json")
	snapshot := modelAvailability{
		UpdatedAt: time.Now().Unix(),
		Node:      "NB-fixture",
		Models:    map[string]int{"deepseek-v4.1-flash-free": 429, "beam-free": 200},
		Available: []string{"beam-free"},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(availabilityEnv, path)

	s := &Server{store: openTestStore(t)}
	if got := s.fallbackModelForLimit(t.Context(), "deepseek-v4.1-flash-free"); got != "beam-free" {
		t.Fatalf("fallback = %q, want beam-free", got)
	}
	// The model that is already available must not be chosen as its own fallback.
	if got := s.fallbackModelForLimit(t.Context(), "beam-free"); got != "" {
		t.Fatalf("fallback for the available model = %q, want empty", got)
	}
}

func TestFallbackModelForLimitIgnoresStaleSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "availability.json")
	raw, err := json.Marshal(modelAvailability{
		UpdatedAt: time.Now().Add(-2 * time.Hour).Unix(),
		Models:    map[string]int{"beam-free": 200},
		Available: []string{"beam-free"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(availabilityEnv, path)

	s := &Server{store: openTestStore(t)}
	if got := s.fallbackModelForLimit(t.Context(), "deepseek-v4.1-flash-free"); got != "" {
		t.Fatalf("stale snapshot produced a fallback: %q", got)
	}
}
