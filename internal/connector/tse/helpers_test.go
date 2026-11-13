package tse

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/identity"
	"geoint/internal/rawstore"
	"geoint/internal/testutil"
)

var ctx = context.Background()

var (
	refA = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	refB = time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
)

// fixtureYears are the years of testdata/tse: 2022 (full CPFs), 2024 (no
// candidate CPF) and 2026 (full CPFs).
var fixtureYears = []int{2022, 2024, 2026}

// fixtureFile reads testdata/tse/<name> (Latin-1 bytes, unchanged).
func fixtureFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "tse", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtureEntries returns the testdata CSVs of one year and kind
// ("consulta_cand" or "receitas_candidatos"), keyed by entry name.
func fixtureEntries(t *testing.T, kind string, year int) map[string][]byte {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "tse", fmt.Sprintf("%s_%d_*.csv", kind, year)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, m := range matches {
		out[filepath.Base(m)] = fixtureFile(t, filepath.Base(m))
	}
	if len(out) == 0 {
		t.Fatalf("no fixture %s files for %d", kind, year)
	}
	return out
}

// brasil builds the _BRASIL aggregate the TSE publishes: the header once and
// every data line of the UF files.
func brasil(entries map[string][]byte) []byte {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for i, n := range names {
		b := entries[n]
		if i > 0 {
			_, b, _ = bytes.Cut(b, []byte("\r\n"))
		}
		buf.Write(b)
	}
	return buf.Bytes()
}

// zipOf builds a zip with the given entries, in name order.
func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entries[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fixtureZips maps the zip names of the fixture years to their content, as
// the TSE publishes them: the UF (and BR) CSVs, the _BRASIL aggregate and
// entries the loader must ignore. edit, when set, may change each CSV entry
// before zipping (the aggregate is built from the edited entries).
func fixtureZips(t *testing.T, edit func(entry string, b []byte) []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, y := range fixtureYears {
		for _, kind := range []string{"consulta_cand", "receitas_candidatos"} {
			entries := fixtureEntries(t, kind, y)
			if edit != nil {
				for n, b := range entries {
					entries[n] = edit(n, b)
				}
			}
			agg := brasil(entries)
			entries[fmt.Sprintf("%s_%d_BRASIL.csv", kind, y)] = agg
			if kind == "consulta_cand" {
				entries["leiame.pdf"] = []byte("%PDF-1.4 not a real pdf\n")
				out[fmt.Sprintf("consulta_cand_%d.zip", y)] = zipOf(t, entries)
				continue
			}
			ac := entries[fmt.Sprintf("receitas_candidatos_%d_AC.csv", y)]
			entries[fmt.Sprintf("receitas_candidatos_doador_originario_%d_AC.csv", y)] = ac
			entries[fmt.Sprintf("despesas_pagas_candidatos_%d_AC.csv", y)] = []byte("\"DT_GERACAO\"\r\n")
			entries["leiame_receitas-candidatos.pdf"] = []byte("%PDF-1.4 not a real pdf\n")
			out[fmt.Sprintf("prestacao_de_contas_eleitorais_candidatos_%d.zip", y)] = zipOf(t, entries)
		}
	}
	return out
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

// env is a migrated schema with the sources, a raw store and the connector.
type env struct {
	pool *pgxpool.Pool
	raw  rawstore.Store
	c    *tse
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	return env{pool: pool, raw: newRaw(t), c: newTSE("http://127.0.0.1:1/unused/", fixtureYears)}
}

// store puts the zips into the raw store under the prefix of ref.
func (e env) store(t *testing.T, ref time.Time, zips map[string][]byte) []rawstore.File {
	t.Helper()
	var files []rawstore.File
	for name, b := range zips {
		f, err := e.raw.Put(ctx, rawPrefix(ref)+name, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// load runs one collection over zips and finishes it, as connector.Run does.
func (e env) load(t *testing.T, ref time.Time, zips map[string][]byte) (collection.Collection, connector.Stats) {
	t.Helper()
	col, stats, err := e.loadErr(t, ref, zips, collection.Scope{Full: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return col, stats
}

func (e env) loadErr(t *testing.T, ref time.Time, zips map[string][]byte, scope collection.Scope) (collection.Collection, connector.Stats, error) {
	t.Helper()
	files := e.store(t, ref, zips)
	col, err := collection.Begin(ctx, e.pool, sourceID, ref, scope)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := e.c.Load(ctx, col, files, e.raw, e.pool)
	if err != nil {
		if ferr := collection.Fail(ctx, e.pool, col.ID, err); ferr != nil {
			t.Fatal(ferr)
		}
		return col, nil, err
	}
	if err := collection.Finish(ctx, e.pool, col.ID, stats); err != nil {
		t.Fatal(err)
	}
	return col, stats, nil
}

func (e env) begin(t *testing.T, ref time.Time) collection.Collection {
	t.Helper()
	col, err := collection.Begin(ctx, e.pool, sourceID, ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	return col
}

func (e env) loadFixture(t *testing.T) (collection.Collection, connector.Stats) {
	t.Helper()
	return e.load(t, refA, fixtureZips(t, nil))
}

func (e env) resolve(t *testing.T, colID int64) {
	t.Helper()
	if _, err := (&identity.Resolver{Pool: e.pool}).Resolve(ctx, colID); err != nil {
		t.Fatal(err)
	}
}

func (e env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (e env) id(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
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

// record returns the id of the person record with that context key.
func (e env) record(t *testing.T, contextKey string) int64 {
	t.Helper()
	return e.id(t, `SELECT id FROM person_record WHERE source_id = $1 AND context_key = $2`, sourceID, contextKey)
}

func (e env) entityOf(t *testing.T, contextKey string) int64 {
	t.Helper()
	return e.id(t, `SELECT entity_id FROM person_record WHERE source_id = $1 AND context_key = $2`, sourceID, contextKey)
}

func (e env) party(t *testing.T, sigla string) int64 {
	t.Helper()
	return e.id(t, `SELECT id FROM entity WHERE kind = 'party' AND natural_key = $1`, sigla)
}

// attr returns attrs ->> key of the single relationship matching where.
func (e env) attr(t *testing.T, key, where string, args ...any) string {
	t.Helper()
	got := e.strings(t, `SELECT coalesce(attrs ->> '`+key+`', '<null>') FROM relationship WHERE `+where, args...)
	if len(got) != 1 {
		t.Fatalf("%d relationships match %s %v, want 1", len(got), where, args)
	}
	return got[0]
}

func joined(s []string) string { return strings.Join(s, ",") }
