package cvm

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

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/rawstore"
	"geoint/internal/sources"
	"geoint/internal/testutil"
)

var ctx = context.Background()

var (
	refA = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	refB = time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	refC = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
)

// fixtureYears are the years of testdata/cvm; a reference date in 2026
// loads both (the current year and the previous one).
var fixtureYears = []int{2025, 2026}

// Natural keys of the fixture (testdata/cvm/README.md).
const (
	baseAlfa    = "98990001" // listed, declared controllers
	baseBeta    = "98991001" // listed, no controller
	baseGama    = "98991002" // listed, only a 2025 form; majority holder not flagged
	baseHolding = "98991101" // controller of Alfa, 51.37 %
	baseDelta   = "98991102" // shareholder of Beta, 30 %
	baseOmega   = "98991103" // shareholder of Gama, 75 %, and of Beta, 10 %
	baseControl = "98991108" // indirect: holds 70 % of Alfa Holding
	baseFounder = "98991109" // indirect: holds 90 % of Omega (Beta's description)

	keyFictitious = "cvm:98990001:FICTITIOUS CAPITAL LLC"
	keyInventada  = "cvm:98991001:INVENTADA HOLDING S L"
	keyGlobalZero = "cvm:98991102:GLOBAL ZERO FUND LP"
)

// Totals of the fixture.
const (
	fixtureRows          = 44 // data lines of both files
	fixtureMalformed     = 5
	fixtureShareholderOf = 17 // 11 direct, 6 indirect
	fixtureControls      = 5  // 2 direct, 3 indirect
	fixtureRels          = fixtureShareholderOf + fixtureControls
	// Ana in Alfa and in Gama, Bruno and Carla in Beta; indirect: Eduardo
	// (Alfa Holding), Fernanda (Alfa Controle), Helio (Delta).
	fixtureRecords = 7
)

// fixtureFile reads testdata/cvm/<name> (Latin-1 bytes, unchanged).
func fixtureFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "cvm", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
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

// fixtureZips maps fre_cia_aberta_<year>.zip to its content, laid out as the
// CVM publishes it: the shareholding entry next to entries the loader must
// ignore. edit, when set, may change the shareholding entry of each year.
func fixtureZips(t *testing.T, edit func(year int, b []byte) []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, y := range fixtureYears {
		b := fixtureFile(t, entryName(y))
		if edit != nil {
			b = edit(y, b)
		}
		out[fmt.Sprintf("fre_cia_aberta_%d.zip", y)] = zipOf(t, map[string][]byte{
			entryName(y):                            b,
			fmt.Sprintf("fre_cia_aberta_%d.csv", y): []byte("CNPJ_CIA;DT_REFER;VERSAO;DENOM_CIA;CD_CVM;CATEG_DOC;ID_DOC;DT_RECEB;LINK_DOC\r\n"),
			fmt.Sprintf("fre_cia_aberta_posicao_acionaria_classe_acao_%d.csv", y):        []byte("not;loaded\r\n"),
			fmt.Sprintf("fre_cia_aberta_distribuicao_capital_%d.csv", y):                 []byte("not;loaded\r\n"),
			fmt.Sprintf("fre_cia_aberta_administrador_membro_conselho_fiscal_%d.csv", y): []byte("not;loaded\r\n"),
		})
	}
	return out
}

// replaceLine returns b with the given text replaced on the lines that
// contain marker (Latin-1 safe: all arguments are ASCII).
func replaceLine(b []byte, marker, old, new string) []byte {
	lines := bytes.Split(b, []byte("\r\n"))
	for i, l := range lines {
		if bytes.Contains(l, []byte(marker)) {
			lines[i] = bytes.Replace(l, []byte(old), []byte(new), 1)
		}
	}
	return bytes.Join(lines, []byte("\r\n"))
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
	c    *cvm
	// prefix makes the raw keys of this test its own (tests share the bucket).
	prefix string
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	return env{pool: pool, raw: newRaw(t), c: newCVM("http://127.0.0.1:1/unused/"),
		prefix: "raw/test-cvm/" + strings.ReplaceAll(t.Name(), "/", "_") + fmt.Sprintf("-%d/", time.Now().UnixNano())}
}

// store puts the zips into the raw store under a prefix of this test and ref.
func (e env) store(t *testing.T, ref time.Time, zips map[string][]byte) []rawstore.File {
	t.Helper()
	var files []rawstore.File
	for name, b := range zips {
		f, err := e.raw.Put(ctx, e.prefix+ref.Format(time.DateOnly)+"/"+name, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// load runs one full collection over zips and finishes it.
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
	// Observed on its reference date (the day a CVM run starts), so the
	// closing bound of these tests is ref + 1 day.
	col, err := collection.BeginObserved(ctx, e.pool, sourceID, ref, ref, scope)
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

// loadAndClose runs a full collection as connector.Run does for a snapshot
// source: Load, then CloseUnseen with the production guard, then Finish.
// It returns the number of rows CloseUnseen closed.
func (e env) loadAndClose(t *testing.T, ref time.Time, zips map[string][]byte) (collection.Collection, connector.Stats, int64) {
	t.Helper()
	files := e.store(t, ref, zips)
	col, err := collection.BeginObserved(ctx, e.pool, sourceID, ref, ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := e.c.Load(ctx, col, files, e.raw, e.pool)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	src, ok := sources.ByKey(Key)
	if !ok {
		t.Fatal("source cvm is not catalogued")
	}
	closed, err := claims.NewStore(e.pool).CloseUnseen(ctx, src, col, 0.9)
	if err != nil {
		t.Fatalf("CloseUnseen: %v", err)
	}
	if err := collection.Finish(ctx, e.pool, col.ID, stats); err != nil {
		t.Fatal(err)
	}
	return col, stats, closed
}

func (e env) loadFixture(t *testing.T) (collection.Collection, connector.Stats) {
	t.Helper()
	return e.load(t, refA, fixtureZips(t, nil))
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

// company returns the id of the company entity with that CNPJ base.
func (e env) company(t *testing.T, base string) int64 {
	t.Helper()
	return e.id(t, `SELECT id FROM entity WHERE kind = 'company' AND natural_key = $1`, base)
}

func (e env) foreign(t *testing.T, key string) int64 {
	t.Helper()
	return e.id(t, `SELECT id FROM entity WHERE kind = 'foreign' AND natural_key = $1`, key)
}

// record returns the id of the cvm person record with that context and name.
func (e env) record(t *testing.T, base, name string) int64 {
	t.Helper()
	return e.id(t, `SELECT id FROM person_record WHERE source_id = $1 AND context_key = $2 AND name = $3`,
		sourceID, "cvm:"+base, name)
}

// openAttr returns attrs ->> key of the single open relationship of that
// type from src (an entity id) to the company with base dst.
func (e env) openAttr(t *testing.T, typ string, src int64, dstBase, key string) string {
	t.Helper()
	got := e.strings(t, `SELECT coalesce(r.attrs ->> '`+key+`', '<null>') FROM relationship r JOIN entity d ON d.id = r.dst_entity_id
		WHERE r.rel_type = $1 AND r.src_entity_id = $2 AND d.kind = 'company' AND d.natural_key = $3 AND r.closed_collection_id IS NULL`,
		typ, src, dstBase)
	if len(got) != 1 {
		t.Fatalf("%d open %s relationships from %d to %s, want 1", len(got), typ, src, dstBase)
	}
	return got[0]
}

func joined(s []string) string { return strings.Join(s, ",") }
