package receita

import (
	"archive/zip"
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"geoint/internal/collection"
)

func TestOlderEditionAfterNewerIsRefused(t *testing.T) {
	e := newEnv(t)
	e.load(t, refNov, fixtureZips(t, variant), collection.Scope{Full: true})
	before := e.snapshot(t)
	beforeSeen := e.strings(t, `SELECT id || ':' || coalesce(last_seen_collection_id, 0) || ':' || coalesce(closed_collection_id, 0)
		FROM relationship UNION ALL SELECT 'v' || id || ':' || coalesce(last_seen_collection_id, 0) || ':' || coalesce(closed_collection_id, 0)
		FROM company_version ORDER BY 1`)

	_, _, err := e.loadErr(t, refOct, fixtureZips(t, nil), collection.Scope{Full: true})
	if !errors.Is(err, ErrOlderEdition) {
		t.Fatalf("err = %v, want ErrOlderEdition", err)
	}
	if after := e.snapshot(t); !equalSnapshots(t, before, after) {
		t.Error("claims changed")
	}
	afterSeen := e.strings(t, `SELECT id || ':' || coalesce(last_seen_collection_id, 0) || ':' || coalesce(closed_collection_id, 0)
		FROM relationship UNION ALL SELECT 'v' || id || ':' || coalesce(last_seen_collection_id, 0) || ':' || coalesce(closed_collection_id, 0)
		FROM company_version ORDER BY 1`)
	if !slices.Equal(beforeSeen, afterSeen) {
		t.Error("last_seen/closed changed on relationship or company_version")
	}
	// A later day of the same month is still allowed; an earlier day than the
	// latest done collection is not, even within the same month.
	e.load(t, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), fixtureZips(t, variant), collection.Scope{Full: true})
	_, _, err = e.loadErr(t, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), fixtureZips(t, variant), collection.Scope{Full: true})
	if !errors.Is(err, ErrOlderEdition) {
		t.Fatalf("earlier day of the same month: err = %v, want ErrOlderEdition", err)
	}
}

func equalSnapshots(t *testing.T, want, got map[string][]string) bool {
	t.Helper()
	ok := true
	for k, w := range want {
		if g := got[k]; !slices.Equal(w, g) {
			ok = false
			t.Errorf("%s differs: %d rows want, %d got", k, len(w), len(g))
			for _, row := range w {
				if !slices.Contains(g, row) {
					t.Logf("  missing: %s", row)
				}
			}
			for _, row := range g {
				if !slices.Contains(w, row) {
					t.Logf("  extra:   %s", row)
				}
			}
		}
	}
	return ok
}

// failAt makes the load stop after the named checkpoint, as a crash would.
func failAt(t *testing.T, point string) {
	t.Helper()
	checkpointHook = func(p string) error {
		if p == point {
			return errors.New("injected failure at " + p)
		}
		return nil
	}
	t.Cleanup(func() { checkpointHook = nil })
}

// After a failed collection the runner records the failure; the retry is a
// new collection. It must end with exactly the claims of a clean load.
func testRecoveryAfter(t *testing.T, point string) {
	clean := newEnv(t)
	clean.loadFixture(t)
	want := clean.snapshot(t)

	e := newEnv(t)
	failAt(t, point)
	if _, _, err := e.loadErr(t, refOct, fixtureZips(t, nil), collection.Scope{Full: true}); err == nil ||
		!strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if n := e.count(t, `SELECT (SELECT count(*) FROM receita_stg_socio) + (SELECT count(*) FROM receita_stg_partner)
		+ (SELECT count(*) FROM receita_stg_person)`); n != 0 {
		t.Errorf("%d staging rows left after a failed load", n)
	}
	checkpointHook = nil
	col2, _ := e.loadFixture(t)
	equalSnapshots(t, want, e.snapshot(t))
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NULL AND last_seen_collection_id <> $1`, col2.ID); n != 0 {
		t.Errorf("%d relationships not seen by the retry", n)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id = r.src_record_id WHERE r.src_entity_id <> p.entity_id`); n != 0 {
		t.Errorf("%d relationships disagree with their record's entity", n)
	}
}

func TestRetryAfterFailureBetweenRelationshipChunks(t *testing.T) {
	testRecoveryAfter(t, "relationship chunk 3")
}

func TestRetryAfterFailureAfterCompanies(t *testing.T) {
	testRecoveryAfter(t, "companies")
}

func TestRelationshipsAreWrittenInSeveralChunks(t *testing.T) {
	e := newEnv(t)
	var chunks int
	checkpointHook = func(p string) error {
		if strings.HasPrefix(p, "relationship chunk ") {
			chunks++
		}
		return nil
	}
	t.Cleanup(func() { checkpointHook = nil })
	e.loadFixture(t)
	if chunks < 10 {
		t.Errorf("relationship chunks = %d, want one per company bucket (the fixture spreads over 10+)", chunks)
	}
}

func rawZip(t *testing.T, entries ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range entries {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(fixtureFile(t, "Empresas.csv"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestZipMustHoldExactlyOneFile(t *testing.T) {
	for i, entries := range [][]string{nil, {"Empresas.csv", "Other.csv"}} {
		e := newEnv(t)
		zips := fixtureZips(t, nil)
		zips["Empresas0.zip"] = rawZip(t, entries...)
		ref := time.Date(2030, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC) // own keys: different bytes
		_, _, err := e.loadErr(t, ref, zips, collection.Scope{Full: true})
		if err == nil || !strings.Contains(err.Error(), "Empresas0.zip") || !strings.Contains(err.Error(), "want exactly 1") {
			t.Errorf("%d entries: err = %v", len(entries), err)
		}
		if n := e.count(t, `SELECT count(*) FROM entity`); n != 0 {
			t.Errorf("%d entries: %d entities written", len(entries), n)
		}
	}
}

// A newer collection that failed after it started writing (it recorded raw
// files) blocks an older edition too: its partial data is newer. A failed
// newer attempt that wrote nothing (no file recorded) does not.
func TestOlderEditionAfterAFailedNewerLoadIsRefused(t *testing.T) {
	e := newEnv(t)
	wrote, err := collection.Begin(ctx, e.pool, sourceID, refNov, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.RecordSourceFile(ctx, e.pool, wrote.ID, collection.File{Name: "K3241.EMPRECSV", RawKey: rawPrefix(refNov) + "Empresas0.zip",
		SHA256: strings.Repeat("a", 64), Size: 1, SourceURL: "https://example.org/2026-11/Empresas0.zip"}); err != nil {
		t.Fatal(err)
	}
	if err := collection.Fail(ctx, e.pool, wrote.ID, errors.New("killed mid-load")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.loadErr(t, refOct, fixtureZips(t, nil), collection.Scope{Full: true}); !errors.Is(err, ErrOlderEdition) {
		t.Fatalf("after a failed newer load that wrote: err = %v, want ErrOlderEdition", err)
	}

	f := newEnv(t)
	empty, err := collection.Begin(ctx, f.pool, sourceID, refNov, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Fail(ctx, f.pool, empty.ID, errors.New("listing: 503")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.loadErr(t, refOct, fixtureZips(t, nil), collection.Scope{Full: true}); err != nil {
		t.Fatalf("after a failed newer attempt that wrote nothing: %v", err)
	}
}
