package conformance

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polanski13/idemkit"
	"github.com/polanski13/idemkit/store/mem"
)

func newMiddleware(t *testing.T, mode idemkit.ConflictMode) http.Handler {
	t.Helper()
	store := mem.New(mem.Config{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"resource_42"}`))
	})
	cfg := idemkit.Config{ConflictMode: mode}
	return idemkit.Middleware(store, cfg)(handler)
}

func doRequest(t *testing.T, mw http.Handler, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/resources", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	return rec
}
