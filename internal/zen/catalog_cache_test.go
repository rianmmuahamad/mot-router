package zen

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatalogSharedRefreshDiskAndFailureBackoff(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"fixture-model","object":"model"}]}`)
	}))
	defer up.Close()
	client := New(up.URL, "", "")
	file := filepath.Join(t.TempDir(), "catalog.json")
	if err := client.ConfigureCatalogCache(file, ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models, err := client.WithHTTP(up.Client()).ListModels(context.Background())
			if err != nil || len(models) != 1 {
				t.Errorf("catalog: %v", err)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("refresh storm: %d", hits.Load())
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("cache file not private", err)
	}
	warm := New(up.URL, "", "")
	if err := warm.ConfigureCatalogCache(file, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := warm.ListModels(t.Context()); err != nil || hits.Load() != 1 {
		t.Fatal("warm cache fetched unnecessarily", err)
	}
	// Mutating a returned slice cannot corrupt another caller's snapshot.
	models, _ := client.ListModels(t.Context())
	models[0].ID = "changed"
	models, _ = client.ListModels(t.Context())
	if models[0].ID != "fixture-model" {
		t.Fatal("cache snapshot was mutable")
	}
	fail.Store(true)
	client.catalog.mu.Lock()
	client.catalog.entries[client.catalogKey()].CheckedAt = time.Now().Add(-time.Hour)
	client.catalog.mu.Unlock()
	start := time.Now()
	models, err = client.ListModels(t.Context())
	if err != nil || len(models) != 1 || time.Since(start) > time.Second {
		t.Fatal("stale cache unavailable")
	}
	deadline := time.Now().Add(2 * time.Second)
	for client.CatalogStatus()["refreshing"] == true && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for range 10 {
		if _, err := client.ListModels(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 2 || client.CatalogStatus()["last_error"] == nil {
		t.Fatal("missing catalog backoff or stale warning", hits.Load())
	}
	before, _ := os.ReadFile(file)
	if !strings.Contains(string(before), "fixture-model") {
		t.Fatal("failure destroyed cache")
	}
}

func TestCatalogMetadataRejectsPaidDeprecatedAndDuplicates(t *testing.T) {
	data := []byte(`{"opencode":{"models":{"active":{"cost":{"input":0,"output":0},"reasoning":true,"limit":{"output":32000,"context":1000000}},"paid-output":{"cost":{"input":0,"output":1}},"old":{"status":"deprecated","cost":{"input":0,"output":0}},"unknown-cost":{}}}}`)
	models, err := decodeCatalog(data, false)
	if err != nil || len(models) != 1 || models[0].ID != "active" || models[0].MaxOutput != 32000 {
		t.Fatal(models, err)
	}
	models, err = decodeCatalog([]byte(`{"data":[{"id":"a"},{"id":"a"}]}`), false)
	if err != nil || validateModels(models) == nil {
		t.Fatal("duplicate IDs accepted")
	}
}
func TestCatalogNeverSendsAPIKeyToMetadataSource(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("API key leaked into metadata request")
		}
		fmt.Fprint(w, `{"data":[{"id":"fixture"}]}`)
	}))
	defer up.Close()
	client := New("https://provider.invalid/v1", "", "PRIVATE_FIXTURE_KEY")
	if err := client.ConfigureCatalogCache("", up.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := client.ConfigureCatalogCache("", "http://public.invalid/catalog"); err == nil {
		t.Fatal("plaintext public metadata source allowed")
	}
}

func TestCatalogHonorsRetryAfterWithoutRepeatedFetch(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	}))
	defer up.Close()
	c := New(up.URL, "", "")
	for range 3 {
		if _, err := c.ListModels(t.Context()); err == nil {
			t.Fatal("failed fetch reported success")
		}
	}
	c.catalog.mu.Lock()
	retry := c.catalog.entries[c.catalogKey()].retryAt
	c.catalog.mu.Unlock()
	if hits.Load() != 1 || time.Until(retry) < 119*time.Second {
		t.Fatal("Retry-After or failure coalescing ignored")
	}
}
