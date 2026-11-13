package connector_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/rawstore"
	"geoint/internal/sources"
	"geoint/internal/testutil"
)

var ctx = context.Background()

var ref = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

type fake struct {
	key       string
	fetch     func(context.Context) ([]rawstore.File, error)
	load      func(context.Context) (connector.Stats, error)
	fetched   int
	loaded    int
	gotFiles  []rawstore.File
	gotCol    collection.Collection
	gotRefDay time.Time
}

func (f *fake) Key() string { return f.key }

func (f *fake) Fetch(c context.Context, r time.Time, _ rawstore.Store) ([]rawstore.File, error) {
	f.fetched++
	f.gotRefDay = r
	if f.fetch != nil {
		return f.fetch(c)
	}
	return []rawstore.File{{Key: "raw/x/2026-10-01/a", SHA256: "h", Size: 1}}, nil
}

func (f *fake) Load(c context.Context, col collection.Collection, files []rawstore.File, _ rawstore.Store, _ *pgxpool.Pool) (connector.Stats, error) {
	f.loaded++
	f.gotCol, f.gotFiles = col, files
	if f.load != nil {
		return f.load(c)
	}
	return connector.Stats{"rows_read": 3, "rows_skipped": 0}, nil
}

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	return pool
}

type row struct {
	status string
	errMsg *string
	rows   *int64
}

func lastCollection(t *testing.T, pool *pgxpool.Pool) row {
	t.Helper()
	var r row
	err := pool.QueryRow(ctx, `SELECT status, error, (stats->>'rows_read')::bigint FROM collection ORDER BY id DESC LIMIT 1`).
		Scan(&r.status, &r.errMsg, &r.rows)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func count(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM collection`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func req(source string) connector.Request {
	return connector.Request{Source: source, ReferenceDate: ref, Scope: collection.Scope{Full: true}}
}

func TestRunMarksCollectionDoneAndStoresStats(t *testing.T) {
	pool := setup(t)
	f := &fake{key: "receita"}
	var closed, resolved int
	h := connector.Hooks{
		CloseUnseen: func(context.Context, sources.Source, collection.Collection) (int64, error) { closed++; return 0, nil },
		Resolve:     func(_ context.Context, id int64) error { resolved++; return nil },
	}
	if err := connector.Run(ctx, f, req("receita"), pool, nil, h); err != nil {
		t.Fatal(err)
	}
	r := lastCollection(t, pool)
	if r.status != "done" || r.rows == nil || *r.rows != 3 {
		t.Fatalf("unexpected %+v", r)
	}
	if closed != 1 || resolved != 1 {
		t.Errorf("closeUnseen=%d resolve=%d, want 1 and 1 (receita is a snapshot)", closed, resolved)
	}
	if len(f.gotFiles) != 1 || f.gotCol.Status != "running" || f.gotCol.ID == 0 || !f.gotRefDay.Equal(ref) {
		t.Errorf("Load/Fetch got files=%v col=%+v ref=%v", f.gotFiles, f.gotCol, f.gotRefDay)
	}
}

func TestRunFailureSkipsCloseUnseenAndMarksFailed(t *testing.T) {
	pool := setup(t)
	f := &fake{key: "receita", load: func(context.Context) (connector.Stats, error) { return nil, errors.New("load exploded") }}
	var closed, resolved int
	h := connector.Hooks{
		CloseUnseen: func(context.Context, sources.Source, collection.Collection) (int64, error) { closed++; return 0, nil },
		Resolve:     func(context.Context, int64) error { resolved++; return nil },
	}
	err := connector.Run(ctx, f, req("receita"), pool, nil, h)
	if err == nil || !strings.Contains(err.Error(), "load exploded") {
		t.Fatalf("err = %v", err)
	}
	r := lastCollection(t, pool)
	if r.status != "failed" || r.errMsg == nil || !strings.Contains(*r.errMsg, "load exploded") {
		t.Fatalf("unexpected %+v", r)
	}
	if closed != 0 || resolved != 0 {
		t.Errorf("hooks ran after failure: closeUnseen=%d resolve=%d", closed, resolved)
	}
}

func TestRunSkipsCloseUnseenForNonSnapshotSource(t *testing.T) {
	pool := setup(t)
	f := &fake{key: "camara"}
	var closed, resolved int
	h := connector.Hooks{
		CloseUnseen: func(context.Context, sources.Source, collection.Collection) (int64, error) { closed++; return 0, nil },
		Resolve:     func(context.Context, int64) error { resolved++; return nil },
	}
	if err := connector.Run(ctx, f, req("camara"), pool, nil, h); err != nil {
		t.Fatal(err)
	}
	if closed != 0 || resolved != 1 {
		t.Errorf("closeUnseen=%d resolve=%d, want 0 and 1", closed, resolved)
	}
	if r := lastCollection(t, pool); r.status != "done" {
		t.Fatalf("unexpected %+v", r)
	}
}

func TestRunNilHooksAreSkipped(t *testing.T) {
	pool := setup(t)
	if err := connector.Run(ctx, &fake{key: "receita"}, req("receita"), pool, nil, connector.Hooks{}); err != nil {
		t.Fatal(err)
	}
	if r := lastCollection(t, pool); r.status != "done" {
		t.Fatalf("unexpected %+v", r)
	}
}

func TestRunHookErrorFailsCollection(t *testing.T) {
	pool := setup(t)
	h := connector.Hooks{Resolve: func(context.Context, int64) error { return errors.New("resolve broke") }}
	if err := connector.Run(ctx, &fake{key: "camara"}, req("camara"), pool, nil, h); err == nil {
		t.Fatal("expected error")
	}
	if r := lastCollection(t, pool); r.status != "failed" {
		t.Fatalf("unexpected %+v", r)
	}
}

func TestRunErrorCarriesCollectionID(t *testing.T) {
	pool := setup(t)
	f := &fake{key: "receita", load: func(context.Context) (connector.Stats, error) { return nil, errors.New("x") }}
	err := connector.Run(ctx, f, req("receita"), pool, nil, connector.Hooks{})
	var re *connector.RunError
	if !errors.As(err, &re) || re.CollectionID == 0 || re.CollectionID != f.gotCol.ID {
		t.Fatalf("err = %v (%+v)", err, re)
	}
}

func TestRunBeginFailureIsNotARunError(t *testing.T) {
	pool := setup(t)
	pool.Close() // database unreachable: Begin fails
	err := connector.Run(ctx, &fake{key: "receita"}, req("receita"), pool, nil, connector.Hooks{})
	var re *connector.RunError
	if err == nil || errors.As(err, &re) {
		t.Fatalf("err = %v, want a plain error", err)
	}
}

func TestRunUnknownSourceFailsBeforeBegin(t *testing.T) {
	pool := setup(t)
	f := &fake{key: "nope"}
	if err := connector.Run(ctx, f, req("nope"), pool, nil, connector.Hooks{}); err == nil {
		t.Fatal("expected error")
	}
	if n := count(t, pool); n != 0 || f.fetched != 0 {
		t.Fatalf("collections=%d fetched=%d", n, f.fetched)
	}
}

func TestRunRecoversFromPanicAndMarksFailed(t *testing.T) {
	for name, f := range map[string]*fake{
		"fetch": {key: "receita", fetch: func(context.Context) ([]rawstore.File, error) { panic("fetch blew up") }},
		"load":  {key: "receita", load: func(context.Context) (connector.Stats, error) { panic("load blew up") }},
	} {
		t.Run(name, func(t *testing.T) {
			pool := setup(t)
			err := connector.Run(ctx, f, req("receita"), pool, nil, connector.Hooks{})
			if err == nil || !strings.Contains(err.Error(), "blew up") {
				t.Fatalf("err = %v", err)
			}
			r := lastCollection(t, pool)
			if r.status != "failed" || r.errMsg == nil || !strings.Contains(*r.errMsg, "panic") {
				t.Fatalf("unexpected %+v", r)
			}
		})
	}
}

func TestRunCancelledContextMarksFailed(t *testing.T) {
	pool := setup(t)
	cctx, cancel := context.WithCancel(ctx)
	f := &fake{key: "receita", load: func(c context.Context) (connector.Stats, error) {
		cancel()
		<-c.Done()
		return nil, c.Err()
	}}
	err := connector.Run(cctx, f, req("receita"), pool, nil, connector.Hooks{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if r := lastCollection(t, pool); r.status != "failed" {
		t.Fatalf("unexpected %+v", r)
	}
}

func TestRunCancelledWhileConnectorIgnoresContext(t *testing.T) {
	pool := setup(t)
	cctx, cancel := context.WithCancel(ctx)
	f := &fake{key: "receita", load: func(context.Context) (connector.Stats, error) {
		cancel() // connector returns success anyway
		return connector.Stats{"rows_read": 1, "rows_skipped": 0}, nil
	}}
	err := connector.Run(cctx, f, req("receita"), pool, nil, connector.Hooks{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if r := lastCollection(t, pool); r.status != "failed" {
		t.Fatalf("unexpected %+v", r)
	}
}

func TestRunMissingMandatoryStatsFails(t *testing.T) {
	for name, stats := range map[string]connector.Stats{
		"no rows_read":    {"rows_skipped": 0},
		"no rows_skipped": {"rows_read": 1},
		"nil":             nil,
	} {
		t.Run(name, func(t *testing.T) {
			pool := setup(t)
			f := &fake{key: "receita", load: func(context.Context) (connector.Stats, error) { return stats, nil }}
			var closed int
			h := connector.Hooks{CloseUnseen: func(context.Context, sources.Source, collection.Collection) (int64, error) { closed++; return 0, nil }}
			err := connector.Run(ctx, f, req("receita"), pool, nil, h)
			if err == nil || !strings.Contains(err.Error(), "rows_") {
				t.Fatalf("err = %v", err)
			}
			r := lastCollection(t, pool)
			if r.status != "failed" || r.errMsg == nil || !strings.Contains(*r.errMsg, "rows_") || closed != 0 {
				t.Fatalf("unexpected %+v closed=%d", r, closed)
			}
		})
	}
}

func TestRegisterAndGet(t *testing.T) {
	f := &fake{key: "registry-test"}
	connector.Register(f)
	got, ok := connector.Get("registry-test")
	if !ok || got != connector.Connector(f) {
		t.Fatalf("Get = %v, %v", got, ok)
	}
	if _, ok := connector.Get("missing"); ok {
		t.Error("Get found a connector that was never registered")
	}
}

func TestRunFinishedHookRunsAfterTheCollectionIsDone(t *testing.T) {
	pool := setup(t)
	var steps []string
	var resolvedID, finishedID int64
	h := connector.Hooks{
		Resolve: func(_ context.Context, id int64) error {
			steps = append(steps, "resolve")
			resolvedID = id
			return nil
		},
		Finished: func(_ context.Context, id int64) {
			steps = append(steps, "finished:"+lastCollection(t, pool).status)
			finishedID = id
		},
	}
	if err := connector.Run(ctx, &fake{key: "receita"}, req("receita"), pool, nil, h); err != nil {
		t.Fatal(err)
	}
	if strings.Join(steps, ",") != "resolve,finished:done" {
		t.Errorf("steps = %v, want resolve then finished on a done collection", steps)
	}
	if resolvedID == 0 || finishedID != resolvedID {
		t.Errorf("finished got collection %d, resolve got %d", finishedID, resolvedID)
	}
}

func TestRunFinishedHookSkippedOnFailure(t *testing.T) {
	pool := setup(t)
	called := false
	h := connector.Hooks{
		Resolve:  func(context.Context, int64) error { return errors.New("resolve broke") },
		Finished: func(context.Context, int64) { called = true },
	}
	if err := connector.Run(ctx, &fake{key: "receita"}, req("receita"), pool, nil, h); err == nil {
		t.Fatal("expected error")
	}
	if called {
		t.Error("Finished ran for a failed collection")
	}
}
