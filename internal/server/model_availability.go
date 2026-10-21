package server

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// modelAvailability is the per-model availability snapshot written by the
// egress health monitor on the VPS.
//
// The free tier's allowance is scoped per (egress IP, model), not per IP
// alone: measured from one exit IP, `beam-free` answered 200 while
// `deepseek-v4.1-flash-free` answered 429, and changing the exit IP left the
// 429 in place. So a spent model cannot be repaired by rotating the egress —
// the only way to answer is a different model that still has allowance.
type modelAvailability struct {
	UpdatedAt int64            `json:"updated_at"`
	Node      string           `json:"node"`
	Models    map[string]int   `json:"models"`
	Available []string         `json:"available"`
}

const (
	// availabilityFile is where the monitor publishes its snapshot.
	availabilityFile = "/opt/mot-router/data/model-availability.json"
	// availabilityMaxAge bounds how stale a snapshot may be. A snapshot older
	// than this is ignored, because a fallback to a model whose allowance has
	// since been spent would just move the failure.
	availabilityMaxAge = 30 * time.Minute
	// availabilityEnv overrides the snapshot path, so a test or a laptop
	// install can point at its own file.
	availabilityEnv = "MOT_MODEL_AVAILABILITY"
)

var (
	availabilityMu   sync.Mutex
	availabilityRead time.Time
	availabilityData modelAvailability
	availabilityPath string
)

// readModelAvailability loads the published snapshot, if it is fresh enough.
//
// The cache is keyed by path and mtime, so a snapshot that was rewritten (a
// new measurement) is picked up immediately while an unchanged file is not
// re-read on every request.
func (s *Server) readModelAvailability() modelAvailability {
	availabilityMu.Lock()
	defer availabilityMu.Unlock()

	path := availabilityFile
	if override := os.Getenv(availabilityEnv); override != "" {
		path = override
	}
	info, err := os.Stat(path)
	if err != nil {
		availabilityData = modelAvailability{}
		availabilityPath = path
		availabilityRead = time.Time{}
		return availabilityData
	}
	if path == availabilityPath && info.ModTime().Equal(availabilityRead) {
		return availabilityData
	}
	availabilityPath = path
	availabilityRead = info.ModTime()

	raw, err := os.ReadFile(path)
	if err != nil {
		availabilityData = modelAvailability{}
		return availabilityData
	}
	var snap modelAvailability
	if err := json.Unmarshal(raw, &snap); err != nil {
		availabilityData = modelAvailability{}
		return availabilityData
	}
	if snap.UpdatedAt <= 0 || time.Since(time.Unix(snap.UpdatedAt, 0)) > availabilityMaxAge {
		availabilityData = modelAvailability{}
		return availabilityData
	}
	availabilityData = snap
	return availabilityData
}

// fallbackModelForLimit picks a different model that still has allowance.
//
// The requested model comes first so a model that merely looks unavailable in
// a stale snapshot is not displaced. The result is only used when the
// upstream answered 429 for the requested model, which is the one failure a
// different model can actually fix.
func (s *Server) fallbackModelForLimit(ctx context.Context, requested string) string {
	snap := s.readModelAvailability()
	if len(snap.Available) == 0 {
		return ""
	}
	for _, model := range snap.Available {
		if model != requested {
			return model
		}
	}
	return ""
}
