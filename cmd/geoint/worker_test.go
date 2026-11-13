package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/identity"
	"geoint/internal/search"
	"geoint/internal/sources"
	"geoint/internal/testutil"
)

const validBody = `{"source":"camara","reference_date":"2026-10-01T00:00:00Z","scope":{"full":true}}`

func neverRun(t *testing.T) func(context.Context, connector.Connector, connector.Request) error {
	return func(context.Context, connector.Connector, connector.Request) error {
		t.Error("run must not be called")
		return nil
	}
}

// useConnectors makes handleRequest see only the given connectors (by key)
// for the duration of the test.
func useConnectors(t *testing.T, cs ...connector.Connector) {
	t.Helper()
	prev := lookupConnector
	t.Cleanup(func() { lookupConnector = prev })
	lookupConnector = func(key string) (connector.Connector, bool) {
		for _, c := range cs {
			if c.Key() == key {
				return c, true
			}
		}
		return nil, false
	}
}

func TestHandleRequestAcksRequestsThatCanNeverSucceed(t *testing.T) {
	useConnectors(t) // camara is a known source; here nothing serves it
	bodies := map[string]string{
		"malformed":      `not json`,
		"no reference":   `{"source":"camara"}`,
		"unknown source": `{"source":"nope","reference_date":"2026-10-01T00:00:00Z"}`,
		"no connector":   validBody, // camara is a known source but no connector serves it
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			if err := handleRequest(context.Background(), []byte(body), neverRun(t)); err != nil {
				t.Errorf("err = %v, want nil (ack)", err)
			}
		})
	}
}

type stubConnector struct{ connector.Connector }

func (stubConnector) Key() string { return "camara" }

func TestHandleRequestRunOutcomes(t *testing.T) {
	useConnectors(t, stubConnector{})
	runWith := func(err error) func(context.Context, connector.Connector, connector.Request) error {
		return func(context.Context, connector.Connector, connector.Request) error { return err }
	}
	t.Run("success acks", func(t *testing.T) {
		if err := handleRequest(context.Background(), []byte(validBody), runWith(nil)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("nothing new acks", func(t *testing.T) {
		err := handleRequest(context.Background(), []byte(validBody),
			runWith(fmt.Errorf("connector receita: edition 2026-10-01: %w", connector.ErrNothingNew)))
		if err != nil {
			t.Fatalf("err = %v, want nil (ack)", err)
		}
	})
	t.Run("failed run is recorded and acked", func(t *testing.T) {
		err := handleRequest(context.Background(), []byte(validBody),
			runWith(&connector.RunError{CollectionID: 9, Err: errors.New("load exploded")}))
		if err != nil {
			t.Fatalf("err = %v, want nil (ack)", err)
		}
	})
	t.Run("run that never started naks", func(t *testing.T) {
		boom := errors.New("database down")
		err := handleRequest(context.Background(), []byte(validBody), runWith(boom))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the begin error so the message is redelivered", err)
		}
	})
	t.Run("shutdown naks even for a recorded failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := handleRequest(ctx, []byte(validBody),
			runWith(&connector.RunError{CollectionID: 9, Err: context.Canceled}))
		if err == nil {
			t.Fatal("err = nil, want an error so the message is redelivered")
		}
	})
}

func TestCommonNameThreshold(t *testing.T) {
	for in, want := range map[string]int{"": 100, "1": 1, "250": 250} {
		if got, err := commonNameThreshold(in); err != nil || got != want {
			t.Errorf("commonNameThreshold(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "-3", "abc", "1.5", " 10"} {
		if _, err := commonNameThreshold(in); err == nil {
			t.Errorf("commonNameThreshold(%q) accepted an invalid value", in)
		}
	}
}

func TestRegisterConnectorsRegistersEverySource(t *testing.T) {
	registerConnectors()
	registerConnectors() // idempotent
	for _, src := range sources.All {
		c, ok := connector.Get(src.Key)
		if ok != src.HasConnector || (ok && c.Key() != src.Key) {
			t.Errorf("%s: registered %v (connector %v), catalogue HasConnector %v", src.Key, ok, c, src.HasConnector)
		}
	}
}

func TestRunBothStopsTheOtherOnError(t *testing.T) {
	boom := errors.New("boom")
	err := runBoth(context.Background(),
		func(context.Context) error { return boom },
		func(ctx context.Context) error { <-ctx.Done(); return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wait := func(ctx context.Context) error { <-ctx.Done(); return nil }
	if err := runBoth(ctx, wait, wait); err != nil {
		t.Fatalf("clean cancel: %v", err)
	}
}

// fakeSteps records the order in which the collection hooks call the services.
type fakeSteps struct {
	calls      []string
	rebuildErr error
}

func (f *fakeSteps) CloseUnseen(_ context.Context, _ sources.Source, _ collection.Collection, _ float64) (int64, error) {
	f.calls = append(f.calls, "close")
	return 0, nil
}

func (f *fakeSteps) Resolve(context.Context, int64) (identity.Result, error) {
	f.calls = append(f.calls, "resolve")
	return identity.Result{}, nil
}

func (f *fakeSteps) RefreshCollection(_ context.Context, collectionID int64) (search.Stats, bool, error) {
	f.calls = append(f.calls, fmt.Sprintf("refresh:%d", collectionID))
	return search.Stats{}, f.rebuildErr == nil, f.rebuildErr
}

func (f *fakeSteps) RebuildIfDirty(context.Context) (search.Stats, bool, error) {
	f.calls = append(f.calls, "rebuild-if-dirty")
	return search.Stats{}, true, f.rebuildErr
}

// captureLogs sends slog output to a buffer for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// runHooks calls the hooks in the order connector.Run does for a snapshot
// source whose collection succeeded.
func runHooks(t *testing.T, h connector.Hooks) {
	t.Helper()
	src, _ := sources.ByKey("receita")
	if _, err := h.CloseUnseen(context.Background(), src, collection.Collection{ID: 41}); err != nil {
		t.Fatal(err)
	}
	if err := h.Resolve(context.Background(), 41); err != nil {
		t.Fatal(err)
	}
	h.Finished(context.Background(), 41)
}

func TestCollectionHooksRefreshSearchAfterResolve(t *testing.T) {
	captureLogs(t)
	f := &fakeSteps{}
	h := collectionHooks(f, f, f)
	if h.Finished == nil {
		t.Fatal("no Finished hook: search is never rebuilt")
	}
	runHooks(t, h)
	if got := strings.Join(f.calls, ","); got != "close,resolve,refresh:41" {
		t.Fatalf("calls = %s, want close,resolve,refresh:41", got)
	}
}

func TestCollectionHooksRebuildFailureIsLoggedNotReturned(t *testing.T) {
	logs := captureLogs(t)
	f := &fakeSteps{rebuildErr: errors.New("disk full")}
	runHooks(t, collectionHooks(f, f, f))
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "collection=41") || !strings.Contains(out, "disk full") {
		t.Fatalf("rebuild failure not logged as an error with the collection id:\n%s", out)
	}
}

// blockingRebuilder blocks RebuildIfDirty until ctx is cancelled.
type blockingRebuilder struct {
	fakeSteps
	started chan struct{}
}

func (b *blockingRebuilder) RebuildIfDirty(ctx context.Context) (search.Stats, bool, error) {
	close(b.started)
	<-ctx.Done()
	return search.Stats{}, false, ctx.Err()
}

func TestSearchCatchUpRunsInBackgroundAndStopsWithContext(t *testing.T) {
	captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	b := &blockingRebuilder{started: make(chan struct{})}
	done := searchCatchUp(ctx, b) // must return while the rebuild still runs
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("catch-up rebuild never started")
	}
	select {
	case <-done:
		t.Fatal("catch-up finished before cancellation")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("catch-up did not stop with the context")
	}
}

func TestSearchCatchUpLogsErrors(t *testing.T) {
	logs := captureLogs(t)
	f := &fakeSteps{rebuildErr: errors.New("disk full")}
	<-searchCatchUp(context.Background(), f)
	if got := strings.Join(f.calls, ","); got != "rebuild-if-dirty" {
		t.Errorf("calls = %s", got)
	}
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "disk full") {
		t.Errorf("catch-up failure not logged:\n%s", out)
	}
}

func TestSchedulerEnabled(t *testing.T) {
	for v, want := range map[string]bool{"": true, "true": true, "1": true, "false": false, "0": false} {
		got, err := schedulerEnabled(v)
		if err != nil || got != want {
			t.Errorf("%q: got %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"yes", "FALSE", "off", "2"} {
		if _, err := schedulerEnabled(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestStartSchedulerDisabledNeverTicks(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	tick := func(context.Context) ([]string, error) { mu.Lock(); calls++; mu.Unlock(); return nil, nil }
	done := startScheduler(context.Background(), false, tick, time.Millisecond, time.Millisecond)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled scheduler started a loop")
	}
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("tick called %d times", calls)
	}
}

func TestStartSchedulerTicksRepeatedlySurvivesErrorsAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	var mu sync.Mutex
	calls := 0
	tick := func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return nil, errors.New("boom")
		}
		return []string{"receita"}, nil
	}
	done := startScheduler(ctx, true, tick, time.Millisecond, 5*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d ticks", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop with the context")
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != n {
		t.Fatalf("ticks continued after cancel: %d -> %d", n, calls)
	}
	out := buf.String()
	if !strings.Contains(out, "scheduler tick failed") || !strings.Contains(out, "source=receita") {
		t.Fatalf("log missing error or requested source:\n%s", out)
	}
}

func TestWorkerPoolSize(t *testing.T) {
	parse := func(url string) *pgxpool.Config {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := parse("postgres://u:p@db:5432/x")
	if err := sizeWorkerPool(cfg, false); err != nil || cfg.MaxConns != workerDefaultConns {
		t.Errorf("no pool_max_conns: %d, %v; want the default %d", cfg.MaxConns, err, workerDefaultConns)
	}
	cfg = parse("postgres://u:p@db:5432/x?pool_max_conns=20")
	if err := sizeWorkerPool(cfg, true); err != nil || cfg.MaxConns != 20 {
		t.Errorf("pool_max_conns=20: %d, %v", cfg.MaxConns, err)
	}
	cfg = parse("postgres://u:p@db:5432/x?pool_max_conns=6")
	err := sizeWorkerPool(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "at least 12") {
		t.Errorf("pool_max_conns=6: err = %v, want a clear refusal", err)
	}
	if err != nil && strings.Contains(err.Error(), ":p@") {
		t.Error("the error leaks the password")
	}
}

// With the scheduler disabled, the worker still fails dead runs at start
// (connector.FailDeadRuns, called before anything else).
func TestWorkerStartFailsDeadRunsWithTheSchedulerOff(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	dead, err := collection.Begin(context.Background(), pool, 1, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if on, err := schedulerEnabled("false"); err != nil || on {
		t.Fatal("SCHEDULER_ENABLED=false not parsed as off")
	}
	connector.FailDeadRuns(context.Background(), pool)
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM collection WHERE id = $1`, dead.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != collection.StatusFailed {
		t.Fatalf("status %s, want failed", status)
	}
}
