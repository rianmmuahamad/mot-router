package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rian/motgate/internal/store"
)

func TestDashboardAccountIsolationAndBounds(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	alice, _ := st.CreateUser("Alice <script>", time.Now().Add(time.Hour).UnixMilli())
	bob, _ := st.CreateUser("Bob", 0)
	for _, v := range []store.UsageInput{{UserID: alice.ID, Model: "model_one", Status: 200, InTokens: 10, OutTokens: 5}, {UserID: alice.ID, Model: "modelXone", Status: 429, InTokens: 3}, {UserID: bob.ID, Model: "private-bob", Status: 200, InTokens: 9999}} {
		if err := st.AddUsage(v); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := New(Config{PublicURL: "https://arbyte.coddx.store", AdminPassword: "test"}, st)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	w := call("/api/user/" + alice.Key + "/usage?limit=1&period=7d")
	if w.Code != 200 {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var result store.UserUsage
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || len(result.Requests) != 1 || len(result.Buckets) != 7 {
		t.Fatalf("unexpected pagination/buckets: %+v", result)
	}
	if result.Requests[0].Status != 429 {
		t.Fatal("newest requests must sort first")
	}
	var in, out int64
	for _, b := range result.Buckets {
		in += b.TokensIn
		out += b.TokensOut
	}
	if in != 13 || out != 5 {
		t.Fatalf("cross-account accounting: %d/%d", in, out)
	}
	if strings.Contains(w.Body.String(), "private-bob") || strings.Contains(w.Body.String(), bob.Key) {
		t.Fatal("other account leaked")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("usage may be cached")
	}
	w = call("/api/user/" + alice.Key + "/usage?page=2&limit=1")
	json.Unmarshal(w.Body.Bytes(), &result)
	if len(result.Requests) != 1 || result.Requests[0].TokensIn != 10 {
		t.Fatal("wrong page")
	}
	w = call("/api/user/" + alice.Key + "/usage?search=model_one")
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.Total != 1 {
		t.Fatal("search underscores must be literal")
	}
	for _, query := range []string{"limit=201", "limit=-1", "page=0", "page=10001", "period=1y"} {
		if w := call("/api/user/" + alice.Key + "/usage?" + query); w.Code != 400 {
			t.Errorf("bounds %s: %d", query, w.Code)
		}
	}
	if call("/api/user/unknown/usage").Code != 404 {
		t.Fatal("unknown key allowed")
	}
	if call("/api/admin/users").Code != 401 {
		t.Fatal("admin auth changed")
	}
	w = call("/u/" + alice.Key)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "stat-requests") || strings.Contains(w.Body.String(), "mg_demo_") {
		t.Fatal("live dashboard not served")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("user credential page headers missing")
	}
	for _, path := range []string{"/assets/fonts/InterVariable.woff2", "/assets/fonts/JetBrainsMono-Regular.woff2"} {
		w = call(path)
		if w.Code != 200 || !strings.HasPrefix(w.Body.String(), "wOF2") {
			t.Fatalf("font not embedded: %s", path)
		}
	}
	st.SetRevoked(alice.ID, true)
	if call("/api/user/"+alice.Key+"/usage").Code != 200 {
		t.Fatal("revoked account cannot see its own history")
	}
	w = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+alice.Key)
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("revoked inference gate changed")
	}
}

func TestUsageVisibleAfterStreamingInference(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"real-model\",\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, _ := st.CreateUser("A", time.Now().Add(time.Hour).UnixMilli())
	srv, _ := New(Config{PublicURL: "https://arbyte.coddx.store", Upstream: upstream.URL}, st)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+u.Key)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("stream altered: %d", w.Code)
	}
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/user/"+u.Key+"/usage", nil))
	var result store.UserUsage
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.Total != 1 || result.Requests[0].TokensIn != 11 || result.Requests[0].TokensOut != 7 || result.Requests[0].Model != "real-model" {
		t.Fatalf("missing real usage: %+v", result)
	}
}
