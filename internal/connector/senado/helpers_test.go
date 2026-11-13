package senado

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/rawstore"
	"geoint/internal/testutil"
)

var ctx = context.Background()

// refs hands out reference dates: the raw store outlives a test, and Fetch
// reuses what is stored under a date, so every collection of the package
// gets its own.
var (
	refMu   sync.Mutex
	nextDay = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
)

func newRef() time.Time {
	refMu.Lock()
	defer refMu.Unlock()
	r := nextDay
	nextDay = nextDay.AddDate(0, 0, 1)
	return r
}

// fixtureLegislatures are the legislatures of testdata/senado.
var fixtureLegislatures = []int{56, 57}

// api serves testdata/senado as the Senado API does, under /dadosabertos.
type api struct {
	mu    sync.Mutex
	files map[string][]byte // response bodies by raw file name
	calls map[string]int    // requests by path?query
	// before, when set, may answer a request itself (returning true).
	before func(w http.ResponseWriter, r *http.Request) bool
}

func newAPI(t *testing.T) (*api, *httptest.Server) {
	t.Helper()
	a := &api{files: map[string][]byte{}, calls: map[string]int{}}
	matches, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "senado", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		a.files[filepath.Base(m)] = b
	}
	if len(a.files) == 0 {
		t.Fatal("no fixture")
	}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return a, srv
}

var (
	routeList      = regexp.MustCompile(`^/dadosabertos/senador/lista/legislatura/([0-9]+)$`)
	routeFiliacoes = regexp.MustCompile(`^/dadosabertos/senador/([0-9]+)/filiacoes$`)
)

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.calls[r.URL.Path+"?"+r.URL.RawQuery]++
	before := a.before
	a.mu.Unlock()
	if before != nil && before(w, r) {
		return
	}
	if r.Header.Get("User-Agent") != "geoint-dev" || r.Header.Get("Accept") != "application/json" {
		http.Error(w, "bad headers", http.StatusBadRequest)
		return
	}
	var name string
	switch p := r.URL.Path; {
	case routeList.MatchString(p):
		name = "senadores_" + routeList.FindStringSubmatch(p)[1] + ".json"
	case routeFiliacoes.MatchString(p):
		name = "senador_" + routeFiliacoes.FindStringSubmatch(p)[1] + "_filiacoes.json"
	}
	a.mu.Lock()
	b, ok := a.files[name]
	a.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (a *api) count(pathQuery string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[pathQuery]
}

// edit decodes the fixture name, lets f change it and stores it back.
func (a *api) edit(t *testing.T, name string, f func(doc map[string]any)) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var doc map[string]any
	if err := json.Unmarshal(a.files[name], &doc); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	f(doc)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	a.files[name] = b
}

// logBuffer collects log output; safe for concurrent writes.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLogs sends slog output to a buffer for the rest of the test.
func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	b := &logBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return b
}

func newRaw(t *testing.T) rawstore.Store {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("TEST_S3_ENDPOINT is not set; run the tests through `make test`")
	}
	st, err := rawstore.NewS3(endpoint, os.Getenv("TEST_S3_ACCESS_KEY"), os.Getenv("TEST_S3_SECRET_KEY"), os.Getenv("TEST_S3_BUCKET"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// fastConnector is the connector on srv without pauses between requests.
func fastConnector(t *testing.T, srv *httptest.Server) *senado {
	t.Helper()
	c := newSenado(srv.URL+"/dadosabertos", fixtureLegislatures)
	c.pace, c.backoff, c.maxBackoff = 0, time.Millisecond, 10*time.Millisecond
	return c
}

// env is a migrated schema with the sources, a raw store, the fixture API
// and the connector reading from it.
type env struct {
	pool *pgxpool.Pool
	raw  rawstore.Store
	api  *api
	c    *senado
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	a, srv := newAPI(t)
	return env{pool: pool, raw: newRaw(t), api: a, c: fastConnector(t, srv)}
}

// collect fetches from the fixture API under ref and loads, as connector.Run does.
func (e env) collect(t *testing.T, ref time.Time) (collection.Collection, connector.Stats) {
	t.Helper()
	files, err := e.c.Fetch(ctx, ref, e.raw)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	col, err := collection.Begin(ctx, e.pool, sourceID, ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := e.c.Load(ctx, col, files, e.raw, e.pool)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := collection.Finish(ctx, e.pool, col.ID, stats); err != nil {
		t.Fatal(err)
	}
	return col, stats
}

func (e env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (e env) strings(t *testing.T, query string, args ...any) []string {
	t.Helper()
	rows, err := e.pool.Query(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// mandates lists the holds_mandate claims of a senator as
// "role from..to closed-by" lines, in valid_from order.
func (e env) mandates(t *testing.T, id string) []string {
	t.Helper()
	return e.strings(t, `
		SELECT (r.attrs ->> 'role') || ' ' || r.valid_from || '..' || coalesce(r.valid_to_before::text, '') ||
			CASE WHEN r.closed_collection_id IS NULL THEN ' open' ELSE ' closed' END
		FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE p.source_id = $1 AND p.context_key = $2 AND r.rel_type = 'holds_mandate'
		ORDER BY r.valid_from, r.attrs ->> 'legislature', r.id`, sourceID, "sen:"+id)
}
