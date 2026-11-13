package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/api"
	"geoint/internal/auth"
	"geoint/internal/freshness"
	"geoint/internal/graph"
	"geoint/internal/graph/graphtest"
	"geoint/internal/identity"
	"geoint/internal/jobs"
	"geoint/internal/queue"
	"geoint/internal/search"
	"geoint/internal/testutil"
)

var bg = context.Background()

// Tokens the fake verifier accepts.
const (
	tokAnalyst   = "tok-analyst"
	tokAnalyst2  = "tok-analyst-2"
	tokAdmin     = "tok-admin"      // admin and analyst, as the realm's composite role gives
	tokAdminOnly = "tok-admin-only" // admin without analyst
	tokPanic     = "tok-panic"      // the verifier panics
	tokSlow      = "tok-slow"       // the verifier blocks until the request ends
	tokLatePanic = "tok-late-panic" // the verifier panics once the request has ended
)

const secretPanicText = "secret-panic-detail"

type fakeVerifier map[string]auth.User

func (f fakeVerifier) Verify(ctx context.Context, raw string) (auth.User, error) {
	switch raw {
	case tokPanic:
		panic(secretPanicText)
	case tokLatePanic:
		<-ctx.Done()
		panic(secretPanicText)
	case tokSlow:
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return auth.User{}, ctx.Err()
	}
	u, ok := f[raw]
	if !ok {
		return auth.User{}, errors.New("unknown token")
	}
	return u, nil
}

var verifier = fakeVerifier{
	tokAnalyst:   {Sub: "sub-analyst", Roles: []string{"analyst"}},
	tokAnalyst2:  {Sub: "sub-analyst-2", Roles: []string{"analyst"}},
	tokAdmin:     {Sub: "sub-admin", Roles: []string{"admin", "analyst"}},
	tokAdminOnly: {Sub: "sub-admin-only", Roles: []string{"admin"}},
}

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *queue.Client
	deps api.Deps
	h    http.Handler
	ids  graphtest.IDs
}

// newEnv builds the API on a fresh schema and queue. seed loads the graph
// test fixture (and rebuilds search on it); mod adjusts the dependencies.
func newEnv(t *testing.T, seed bool, mod ...func(*api.Deps)) *env {
	t.Helper()
	pool := testutil.DB(t)
	q := testutil.Queue(t)
	e := &env{t: t, pool: pool, q: q}
	if seed {
		e.ids = graphtest.Seed(t, pool)
	} else {
		testutil.SeedSources(t, pool)
	}
	js := jobs.New(pool, q)
	jobs.RegisterGraphTypes(js)
	e.deps = api.Deps{
		Pool:     pool,
		Graph:    graph.New(pool),
		Search:   search.New(pool),
		Jobs:     js,
		Identity: &identity.Resolver{Pool: pool},
		Fresh:    freshness.New(pool, time.Now),
		Queue:    q,
		Verifier: verifier,
	}
	for _, m := range mod {
		m(&e.deps)
	}
	e.h = api.NewRouter(e.deps)
	return e
}

func (e *env) rebuildSearch() {
	e.t.Helper()
	if _, err := search.New(e.pool).Rebuild(bg); err != nil {
		e.t.Fatal(err)
	}
}

// testHost is the Host header of test requests.
const testHost = "localhost:8080"

// do sends a request; headers are name, value pairs ("Host" sets the Host).
func (e *env) do(method, path, token, body string, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Host = testHost // as the browser reaches the API (httptest's default, example.com, is refused)
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			r.Host = headers[i+1]
			continue
		}
		r.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

// want fails unless rec has the status; it returns the decoded JSON body.
func want[T any](t *testing.T, rec *httptest.ResponseRecorder, status int) T {
	t.Helper()
	var v T
	if rec.Code != status {
		t.Fatalf("status %d, want %d; body %s", rec.Code, status, rec.Body.String())
	}
	if status == http.StatusNoContent {
		return v
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

// wantError fails unless rec is status with {"error": code}.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	got := want[map[string]string](t, rec, status)
	if len(got) != 1 || got["error"] != code {
		t.Fatalf("body %s, want {\"error\":%q}", rec.Body.String(), code)
	}
}

// auditCount counts audit rows of action (and entity, when not nil).
func (e *env) auditCount(action string, entity *int64) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(bg, `SELECT count(*) FROM audit_log WHERE action = $1 AND ($2::bigint IS NULL OR entity_id = $2)`,
		action, entity).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

type entityRef struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	NaturalKey string `json:"natural_key"`
}

type edge struct {
	RelationshipID     int64          `json:"relationship_id"`
	Type               string         `json:"type"`
	Src                entityRef      `json:"src"`
	Dst                entityRef      `json:"dst"`
	Kind               string         `json:"kind"`
	Confidence         *float64       `json:"confidence"`
	IdentityConfidence *float64       `json:"identity_confidence"`
	ValidFrom          *string        `json:"valid_from"`
	ValidToAfter       *string        `json:"valid_to_after"`
	ValidToBefore      *string        `json:"valid_to_before"`
	Attrs              map[string]any `json:"attrs"`
	SourceKey          string         `json:"source_key"`
	CollectionID       int64          `json:"collection_id"`
	Stale              bool           `json:"stale"`
	Closed             *bool          `json:"closed"`
	IdentityBasis      *string        `json:"identity_basis"`
}
