package receita

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/rawstore"
	"geoint/internal/testutil"
)

var ctx = context.Background()

// The fixture's edition and a second one with a few changes (see variant).
var (
	refOct = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	refNov = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
)

// fixtureFile reads testdata/receita/<name> (Latin-1 bytes, unchanged).
func fixtureFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "receita", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// zipOf returns a zip with one entry, like the Receita files.
func zipOf(t *testing.T, entry string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fixtureZips maps the zip names of an edition to their content. edit, when
// set, may change each CSV before zipping.
func fixtureZips(t *testing.T, edit func(name string, b []byte) []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for zipName, csv := range map[string]string{
		"Empresas0.zip": "Empresas.csv", "Estabelecimentos0.zip": "Estabelecimentos.csv", "Socios0.zip": "Socios.csv",
	} {
		b := fixtureFile(t, csv)
		if edit != nil {
			b = edit(csv, b)
		}
		out[zipName] = zipOf(t, csv, b)
	}
	return out
}

// variant is the November edition: one company renamed and one partner row
// of the person in three companies with a new qualification.
func variant(name string, b []byte) []byte {
	switch name {
	case "Empresas.csv":
		return bytes.Replace(b, []byte(`"PADARIA SOL NASCENTE LTDA"`), []byte(`"PADARIA SOL NASCENTE COMERCIO LTDA"`), 1)
	case "Socios.csv":
		return bytes.Replace(b, []byte(`"***123456**";"49";"20100315"`), []byte(`"***123456**";"05";"20100315"`), 1)
	}
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

// env is a migrated schema with the sources, a raw store and the connector.
type env struct {
	pool *pgxpool.Pool
	raw  rawstore.Store
	c    *receita
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	return env{pool: pool, raw: newRaw(t), c: newReceita("http://127.0.0.1:1/unused/")}
}

// store puts the zips of an edition into the raw store.
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

// load runs one collection of the edition ref over zips and finishes it,
// as connector.Run does.
func (e env) load(t *testing.T, ref time.Time, zips map[string][]byte, scope collection.Scope) (collection.Collection, connector.Stats) {
	t.Helper()
	col, stats, err := e.loadErr(t, ref, zips, scope)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return col, stats
}

// loadErr runs one collection and records its outcome like connector.Run:
// done with the stats, or failed.
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

// snapshot describes every claim by natural identities (no ids, no
// collection ids), so two databases loaded differently can be compared.
func (e env) snapshot(t *testing.T) map[string][]string {
	t.Helper()
	end := func(rec, ent string) string {
		return `coalesce(` + rec + `.context_key || '/' || ` + rec + `.name_norm || '/' || ` + rec + `.cpf_fragment,
			` + ent + `.kind || ':' || coalesce(` + ent + `.natural_key, ''))`
	}
	return map[string][]string{
		"entity": e.strings(t, `SELECT kind || '|' || coalesce(natural_key, '') || '|' || name || '|' || name_norm FROM entity ORDER BY 1`),
		"person_record": e.strings(t, `SELECT p.context_key || '|' || p.name || '|' || p.name_norm || '|' || p.cpf_fragment || '|' ||
			p.record_ref || '|' || p.identity_confidence || '|' || e.name FROM person_record p JOIN entity e ON e.id = p.entity_id ORDER BY 1`),
		"company_version": e.strings(t, `SELECT e.natural_key || '|' || v.name || '|' || v.legal_nature || '|' || v.size || '|' ||
			v.uf || '|' || v.city || '|' || v.cnae || '|' || v.status || '|' || coalesce(v.capital::text, '') || '|' ||
			v.record_ref || '|' || (v.closed_collection_id IS NULL) FROM company_version v JOIN entity e ON e.id = v.entity_id ORDER BY 1`),
		"relationship": e.strings(t, `SELECT r.rel_type || '|' || `+end("sp", "se")+` || '|' || `+end("dp", "de")+` || '|' ||
			coalesce(r.attrs ->> 'role', '') || '|' || coalesce(r.valid_from::text, '') || '|' || r.record_ref || '|' ||
			(r.closed_collection_id IS NULL) || '|' || r.kind
			FROM relationship r
			JOIN entity se ON se.id = r.src_entity_id JOIN entity de ON de.id = r.dst_entity_id
			LEFT JOIN person_record sp ON sp.id = r.src_record_id LEFT JOIN person_record dp ON dp.id = r.dst_record_id
			ORDER BY 1`),
	}
}

func (e env) loadFixture(t *testing.T) (collection.Collection, connector.Stats) {
	t.Helper()
	return e.load(t, refOct, fixtureZips(t, nil), collection.Scope{Full: true})
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

func (e env) company(t *testing.T, base string) int64 {
	t.Helper()
	return e.id(t, `SELECT id FROM entity WHERE kind='company' AND natural_key=$1`, base)
}
