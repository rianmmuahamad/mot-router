package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rian/motgate/internal/store"
)

func TestAdminCustomTokenPackageAndTopUpAPI(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := New(Config{PublicURL: "https://arbyte.example", Upstream: "http://localhost", AdminPassword: "test", DefaultHours: 24}, st)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.SetBasicAuth("admin", "test")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	w := call(http.MethodPost, "/api/admin/users", `{"name":"user_b","plan_type":"token","token_limit":50000000,"days":3}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", w.Code, w.Body.String())
	}
	var u map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u["plan_type"] != "token" || u["token_limit"].(float64) != 50000000 || u["quota_used"].(float64) != 0 {
		t.Fatalf("unexpected new plan response: %#v", u)
	}
	id := int(u["id"].(float64))
	w = call(http.MethodPost, "/api/admin/users/"+itoa(id)+"/topup", `{"token_limit":1250000,"days":7}`)
	if w.Code != http.StatusOK {
		t.Fatalf("top-up returned %d: %s", w.Code, w.Body.String())
	}
	if err = json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u["token_limit"].(float64) != 51250000 {
		t.Fatalf("top-up did not add token amount: %#v", u)
	}
	if w = call(http.MethodPost, "/api/admin/users", `{"name":"bad","hours":24,"days":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("mixed durations should be rejected: %d", w.Code)
	}
	if w = call(http.MethodPost, "/api/admin/users", `{"name":"bad","hours":null,"days":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("both duration fields must be rejected even when one is null: %d", w.Code)
	}
	if w = call(http.MethodPost, "/api/admin/users", `{"name":"bad","plan_type":"token","token_limit":1,"days":366}`); w.Code != http.StatusBadRequest {
		t.Fatalf("duration above 365 days should be rejected: %d", w.Code)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
