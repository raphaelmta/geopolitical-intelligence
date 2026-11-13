package collection_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"geoint/internal/collection"
	"geoint/internal/testutil"
)

var ctx = context.Background()

func ref(day int) time.Time { return time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC) }

func TestBeginFinishRoundTrip(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	scope := collection.Scope{CNPJs: []string{"00000001"}, Depth: 2}
	c, err := collection.Begin(ctx, pool, 1, ref(1), scope)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == 0 || c.Status != "running" || c.SourceID != 1 || !c.ReferenceDate.Equal(ref(1)) {
		t.Fatalf("unexpected %+v", c)
	}
	if err := collection.Finish(ctx, pool, c.ID, map[string]int64{"rows": 42}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := collection.LastDone(ctx, pool, 1)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.ID != c.ID || got.Status != "done" || got.Scope.Depth != 2 || len(got.Scope.CNPJs) != 1 || got.Scope.CNPJs[0] != "00000001" {
		t.Fatalf("unexpected %+v", got)
	}
	var rows int64
	var finished *time.Time
	if err := pool.QueryRow(ctx, "SELECT (stats->>'rows')::bigint, finished_at FROM collection WHERE id=$1", c.ID).Scan(&rows, &finished); err != nil {
		t.Fatal(err)
	}
	if rows != 42 || finished == nil {
		t.Fatalf("stats rows=%d finished=%v", rows, finished)
	}
	// A finished collection cannot be finished or failed again.
	if err := collection.Finish(ctx, pool, c.ID, nil); err == nil {
		t.Error("second Finish accepted")
	}
	if err := collection.Fail(ctx, pool, c.ID, errors.New("x")); err == nil {
		t.Error("Fail after Finish accepted")
	}
}

func TestFailRecordsCause(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	c, err := collection.Begin(ctx, pool, 1, ref(1), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Fail(ctx, pool, c.ID, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	var status, msg string
	if err := pool.QueryRow(ctx, "SELECT status, error FROM collection WHERE id=$1", c.ID).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || msg != "boom" {
		t.Fatalf("status=%q error=%q", status, msg)
	}
}

func TestLastDoneIgnoresRunningAndFailed(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	if _, ok, err := collection.LastDone(ctx, pool, 1); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	done, _ := collection.Begin(ctx, pool, 1, ref(1), collection.Scope{Full: true})
	if err := collection.Finish(ctx, pool, done.ID, nil); err != nil {
		t.Fatal(err)
	}
	failed, _ := collection.Begin(ctx, pool, 1, ref(2), collection.Scope{Full: true})
	if err := collection.Fail(ctx, pool, failed.ID, errors.New("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := collection.Begin(ctx, pool, 1, ref(3), collection.Scope{Full: true}); err != nil { // stays running
		t.Fatal(err)
	}
	other, _ := collection.Begin(ctx, pool, 2, ref(4), collection.Scope{Full: true})
	if err := collection.Finish(ctx, pool, other.ID, nil); err != nil {
		t.Fatal(err)
	}

	got, ok, err := collection.LastDone(ctx, pool, 1)
	if err != nil || !ok || got.ID != done.ID {
		t.Fatalf("got %+v ok=%v err=%v, want id %d", got, ok, err, done.ID)
	}
	// The newest done one wins.
	newer, _ := collection.Begin(ctx, pool, 1, ref(5), collection.Scope{Full: true})
	if err := collection.Finish(ctx, pool, newer.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _, _ = collection.LastDone(ctx, pool, 1)
	if got.ID != newer.ID {
		t.Fatalf("got %d, want %d", got.ID, newer.ID)
	}
}

func TestRecordSourceFile(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	c, err := collection.Begin(ctx, pool, 1, ref(1), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("ab", 32)
	if err := collection.RecordSourceFile(ctx, pool, c.ID, collection.File{Name: "Socios.csv", RawKey: "raw/receita/2026-10-01/Socios0.zip", SHA256: sha, Size: 42, SourceURL: "https://example.org/files/Socios.csv"}); err != nil {
		t.Fatal(err)
	}
	// Identical again: no-op. Same name, another checksum: conflict.
	if err := collection.RecordSourceFile(ctx, pool, c.ID, collection.File{Name: "Socios.csv", RawKey: "raw/receita/2026-10-01/Socios0.zip", SHA256: sha, Size: 42, SourceURL: "https://example.org/files/Socios.csv"}); err != nil {
		t.Fatalf("identical re-insert: %v", err)
	}
	if err := collection.RecordSourceFile(ctx, pool, c.ID, collection.File{Name: "Socios.csv", RawKey: "raw/x", SHA256: strings.Repeat("cd", 32), Size: 1, SourceURL: "https://example.org/files/Socios.csv"}); !errors.Is(err, collection.ErrFileConflict) {
		t.Fatalf("different checksum: %v, want ErrFileConflict", err)
	}
	for _, bad := range []string{"", "ABCD", strings.Repeat("AB", 32)} {
		if err := collection.RecordSourceFile(ctx, pool, c.ID, collection.File{Name: "Other.csv", RawKey: "raw/y", SHA256: bad, Size: 1, SourceURL: "https://example.org/files/Other.csv"}); err == nil {
			t.Fatalf("checksum %q accepted", bad)
		}
	}
	for _, stmt := range []string{
		`UPDATE collection_file SET size_bytes = 1`,
		`DELETE FROM collection_file`,
		`TRUNCATE collection_file`,
	} {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Fatalf("%s: allowed", stmt)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM collection_file WHERE sha256 = $1 AND size_bytes = 42`, sha).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows %d err %v", n, err)
	}
}

func TestSafeURLNeverKeepsCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"https://gn672Ad4CF8N6TK@arquivos.receitafederal.gov.br/public.php/webdav/Dados/2026-10/Socios0.zip": "https://arquivos.receitafederal.gov.br/public.php/webdav/Dados/2026-10/Socios0.zip",
		"https://user:secret@example.org/a.zip#frag":                                                         "https://example.org/a.zip",
		"https://dadosabertos.camara.leg.br/api/v2/deputados?idLegislatura=57&itens=100&pagina=2":            "https://dadosabertos.camara.leg.br/api/v2/deputados?idLegislatura=57&itens=100&pagina=2",
		"https://example.org/f?token=abc&ano=2026&X-Amz-Signature=zz&api_key=k":                              "https://example.org/f?ano=2026",
		"https://example.org/f?Password=x":                                                                   "https://example.org/f",
	} {
		got, err := collection.SafeURL(in)
		if err != nil || got != want {
			t.Errorf("SafeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "raw/receita/x.zip", "ftp://example.org/a", "file:///etc/passwd", "https://"} {
		if _, err := collection.SafeURL(bad); !errors.Is(err, collection.ErrSourceURL) {
			t.Errorf("SafeURL(%q): err = %v, want ErrSourceURL", bad, err)
		}
	}
}

func TestRecordSourceFileStoresASafeURL(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	c, err := collection.Begin(ctx, pool, 1, ref(1), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	f := collection.File{Name: "Socios.csv", RawKey: "raw/receita/2026-10-01/Socios0.zip", SHA256: strings.Repeat("ab", 32), Size: 42,
		SourceURL: "https://share-token@arquivos.example.org/webdav/2026-10/Socios0.zip"}
	if err := collection.RecordSourceFile(ctx, pool, c.ID, f); err != nil {
		t.Fatal(err)
	}
	var u string
	if err := pool.QueryRow(ctx, `SELECT source_url FROM collection_file WHERE collection_id = $1`, c.ID).Scan(&u); err != nil {
		t.Fatal(err)
	}
	if u != "https://arquivos.example.org/webdav/2026-10/Socios0.zip" {
		t.Fatalf("stored %q", u)
	}
	f.Name, f.SourceURL = "Other.csv", ""
	if err := collection.RecordSourceFile(ctx, pool, c.ID, f); !errors.Is(err, collection.ErrSourceURL) {
		t.Fatalf("empty URL: err = %v, want ErrSourceURL", err)
	}
	// The database refuses credentials too, whoever writes.
	for _, bad := range []string{"https://user:pw@example.org/a", "https://example.org/a#x", "", "raw/key"} {
		if _, err := pool.Exec(ctx, `INSERT INTO collection_file (collection_id, name, raw_key, sha256, size_bytes, source_url)
			VALUES ($1, 'X.csv', 'raw/x', $2, 1, $3)`, c.ID, strings.Repeat("ab", 32), bad); err == nil {
			t.Errorf("source_url %q accepted by the database", bad)
		}
	}
}

func TestObservedDate(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	pub := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	c, err := collection.BeginObserved(ctx, pool, 1, ref(1), pub, collection.Scope{Full: true})
	if err != nil || !c.ObservedDate.Equal(pub) {
		t.Fatalf("observed %v, %v; want %s", c.ObservedDate, err, pub)
	}
	// Never before the reference date.
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	c2, err := collection.Begin(ctx, pool, 1, future, collection.Scope{Full: true})
	if err != nil || !c2.ObservedDate.Equal(future) {
		t.Fatalf("future reference: observed %v, %v", c2.ObservedDate, err)
	}
	got, err := collection.Get(ctx, pool, c.ID)
	if err != nil || !got.ObservedDate.Equal(pub) || !got.Scope.Full {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO collection (source_id, reference_date, scope, observed_date) VALUES (1, '2026-10-01', '{}', '2026-09-30')`); err == nil {
		t.Error("an observed date before the reference date was accepted")
	}
}

// Guards (migration 0019): only finishing a run changes a collection, a
// finished one keeps its status, entities never change, requests are history.
func TestGuards(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	c, err := collection.Begin(ctx, pool, 1, ref(1), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE collection SET reference_date = '2026-09-01'`,
		`UPDATE collection SET observed_date = '2026-12-01'`,
		`UPDATE collection SET scope = '{"full": false}'`,
		`UPDATE collection SET started_at = now() - interval '1 day'`,
		`UPDATE collection SET source_id = 2`,
	} {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%s: allowed", stmt)
		}
	}
	if err := collection.Finish(ctx, pool, c.ID, map[string]int64{"rows_read": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE collection SET status = 'failed' WHERE id = $1`, c.ID); err == nil {
		t.Error("a done collection became failed")
	}
	if _, err := pool.Exec(ctx, `UPDATE collection SET finished_at = now() WHERE id = $1`, c.ID); err != nil {
		t.Errorf("finished_at refused: %v", err)
	}
	var entity int64
	if err := pool.QueryRow(ctx, `INSERT INTO entity (kind, natural_key, name, name_norm) VALUES ('company', '12345678', 'A', 'A') RETURNING id`).Scan(&entity); err != nil {
		t.Fatal(err)
	}
	// Row triggers fire only on existing rows.
	if _, err := pool.Exec(ctx, `INSERT INTO collection_request (source_id, requested_by, full_scope) VALUES (1, 'cli', true)`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE entity SET name = 'B'`, `UPDATE entity SET name_norm = 'B'`, `UPDATE entity SET kind = 'person'`,
		`UPDATE entity SET natural_key = '87654321'`,
		`DELETE FROM collection_request`, `TRUNCATE collection_request`, `UPDATE collection_request SET requested_by = 'x'`,
	} {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%s: allowed", stmt)
		}
	}
	// A no-op update (EnsureEntity's ON CONFLICT DO UPDATE) still passes.
	if _, err := pool.Exec(ctx, `UPDATE entity SET natural_key = natural_key WHERE id = $1`, entity); err != nil {
		t.Errorf("no-op entity update refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE collection_request SET published_at = now()`); err != nil {
		t.Errorf("published_at refused: %v", err)
	}
}

func TestStartHoldsTheRunLockUntilRelease(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	run, err := collection.Start(ctx, pool, 1, ref(1), time.Time{}, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if failed, err := collection.FailDead(ctx, pool); err != nil || len(failed) != 0 {
		t.Fatalf("FailDead with the lock held: %v, %v", failed, err)
	}
	if err := collection.Finish(ctx, pool, run.ID, nil); err != nil {
		t.Fatal(err)
	}
	run.Release()
	run.Release() // idempotent
	if failed, err := collection.FailDead(ctx, pool); err != nil || len(failed) != 0 {
		t.Fatalf("FailDead after a finished run: %v, %v", failed, err)
	}
	// An unlocked running row (Begin) is dead.
	dead, err := collection.Begin(ctx, pool, 1, ref(2), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := collection.FailDead(ctx, pool)
	if err != nil || len(failed) != 1 || failed[0] != dead.ID {
		t.Fatalf("FailDead = %v, %v; want [%d]", failed, err, dead.ID)
	}
	var msg string
	if err := pool.QueryRow(ctx, `SELECT error FROM collection WHERE id = $1`, dead.ID).Scan(&msg); err != nil || msg != collection.InterruptedError {
		t.Fatalf("error %q, %v", msg, err)
	}
}

// LiveRunSQL sees the run lock of another session through pg_locks, without
// taking it: true while Start's lock is held, false once released, false
// for a row never locked (Begin) and for ids beyond the lock key range.
func TestLiveRunSQL(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	live := func(id int64) bool {
		t.Helper()
		var v bool
		if err := pool.QueryRow(ctx, `SELECT `+collection.LiveRunSQL("$1::bigint"), id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	run, err := collection.Start(ctx, pool, 1, ref(1), time.Time{}, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if !live(run.ID) {
		t.Fatal("a held run lock is not seen as live")
	}
	run.Release()
	if live(run.ID) {
		t.Fatal("a released run lock is still seen as live")
	}
	dead, err := collection.Begin(ctx, pool, 1, ref(2), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if live(dead.ID) {
		t.Fatal("a row opened by Begin (no lock) is seen as live")
	}
	if live(1<<40) || live(0) || live(-1) {
		t.Fatal("an id outside the lock key range is seen as live")
	}
}
