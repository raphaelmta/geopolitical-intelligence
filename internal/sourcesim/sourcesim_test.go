package sourcesim_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/connector/camara"
	"geoint/internal/connector/cvm"
	"geoint/internal/connector/receita"
	"geoint/internal/connector/senado"
	"geoint/internal/connector/tse"
	"geoint/internal/rawstore"
	"geoint/internal/sourcesim"
	"geoint/internal/testutil"
)

// The simulator's clock and the requests' date: the Receita month folder
// is 2026-11 and the CVM years are 2025 and 2026, the fixture years.
var today = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	sim, err := sourcesim.New("../../testdata", func() time.Time { return today })
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(sim)
	t.Cleanup(srv.Close)
	return srv
}

// memStore is an in-memory rawstore.Store.
type memStore struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (m *memStore) Put(_ context.Context, key string, r io.Reader) (rawstore.File, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return rawstore.File{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.files[key]; ok && !bytes.Equal(old, b) {
		return rawstore.File{}, rawstore.ErrConflict
	}
	m.files[key] = b
	return describe(key, b), nil
}

func (m *memStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[key]
	if !ok {
		return nil, errors.New("no such key " + key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStore) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[key]
	return ok, nil
}

func (m *memStore) Stat(_ context.Context, key string) (rawstore.File, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[key]
	if !ok {
		return rawstore.File{}, false, nil
	}
	return describe(key, b), true, nil
}

func describe(key string, b []byte) rawstore.File {
	sum := sha256.Sum256(b)
	return rawstore.File{Key: key, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))}
}

// collect runs a full collection of c against the simulator exactly as the
// worker does (connector.Collect: edition resolution, Fetch, Load, Finish)
// and checks it ended done with what the fixture holds.
func collect(t *testing.T, c connector.Connector, wantRead int64) {
	t.Helper()
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	raw := &memStore{files: map[string][]byte{}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	col, err := connector.Collect(ctx, c, connector.Request{Source: c.Key(), ReferenceDate: today,
		Scope: collection.Scope{Full: true}}, pool, raw, connector.Hooks{})
	if err != nil {
		t.Fatalf("collect %s: %v", c.Key(), err)
	}
	var status string
	var read int64
	if err := pool.QueryRow(ctx, `SELECT status, coalesce((stats ->> 'rows_read')::bigint, (stats ->> 'events_read')::bigint, -1)
		FROM collection WHERE id = $1`, col.ID).Scan(&status, &read); err != nil {
		t.Fatal(err)
	}
	if status != collection.StatusDone {
		t.Fatalf("%s collection %d is %s", c.Key(), col.ID, status)
	}
	if wantRead >= 0 && read != wantRead {
		t.Errorf("%s read %d records, want %d (the fixture's README)", c.Key(), read, wantRead)
	}
	if len(raw.files) == 0 {
		t.Errorf("%s stored no raw file", c.Key())
	}
	// Every recorded file names the simulator URL it came from, never with
	// credentials (the Receita share token travels as Basic auth only).
	rows, err := pool.Query(ctx, `SELECT name, coalesce(source_url, '') FROM collection_file WHERE collection_id = $1`, col.ID)
	if err != nil {
		t.Fatal(err)
	}
	urls, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Name, URL string }])
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) == 0 {
		t.Errorf("%s recorded no file", c.Key())
	}
	for _, u := range urls {
		if !strings.HasPrefix(u.URL, "http://127.0.0.1:") || strings.Contains(u.URL, "@") || strings.Contains(u.URL, "token") {
			t.Errorf("%s: %s recorded with source URL %q", c.Key(), u.Name, u.URL)
		}
	}
}

func TestReceitaEditionAndFetch(t *testing.T) {
	// One part per kind and files dated now: what compose.e2e.yml sets.
	t.Setenv("RECEITA_MIN_PARTS", "1")
	t.Setenv("RECEITA_SETTLE_HOURS", "0")
	srv := newServer(t)
	base := strings.Replace(srv.URL, "http://", "http://e2e-share-token@", 1) + "/receita/"
	// testdata/receita/README.md: a full load reads 104 records.
	collect(t, receita.New(base), 104)
}

func TestReceitaNeedsTheShareToken(t *testing.T) {
	srv := newServer(t)
	req, _ := http.NewRequest("PROPFIND", srv.URL+"/receita/", nil)
	req.Header.Set("Depth", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("PROPFIND without the share user: %d, want 401", resp.StatusCode)
	}
}

func TestTSE(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	// Default years (2018 to 2026): the years without fixtures are empty.
	// testdata/tse/README.md: 36 records (12 candidacies + 24 receipts) for
	// the 2022 AC files; the BR, 2024 and 2026 files add theirs.
	collect(t, tse.New(srv.URL+"/tse/", nil), -1)
}

func TestCamara(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	// Default legislatures (55 to 57); the 55th is empty.
	// testdata/camara/README.md: 14 list items.
	collect(t, camara.New(srv.URL+"/camara/api/v2", nil), -1)
}

func TestSenado(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	collect(t, senado.New(srv.URL+"/senado/dadosabertos", nil), -1)
}

func TestCVM(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	collect(t, cvm.New(srv.URL+"/cvm/"), -1)
}

func TestRangeAndHead(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	head, err := http.Head(srv.URL + "/cvm/fre_cia_aberta_2026.zip")
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.StatusCode != http.StatusOK || head.ContentLength <= 0 || head.Header.Get("ETag") == "" {
		t.Fatalf("HEAD: %d length=%d etag=%q", head.StatusCode, head.ContentLength, head.Header.Get("ETag"))
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/cvm/fre_cia_aberta_2026.zip", nil)
	req.Header.Set("Range", "bytes=10-")
	req.Header.Set("If-Range", head.Header.Get("ETag"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || int64(len(b)) != head.ContentLength-10 {
		t.Fatalf("Range: %d, %d bytes", resp.StatusCode, len(b))
	}
	if missing, _ := http.Head(srv.URL + "/cvm/fre_cia_aberta_1999.zip"); missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown year: %d, want 404", missing.StatusCode)
	}
}
