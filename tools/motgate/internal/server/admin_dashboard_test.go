package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rian/motgate/internal/store"
)

func TestAdminDashboardAuthenticationAndRealUsage(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.CreateUser("admin dashboard account", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.AddUsage(store.UsageInput{UserID: u.ID, Model: "test-model", Surface: "openai", Status: 200, InTokens: 19, OutTokens: 11}); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{PublicURL: "https://arbyte.example", AdminPassword: "test", DefaultHours: 24}, st)
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string, authorized bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if authorized {
			r.SetBasicAuth("admin", "test")
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/admin", "/api/admin/usage", "/api/admin/users", "/api/admin/overview"} {
		w := call(path, false)
		if w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unprotected admin endpoint %s: %d", path, w.Code)
		}
	}
	w := call("/api/admin/usage?period=7d", true)
	if w.Code != 200 {
		t.Fatalf("chart status %d: %s", w.Code, w.Body.String())
	}
	var result store.AdminUsage
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var input, output int64
	for _, bucket := range result.Buckets {
		input += bucket.TokensIn
		output += bucket.TokensOut
	}
	if len(result.Buckets) != 7 || input != 19 || output != 11 {
		t.Fatalf("wrong graph: %+v", result)
	}
	start := time.UnixMilli(result.Since).In(time.FixedZone("WIB", 7*3600))
	if start.Hour() != 0 || start.Minute() != 0 {
		t.Fatal("chart is not aligned to Jakarta midnight")
	}
	if strings.Contains(w.Body.String(), u.Key) || strings.Contains(w.Body.String(), u.Name) {
		t.Fatal("graph leaked account credentials")
	}
	if call("/api/admin/usage?period=1y", true).Code != 400 {
		t.Fatal("unbounded period permitted")
	}
	w = call("/api/admin/overview", true)
	if !strings.Contains(w.Body.String(), `"api_base":"https://arbyte.example/v1"`) {
		t.Fatal("API base not sourced from configuration")
	}
	w = call("/admin", true)
	if w.Code != 200 || w.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(w.Body.String(), "/assets/admin/dashboard.js") {
		t.Fatal("live modern admin page not served")
	}
	for _, path := range []string{"/assets/admin/dashboard.js", "/assets/admin/dashboard.css"} {
		w = call(path, false)
		if w.Code != 200 || strings.Contains(w.Body.String(), "mg_demo_") || strings.Contains(w.Body.String(), "initialAccounts") {
			t.Fatalf("invalid production asset: %s", path)
		}
	}
}
