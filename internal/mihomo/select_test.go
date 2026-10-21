package mihomo

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExpectedSelectionUsesNativeCASAndReportsConflict(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "next" || body["expected_now"] != "old" || r.Method != "PUT" {
			t.Errorf("CAS contract lost: %v", body)
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer stub.Close()
	err := New(stub.URL, "", "").SelectExpected(t.Context(), "MOT", "next", "old")
	if !errors.Is(err, ErrSelectionChanged) {
		t.Fatalf("conflict identity lost: %v", err)
	}
}
