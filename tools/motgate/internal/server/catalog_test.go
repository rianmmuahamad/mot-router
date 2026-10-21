package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rian/motgate/internal/store"
)

func TestCatalogAdminAndPublicLandingData(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := New(Config{AdminPassword: "test"}, st)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body []byte, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if auth {
			r.SetBasicAuth("admin", "test")
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	if got := call(http.MethodGet, "/api/admin/catalog", nil, false).Code; got != http.StatusUnauthorized {
		t.Fatalf("admin catalog unauthenticated status = %d", got)
	}
	if got := call(http.MethodGet, "/admin/catalog", nil, false).Code; got != http.StatusUnauthorized {
		t.Fatalf("catalog page unauthenticated status = %d", got)
	}
	page := call(http.MethodGet, "/admin/catalog", nil, true)
	if page.Code != 200 || !bytes.Contains(page.Body.Bytes(), []byte("Model & paket")) {
		t.Fatalf("catalog page not served: %d", page.Code)
	}
	w := call(http.MethodGet, "/api/catalog", nil, false)
	if w.Code != 200 {
		t.Fatalf("public catalog status %d: %s", w.Code, w.Body.String())
	}
	var c store.Catalog
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Models) != 4 || len(c.Packages) != 1 || c.Models[0].ID == "" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	c.Models[0].Enabled = false
	c.Packages[0].Price = "Rp31.000"
	b, _ := json.Marshal(c)
	if got := call(http.MethodPut, "/api/admin/catalog", b, true).Code; got != 200 {
		t.Fatalf("save status %d", got)
	}
	w = call(http.MethodGet, "/api/catalog", nil, false)
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Models) != 3 || c.Packages[0].Price != "Rp31.000" {
		t.Fatalf("public catalog did not reflect edits: %+v", c)
	}
	var legacy map[string]any
	if err := json.Unmarshal(b, &legacy); err != nil {
		t.Fatal(err)
	}
	legacyPackages := legacy["packages"].([]any)
	legacyPackages[0].(map[string]any)["features"] = "fitur pertama\nfitur kedua"
	b, _ = json.Marshal(legacy)
	if got := call(http.MethodPut, "/api/admin/catalog", b, true).Code; got != 200 {
		t.Fatalf("legacy form save status %d", got)
	}
	w = call(http.MethodGet, "/api/catalog", nil, false)
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Packages[0].Features) != 2 {
		t.Fatalf("legacy feature text was not normalized: %#v", c.Packages[0].Features)
	}
	c.Packages[0].URL = "javascript:alert(1)"
	b, _ = json.Marshal(c)
	if got := call(http.MethodPut, "/api/admin/catalog", b, true).Code; got != http.StatusBadRequest {
		t.Fatalf("unsafe CTA URL accepted: %d", got)
	}
}

func TestPackageModelSelectionPersistsAndValidates(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.GetCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if c.Packages[0].ModelScope != "" {
		t.Fatal("legacy package should keep implicit all-model scope")
	}
	c.Packages[0].ModelScope = "selected"
	c.Packages[0].Models = []string{c.Models[0].ID, c.Models[2].ID}
	if err := validateCatalog(*c); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCatalog(t.Context(), *c); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err = st.GetCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if c.Packages[0].ModelScope != "selected" || len(c.Packages[0].Models) != 2 || c.Packages[0].Models[1] != c.Models[2].ID {
		t.Fatalf("package model selection lost after restart: %+v", c.Packages[0])
	}
	for _, ids := range [][]string{nil, {"unknown-model"}, {c.Models[0].ID, c.Models[0].ID}} {
		c.Packages[0].Models = ids
		if validateCatalog(*c) == nil {
			t.Fatalf("invalid model selection accepted: %v", ids)
		}
	}
	c.Packages[0].ModelScope = "all"
	c.Packages[0].Models = nil
	if err := validateCatalog(*c); err != nil {
		t.Fatalf("all-model scope rejected: %v", err)
	}
}

func TestRouterCatalogAuthenticationAndNormalization(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("wrong path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"deepseek-v4.1-flash-free","owned_by":"opencode"},{"id":"codebuddy/deepseek-v4.1-flash","owned_by":"codebuddy"},{"id":"deepseek-v4.1-flash-free"},{"id":"<invalid>"}]}`))
	}))
	defer upstream.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := New(Config{AdminPassword: "test", ModelCatalogURL: upstream.URL}, st)
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/api/admin/catalog/router-models", nil)
		if auth {
			r.SetBasicAuth("admin", "test")
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if !auth {
			if w.Code != 401 {
				t.Fatal(w.Code)
			}
			continue
		}
		var result struct {
			Models []struct {
				ID       string `json:"id"`
				Provider string `json:"provider"`
			} `json:"models"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Models) != 2 || result.Models[1].Provider != "codebuddy" {
			t.Fatalf("unexpected catalog: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestRouterCatalogInvalidResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"error":"unavailable"}`)) }))
	defer upstream.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := New(Config{AdminPassword: "test", ModelCatalogURL: upstream.URL}, st)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/admin/catalog/router-models", nil)
	r.SetBasicAuth("admin", "test")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 502 {
		t.Fatalf("status %d", w.Code)
	}
	// A failed live query must leave the saved marketing catalogue intact.
	c, err := st.GetCatalog(t.Context())
	if err != nil || len(c.Models) != 4 {
		t.Fatal("saved catalog changed", err)
	}
}
