package api_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"geoint/internal/api"
)

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Host = "127.0.0.1:8080"
	api.NewRouter(api.Deps{Verifier: fakeVerifier{}}).ServeHTTP(rec, r)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
