package gate

import (
	"context"
	"strings"
	"time"
)

type AutomaticStatus struct {
	Enabled          bool   `json:"enabled"`
	Refreshing       bool   `json:"refreshing"`
	RecoveryAttempts int    `json:"recovery_attempts"`
	Repairs          int    `json:"repairs"`
	LastRepairAt     int64  `json:"last_repair_at,omitempty"`
	LastRepairSource string `json:"last_repair_source,omitempty"`
	CooldownUntil    int64  `json:"cooldown_until,omitempty"`
	Limits           string `json:"limits"`
}

func (c *Cache) ProbeModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.current != nil && c.current.ProbeModel != "" {
		return c.current.ProbeModel
	}
	return c.prober.Model
}

func (c *Cache) Automatic() AutomaticStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := AutomaticStatus{Enabled: true, Refreshing: c.inflight || c.recovering, RecoveryAttempts: c.recoveryAttempts, Repairs: c.repairs, LastRepairSource: c.lastRepairSource, Limits: "Kandidat dan layout ID yang dikenali saja; algoritme baru yang belum didukung memerlukan pembaruan."}
	if !c.lastRepair.IsZero() {
		s.LastRepairAt = c.lastRepair.UnixMilli()
	}
	if time.Since(c.recoveryAt) < time.Minute {
		s.CooldownUntil = c.recoveryAt.Add(time.Minute).UnixMilli()
	}
	return s
}

// Recover coalesces concurrent rejected requests and limits one automatic
// discovery run per minute. A manual refresh remains available separately.
func (c *Cache) Recover(ctx context.Context) bool {
	return c.RecoverModel(ctx, c.ProbeModel())
}

func (c *Cache) RecoverModel(ctx context.Context, model string) bool {
	verified := func(p *Profile) bool {
		return p != nil && p.Source == "probe" && p.Verdict == "ok" && p.ProbeModel == model
	}
	c.mu.Lock()
	if c.recovering {
		done := c.recoveryDone
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return false
		}
		p := c.Snapshot()
		return verified(p)
	}
	if c.inflight {
		done := c.done
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return false
		}
		p := c.Snapshot()
		return verified(p)
	}
	if time.Since(c.recoveryAt) < time.Minute {
		// A request rejected just before another request repaired the profile
		// can replay using that result, without launching another probe.
		fresh := verified(c.current) && !c.current.CheckedAt.Before(c.recoveryAt)
		c.mu.Unlock()
		return fresh
	}
	c.recoveryAt = time.Now()
	c.recoveryAttempts++
	c.recovering = true
	c.recoveryDone = make(chan struct{})
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.recovering = false; close(c.recoveryDone); c.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	p := c.RefreshModel(ctx, model)
	return verified(p)
}

// Restore only reinstates bounded, verified profiles. Stale profiles are
// usable as candidates but will still be checked by the background monitor.
func (c *Cache) Restore(p *Profile) bool {
	if p == nil || p.Source != "probe" || p.Verdict != "ok" || !p.Identity.Valid() || !validClientFormat(p.ClientFormat) || !validToolDescriptions(p.ToolDescriptions) || len(p.UserAgent) > 256 || len(p.RequiredTools) > 64 || p.CheckedAt.IsZero() {
		return false
	}
	if len(p.ProbeModel) > 256 || strings.ContainsAny(p.ProbeModel, "\r\n\t") {
		return false
	}
	if !versionPattern.MatchString(versionFromUA(p.UserAgent)) {
		return false
	}
	if p.BaseURL != "" && !IsOfficialEndpoint(p.BaseURL) {
		return false
	}
	for _, name := range p.RequiredTools {
		if len(name) == 0 || len(name) > 64 {
			return false
		}
	}
	copy := *p
	if copy.ClientFormat == "" {
		copy.ClientFormat = ClientV1
	}
	c.mu.Lock()
	c.current = &copy
	c.lastGood = &copy
	c.mu.Unlock()
	return true
}
func versionFromUA(ua string) string {
	const prefix = "opencode/"
	if len(ua) < len(prefix) || ua[:len(prefix)] != prefix {
		return ""
	}
	v := ua[len(prefix):]
	if parts := strings.Split(v, "/"); len(parts) == 3 {
		return parts[1]
	}
	for i, c := range v {
		if c == ' ' {
			return v[:i]
		}
	}
	return v
}

func (c *Cache) NeedsRefresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.inflight || c.recovering {
		return false
	}
	if c.current != nil && c.current.Source == "probe" && time.Since(c.current.CheckedAt) < c.TTL {
		return false
	}
	return c.lastAttempt.IsZero() || time.Since(c.lastAttempt) >= 5*time.Minute
}
