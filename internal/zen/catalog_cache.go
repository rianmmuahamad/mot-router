package zen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxCatalogBytes = 8 << 20

type catalogEntry struct {
	Models    []Model   `json:"models"`
	CheckedAt time.Time `json:"checked_at"`
	retryAt   time.Time
	err       error
	inflight  chan struct{}
}
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]*catalogEntry
	file    string
	source  string
	ttl     time.Duration
}

func newCatalogCache() *catalogCache {
	return &catalogCache{entries: map[string]*catalogEntry{}, ttl: 10 * time.Minute}
}

// ConfigureCatalogCache runs before serving requests. source may point at a
// provider catalog; it changes inventory only, never inference URLs or identity.
func (c *Client) ConfigureCatalogCache(file, source string) error {
	if c.catalog == nil {
		c.catalog = newCatalogCache()
	}
	cache := c.catalog
	if source != "" {
		u, err := url.Parse(source)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
			return fmt.Errorf("catalog URL must use HTTPS without credentials or query parameters")
		}
	}
	cache.file, cache.source = file, source
	if file == "" {
		return nil
	}
	f, err := os.Open(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var saved struct {
		Version   int       `json:"version"`
		Key       string    `json:"key"`
		Models    []Model   `json:"models"`
		CheckedAt time.Time `json:"checked_at"`
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxCatalogBytes {
		return fmt.Errorf("cached catalog exceeds limit")
	}
	if json.Unmarshal(data, &saved) != nil || saved.Version != 1 || saved.CheckedAt.IsZero() || saved.CheckedAt.After(time.Now().Add(time.Minute)) {
		return fmt.Errorf("invalid cached catalog")
	}
	if err := validateModels(saved.Models); err != nil {
		return err
	}
	key := c.catalogKey()
	if saved.Key == key {
		cache.entries[key] = &catalogEntry{Models: saved.Models, CheckedAt: saved.CheckedAt}
	}
	return nil
}

func (c *Client) catalogKey() string {
	source := c.EffectiveBaseURL("") + "/models"
	if c.catalog != nil && c.catalog.source != "" {
		source = c.catalog.source
	}
	// Keying by the credential hash prevents inventory leaking across accounts.
	sum := sha256.Sum256([]byte(source + "\x00" + c.UpstreamKey))
	return hex.EncodeToString(sum[:])
}
func cloneModels(models []Model) []Model {
	out := append([]Model(nil), models...)
	for i := range out {
		if out[i].Reasoning != nil {
			value := *out[i].Reasoning
			out[i].Reasoning = &value
		}
	}
	return out
}
func (c *Client) listCachedModels(ctx context.Context) ([]Model, error) {
	cache := c.catalog
	if cache == nil {
		return c.fetchModels(ctx)
	}
	key := c.catalogKey()
	for {
		cache.mu.Lock()
		e := cache.entries[key]
		if e == nil {
			// Only a few profile endpoints can be active. Evict idle old entries.
			if len(cache.entries) >= 4 {
				for k, v := range cache.entries {
					if v.inflight == nil {
						delete(cache.entries, k)
						break
					}
				}
			}
			e = &catalogEntry{}
			cache.entries[key] = e
		}
		if len(e.Models) > 0 && time.Since(e.CheckedAt) < cache.ttl {
			models := cloneModels(e.Models)
			cache.mu.Unlock()
			return models, nil
		}
		if time.Now().Before(e.retryAt) {
			models, err := cloneModels(e.Models), e.err
			cache.mu.Unlock()
			if len(models) > 0 {
				return models, nil
			}
			return nil, err
		}
		if e.inflight != nil {
			done := e.inflight
			cache.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		e.inflight = done
		previous := cloneModels(e.Models)
		cache.mu.Unlock()
		if len(previous) > 0 {
			copy := c.requestClient(ctx)
			go copy.refreshCatalog(context.Background(), key, e, done)
			return previous, nil
		}
		return c.refreshCatalog(ctx, key, e, done)
	}
}
func (c *Client) refreshCatalog(ctx context.Context, key string, e *catalogEntry, done chan struct{}) ([]Model, error) {
	models, err := c.fetchModels(ctx)
	cache := c.catalog
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if err == nil {
		e.Models = cloneModels(models)
		e.CheckedAt = time.Now()
		e.retryAt = time.Time{}
		e.err = nil
		c.saveCatalog(key, e)
	} else {
		e.err = err
		delay := 30 * time.Second
		var upstream *UpstreamError
		if errors.As(err, &upstream) {
			delay = max(delay, upstream.RetryAfter)
		}
		e.retryAt = time.Now().Add(delay)
	}
	e.inflight = nil
	close(done)
	if err != nil && len(e.Models) > 0 {
		return cloneModels(e.Models), nil
	}
	return cloneModels(models), err
}

func (c *Client) saveCatalog(key string, e *catalogEntry) {
	file := c.catalog.file
	if file == "" {
		return
	}
	data, err := json.Marshal(struct {
		Version   int       `json:"version"`
		Key       string    `json:"key"`
		Models    []Model   `json:"models"`
		CheckedAt time.Time `json:"checked_at"`
	}{1, key, e.Models, e.CheckedAt})
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(file), 0700) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".catalog-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil && closeErr == nil {
		_ = os.Rename(tmp, file)
	}
}
func validateModels(models []Model) error {
	if len(models) == 0 || len(models) > 20000 {
		return fmt.Errorf("catalog must contain 1..20000 models")
	}
	seen := map[string]bool{}
	for _, m := range models {
		if m.ID == "" || len(m.ID) > 256 || strings.ContainsAny(m.ID, "\r\n\x00") || seen[m.ID] || m.MaxOutput < 0 || m.ContextWindow < 0 {
			return fmt.Errorf("invalid or duplicate catalog model")
		}
		seen[m.ID] = true
	}
	return nil
}
func (c *Client) fetchModels(ctx context.Context) ([]Model, error) {
	source := c.EffectiveBaseURL("") + "/models"
	if c.catalog != nil && c.catalog.source != "" {
		source = c.catalog.source
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mot-router/catalog")
	// Credentials belong only to the configured API, never the public metadata host.
	if c.catalog == nil || c.catalog.source == "" {
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("x-opencode-session", SessionID("models"))
		if c.UpstreamKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.UpstreamKey)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		e := upstreamError(resp, []byte(fmt.Sprintf("model catalog returned HTTP %d", resp.StatusCode)))
		return nil, e
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCatalogBytes {
		return nil, fmt.Errorf("model catalog exceeds 8 MiB")
	}
	models, err := decodeCatalog(data, c.UpstreamKey != "" && c.UpstreamKey != "public")
	if err != nil {
		return nil, err
	}
	if err = validateModels(models); err != nil {
		return nil, err
	}
	return models, nil
}
func decodeCatalog(data []byte, includePaid bool) ([]Model, error) {
	var payload struct {
		Data     []Model `json:"data"`
		OpenCode *struct {
			Models map[string]struct {
				Family    string `json:"family"`
				Status    string `json:"status"`
				Reasoning *bool  `json:"reasoning"`
				Cost      struct {
					Input  *float64 `json:"input"`
					Output *float64 `json:"output"`
				} `json:"cost"`
				Limit struct {
					Output  int `json:"output"`
					Context int `json:"context"`
				} `json:"limit"`
				Provider struct {
					NPM string `json:"npm"`
				} `json:"provider"`
			} `json:"models"`
		} `json:"opencode"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("invalid model catalog JSON")
	}
	models := []Model{}
	if payload.OpenCode != nil {
		for id, f := range payload.OpenCode.Models {
			if f.Status == "deprecated" || (!includePaid && (f.Cost.Input == nil || f.Cost.Output == nil || *f.Cost.Input != 0 || *f.Cost.Output != 0)) {
				continue
			}
			protocol := ""
			if strings.Contains(f.Provider.NPM, "anthropic") {
				protocol = "anthropic"
			} else if strings.Contains(f.Provider.NPM, "openai") {
				protocol = "openai"
			}
			models = append(models, Model{ID: id, Object: "model", OwnedBy: "opencode", Family: f.Family, Reasoning: f.Reasoning, MaxOutput: f.Limit.Output, ContextWindow: f.Limit.Context, Protocol: protocol, Status: f.Status})
		}
	} else {
		for _, m := range payload.Data {
			if m.Status != "deprecated" {
				models = append(models, m)
			}
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}
func (c *Client) CatalogStatus() map[string]any {
	if c.catalog == nil {
		return map[string]any{"cached": false}
	}
	cache := c.catalog
	cache.mu.Lock()
	defer cache.mu.Unlock()
	e := cache.entries[c.catalogKey()]
	if e == nil {
		return map[string]any{"cached": false}
	}
	out := map[string]any{"cached": len(e.Models) > 0, "models": len(e.Models), "checked_at": e.CheckedAt, "stale": time.Since(e.CheckedAt) >= cache.ttl, "refreshing": e.inflight != nil, "retry_at": e.retryAt}
	if e.err != nil {
		out["last_error"] = e.err.Error()
	}
	return out
}
