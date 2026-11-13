package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"geoint/internal/api"
)

// TestEveryRouteExceptHealthRequiresToken walks the very table the router
// registers from: a route cannot be added without auth or without this test
// covering it.
func TestEveryRouteExceptHealthRequiresToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	routes := api.Routes()
	if len(routes) != 12 {
		t.Fatalf("route table has %d routes, want 12 (update this test with the new route)", len(routes))
	}
	for _, r := range routes {
		if r.Role != "analyst" && r.Role != "admin" {
			t.Errorf("%s %s: role %q", r.Method, r.Pattern, r.Role)
		}
		path := strings.NewReplacer("{id}", "1", "{key}", "receita").Replace(r.Pattern)
		for _, tok := range []string{"", "not-a-valid-token"} {
			wantError(t, e.do(r.Method, path, tok, "{}"), http.StatusUnauthorized, "unauthorized")
		}
	}
	if rec := e.do("GET", "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

func TestRoles(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	// An analyst on the admin route.
	wantError(t, e.do("POST", "/api/v1/sources/receita/collect", tokAnalyst, ""), http.StatusForbidden, "forbidden")
	// Admin tokens carry analyst too (composite role in the realm).
	want[[]any](t, e.do("GET", "/api/v1/sources", tokAdmin, ""), http.StatusOK)
	// Role matching is exact: admin alone is not analyst.
	wantError(t, e.do("GET", "/api/v1/sources", tokAdminOnly, ""), http.StatusForbidden, "forbidden")
}

func TestCORSPreflight(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	rec := e.do("OPTIONS", "/api/v1/search", "", "",
		"Origin", "http://localhost:5173", "Access-Control-Request-Method", "GET",
		"Access-Control-Request-Headers", "authorization")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: %d %s", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != "http://localhost:5173" ||
		!strings.Contains(h.Get("Access-Control-Allow-Headers"), "Authorization") ||
		!strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") ||
		h.Get("Access-Control-Allow-Credentials") != "" || !strings.Contains(h.Get("Vary"), "Origin") {
		t.Fatalf("preflight headers: %v", h)
	}

	// From another origin the preflight is not answered: it falls through
	// (405 here) without CORS headers.
	rec = e.do("OPTIONS", "/api/v1/search", "", "",
		"Origin", "https://evil.example", "Access-Control-Request-Method", "GET")
	if rec.Code == http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "" ||
		rec.Header().Get("Access-Control-Allow-Methods") != "" {
		t.Fatalf("preflight from another origin: %d %v", rec.Code, rec.Header())
	}
	// A real request from the allowed origin gets the origin echoed; another does not.
	if got := e.do("GET", "/api/v1/sources", tokAnalyst, "", "Origin", "http://localhost:5173").
		Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("allow origin = %q", got)
	}
	if got := e.do("GET", "/api/v1/sources", tokAnalyst, "", "Origin", "https://evil.example").
		Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow origin for another origin = %q", got)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	for _, rec := range []struct {
		name string
		code int
		resp interface {
			Header() http.Header
		}
	}{
		{"ok", 200, e.do("GET", "/api/v1/sources", tokAnalyst, "")},
		{"unauthorized", 401, e.do("GET", "/api/v1/sources", "", "")},
		{"forbidden", 403, e.do("POST", "/api/v1/sources/receita/collect", tokAnalyst, "")},
		{"not found", 404, e.do("GET", "/api/v1/nothing", tokAnalyst, "")},
		{"health", 200, e.do("GET", "/healthz", "", "")},
	} {
		h := rec.resp.Header()
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" ||
			h.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("%s: headers %v", rec.name, h)
		}
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	wantError(t, e.do("GET", "/api/v1/nothing", tokAnalyst, ""), http.StatusNotFound, "not_found")
	wantError(t, e.do("DELETE", "/api/v1/sources", tokAnalyst, ""), http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestBodyTooLarge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	big := `{"type":"paths","params":{"a":1,"b":2},"pad":"` + strings.Repeat("x", 1<<20) + `"}`
	wantError(t, e.do("POST", "/api/v1/jobs", tokAnalyst, big), http.StatusRequestEntityTooLarge, "body_too_large")
}

func TestBadBodiesAndIDs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/v1/jobs", `{"type":"paths","params":{"a":1,"b":2},"extra":1}`},
		{"POST", "/api/v1/jobs", `{"type":"paths","params":{"a":1,"b":2}} {}`},
		{"POST", "/api/v1/jobs", `{"type":"paths"`},
		{"POST", "/api/v1/jobs", `{"type":"nope","params":{}}`},
		{"POST", "/api/v1/jobs", `{"type":"paths","params":{"a":1}}`},
		{"POST", "/api/v1/identity/merge", `{"record_ids":[1,2],"reason":"x","who":"me"}`},
		{"POST", "/api/v1/identity/merge", `{"record_ids":[1],"reason":"x"}`},
		{"POST", "/api/v1/identity/merge", `{"record_ids":[1,2],"reason":""}`},
		{"POST", "/api/v1/identity/split", `{"record_ids":[],"reason":"x"}`},
		{"POST", "/api/v1/identity/proposals/1/decision", `{}`},
		{"POST", "/api/v1/identity/proposals/1/decision", `{"confirm":"yes"}`},
		{"POST", "/api/v1/sources/receita/collect", `{"cnpjs":["123"]}`},
		{"POST", "/api/v1/sources/receita/collect", `{"cnpjs":["12345678"],"depth":4}`},
		{"POST", "/api/v1/sources/receita/collect", `{"depth":1}`},
		{"POST", "/api/v1/sources/receita/collect", `{"cnpjs":null}`},
		{"POST", "/api/v1/sources/receita/collect", `{"depth":null}`},
		{"POST", "/api/v1/sources/receita/collect", `{"cnpjs":[]}`},
		{"POST", "/api/v1/sources/receita/collect", `{"cnpjs":["12345678"],"depth":null}`},
		{"POST", "/api/v1/sources/receita/collect", `{"cnpjs":"12345678"}`},
		{"POST", "/api/v1/sources/receita/collect", `null`},
		{"GET", "/api/v1/entities/abc", ""},
		{"GET", "/api/v1/entities/0", ""},
		{"GET", "/api/v1/entities/-1", ""},
		{"GET", "/api/v1/entities/99999999999999999999", ""},
		{"GET", "/api/v1/entities/1/relationships?state=maybe", ""},
		{"GET", "/api/v1/entities/1/relationships?type=friend_of", ""},
		{"GET", "/api/v1/entities/1/relationships?at=2024-13-01", ""},
		{"GET", "/api/v1/entities/1/relationships?limit=0", ""},
		{"GET", "/api/v1/entities/1/relationships?limit=x", ""},
		{"GET", "/api/v1/relationships/x/provenance", ""},
		{"GET", "/api/v1/jobs/not-a-uuid", ""},
		{"GET", "/api/v1/identity/proposals?status=whatever", ""},
		{"GET", "/api/v1/identity/proposals?cursor=@@", ""},
		{"POST", "/api/v1/identity/proposals/x/decision", `{"confirm":true}`},
		{"GET", "/api/v1/search?q=empresa&limit=abc", ""},
		{"GET", "/api/v1/search?q=empresa&kind=planet", ""},
		{"GET", "/api/v1/search?q=" + strings.Repeat("a", 201), ""},
	} {
		tok := tokAnalyst
		if strings.Contains(c.path, "collect") {
			tok = tokAdmin
		}
		rec := e.do(c.method, c.path, tok, c.body)
		if rec.Code != http.StatusBadRequest || rec.Body.String() != "{\"error\":\"bad_request\"}\n" {
			t.Errorf("%s %s %s: %d %s", c.method, c.path, c.body, rec.Code, rec.Body.String())
		}
	}
	wantError(t, e.do("GET", "/api/v1/entities/123456789", tokAnalyst, ""), http.StatusNotFound, "not_found")
	wantError(t, e.do("GET", "/api/v1/entities/123456789/relationships", tokAnalyst, ""), http.StatusNotFound, "not_found")
	wantError(t, e.do("GET", "/api/v1/relationships/123456789/provenance", tokAnalyst, ""), http.StatusNotFound, "not_found")
	wantError(t, e.do("POST", "/api/v1/sources/nope/collect", tokAdmin, ""), http.StatusNotFound, "not_found")
	wantError(t, e.do("POST", "/api/v1/identity/proposals/123456/decision", tokAnalyst, `{"confirm":true}`),
		http.StatusNotFound, "not_found")
}

func TestPanicIs500WithoutLeak(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	rec := e.do("GET", "/api/v1/sources", tokPanic, "")
	wantError(t, rec, http.StatusInternalServerError, "internal")
	if strings.Contains(rec.Body.String(), secretPanicText) {
		t.Fatal("panic text leaked")
	}
}

func TestTimeoutIs504(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false, func(d *api.Deps) { d.Timeout = 50 * time.Millisecond })
	start := time.Now()
	wantError(t, e.do("GET", "/api/v1/sources", tokSlow, ""), http.StatusGatewayTimeout, "timeout")
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestAuditFailureFailsTheRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true, func(d *api.Deps) {
		d.Audit = func(context.Context, string, string, *int64, map[string]any) error { return errors.New("audit down") }
	})
	rec := e.do("GET", "/api/v1/entities/"+itoa(e.ids.H), tokAnalyst, "")
	wantError(t, rec, http.StatusInternalServerError, "internal")
	if strings.Contains(rec.Body.String(), "HOLDING") {
		t.Fatal("entity data sent without audit")
	}
}

// captureLogs sends slog output to a buffer until the test ends. Not
// parallel: it swaps the global logger.
func captureLogs(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestPanicAfterTimeoutIsLogged(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t, false, func(d *api.Deps) { d.Timeout = 50 * time.Millisecond })
	wantError(t, e.do("GET", "/api/v1/sources?secret=query", tokLatePanic, ""), http.StatusGatewayTimeout, "timeout")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "panic after the response was given up") {
		if time.Now().After(deadline) {
			t.Fatalf("late panic not logged: %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out := logs.String(); !strings.Contains(out, `"stack"`) || strings.Contains(out, "secret=query") ||
		strings.Contains(out, tokLatePanic) {
		t.Fatalf("log: %s", out)
	}
}

func TestClientGoneIs499Quietly(t *testing.T) {
	logs := captureLogs(t)
	e := newEnv(t, false)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	r := httptest.NewRequest("GET", "/api/v1/sources", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tokAnalyst)
	r.Host = testHost
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Body.Len() != 0 {
		t.Fatalf("answer written to a gone client: %s", rec.Body.String())
	}
	out := logs.String()
	if !strings.Contains(out, `"status":499`) || strings.Contains(out, `"level":"ERROR"`) {
		t.Fatalf("log: %s", out)
	}
}

// The Host header must name an allowed host (DNS-rebinding defence): the
// defaults with any port, plus Deps.AllowedHosts; everything else, /healthz
// and preflights included, is 421 misdirected_request.
func TestHostAllowList(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false, func(d *api.Deps) { d.AllowedHosts = []string{"geoint.example.org"} })
	for _, h := range []string{"localhost:8080", "localhost", "LOCALHOST:8080", "127.0.0.1:8080", "[::1]:8080", "[::1]",
		"api:8080", "geoint.example.org", "geoint.example.org:443"} {
		if rec := e.do("GET", "/healthz", "", "", "Host", h); rec.Code != http.StatusOK {
			t.Errorf("Host %q: /healthz status %d, want 200", h, rec.Code)
		}
		if rec := e.do("GET", "/api/v1/sources", tokAnalyst, "", "Host", h); rec.Code != http.StatusOK {
			t.Errorf("Host %q: /api/v1/sources status %d, want 200", h, rec.Code)
		}
	}
	for _, h := range []string{"evil.example", "evil.example:8080", "localhost.evil.example", "geoint.example.org.evil",
		"127.0.0.2:8080", "", "example.com"} {
		for _, req := range []struct{ method, path, token string }{
			{"GET", "/healthz", ""}, {"GET", "/api/v1/sources", tokAnalyst}, {"OPTIONS", "/api/v1/sources", ""},
		} {
			rec := e.do(req.method, req.path, req.token, "", "Host", h, "Origin", api.DefaultWebOrigin,
				"Access-Control-Request-Method", "GET")
			wantError(t, rec, http.StatusMisdirectedRequest, "misdirected_request")
			if rec.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Errorf("Host %q %s %s: CORS headers on a refused host", h, req.method, req.path)
			}
		}
	}
}

func TestParseAllowedHosts(t *testing.T) {
	got, err := api.ParseAllowedHosts(" Geoint.Example.org, ,10.0.0.5,[fd00::1] ")
	if err != nil || !slices.Equal(got, []string{"geoint.example.org", "10.0.0.5", "fd00::1"}) {
		t.Fatalf("ParseAllowedHosts = %v, %v", got, err)
	}
	if got, err := api.ParseAllowedHosts(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{"http://geoint.example.org", "geoint.example.org:8080", "*.example.org", "a/b", "user@host", "-a.example"} {
		if _, err := api.ParseAllowedHosts(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
