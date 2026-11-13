package claims_test

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"geoint/internal/collection"
	"geoint/internal/connector/receita"
	"geoint/internal/rawstore"
)

// receitaEdition zips the Receita test fixture (testdata/receita, synthetic
// data), with edit applied to each CSV, puts it in the raw store under a
// prefix of its own and returns the files.
func receitaEdition(t *testing.T, raw rawstore.Store, edit func(name string, b []byte) []byte) []rawstore.File {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	prefix := "raw/test-close-unseen/" + hex.EncodeToString(buf) + "/"
	var files []rawstore.File
	for _, name := range []string{"Empresas", "Estabelecimentos", "Socios"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "receita", name+".csv"))
		if err != nil {
			t.Fatal(err)
		}
		if edit != nil {
			b = edit(name+".csv", b)
		}
		var z bytes.Buffer
		zw := zip.NewWriter(&z)
		w, err := zw.Create(name + ".csv")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		f, err := raw.Put(ctx, prefix+name+"0.zip", &z)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// TestReceitaLoadsCloseOnlyWhatLeft runs the real Receita loader: a full
// edition, a slice that changes nothing, then a full edition without one
// partner row.
func TestReceitaLoadsCloseOnlyWhatLeft(t *testing.T) {
	f := newFixture(t)
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("TEST_S3_ENDPOINT is not set; run the tests through `make test`")
	}
	raw, err := rawstore.NewS3(endpoint, os.Getenv("TEST_S3_ACCESS_KEY"), os.Getenv("TEST_S3_SECRET_KEY"), os.Getenv("TEST_S3_BUCKET"))
	if err != nil {
		t.Fatal(err)
	}
	src := source(t, "receita")
	loader := receita.New("http://127.0.0.1:1/unused/")
	load := func(ref time.Time, scope collection.Scope, files []rawstore.File) (collection.Collection, int64) {
		t.Helper()
		col := f.begin(t, src, ref, scope)
		stats, err := loader.Load(ctx, col, files, raw, f.pool)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		closed, err := f.s.CloseUnseen(ctx, src, col, 0.9)
		if err != nil {
			t.Fatalf("CloseUnseen: %v", err)
		}
		if err := collection.Finish(ctx, f.pool, col.ID, stats); err != nil {
			t.Fatal(err)
		}
		return col, closed
	}

	load(refOct, full, receitaEdition(t, raw, nil))

	// A slice never closes anything, whatever the loader left out of it.
	if _, closed := load(refNov, collection.Scope{CNPJs: []string{"98990002"}, Depth: 1}, receitaEdition(t, raw, nil)); closed != 0 {
		t.Fatalf("slice closed %d claims, want 0", closed)
	}

	gone := []byte(`"98990008";"2";"GABRIELA MOURA";"***890123**";"49";"20190202";"";"***000000**";"";"00";"4"` + "\n")
	files := receitaEdition(t, raw, func(name string, b []byte) []byte {
		if name != "Socios.csv" {
			return b
		}
		out := bytes.Replace(b, gone, nil, 1)
		if bytes.Equal(out, b) {
			t.Fatal("fixture row to remove not found")
		}
		return out
	})
	c3, closed := load(refDec, full, files)

	// Her partner_of and administrator_of (qualification 49) claims.
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}
	rows := f.partnerRows(t, "GABRIELA MOURA")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		// Last seen in October: the November slice did not cover her company.
		// begin observes on the reference date: the bound is the day after.
		if r.closed == nil || *r.closed != c3.ID || !sameDay(r.validAfter, refOct) || !sameDay(r.validBy, dayAfter(refDec)) {
			t.Fatalf("closed=%v after=%v before=%v, want %d/%s/%s", r.closed, r.validAfter, r.validBy, c3.ID, refOct, dayAfter(refDec))
		}
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NOT NULL`); n != 2 {
		t.Fatalf("closed relationships = %d, want 2", n)
	}
	if n := f.count(t, `SELECT count(*) FROM company_version WHERE closed_collection_id IS NOT NULL`); n != 0 {
		t.Fatalf("closed company versions = %d, want 0", n)
	}
}
