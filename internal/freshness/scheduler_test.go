package freshness_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/freshness"
	"geoint/internal/sources"
	"geoint/internal/testutil"
)

// fakePub records published messages; err, when set, is returned instead.
type fakePub struct {
	mu   sync.Mutex
	subj []string
	reqs []connector.Request
	err  error
}

func (f *fakePub) Publish(_ context.Context, subject string, v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var r connector.Request
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	f.subj = append(f.subj, subject)
	f.reqs = append(f.reqs, r)
	return nil
}

var full = collection.Scope{Full: true}

type env struct {
	t   *testing.T
	p   *pgxpool.Pool
	svc *freshness.Service
	now time.Time
}

// newEnv: every source of the catalogue is requestable (HasConnector true).
func newEnv(t *testing.T, now time.Time) *env {
	t.Helper()
	p := testutil.DB(t)
	testutil.SeedSources(t, p)
	// Time travel: these tests age database-clock stamps (advance,
	// startedAt, request), which the guards of migration 0019 forbid in
	// production. The guards are tested in internal/collection; here they
	// are disabled on this test's own schema only.
	for _, q := range []string{
		`ALTER TABLE collection DISABLE TRIGGER collection_guard_update`,
		`ALTER TABLE collection_request DISABLE TRIGGER collection_request_guard_update`,
		`ALTER TABLE collection_request DISABLE TRIGGER collection_request_no_delete`,
	} {
		if _, err := p.Exec(bg, q); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t, p: p, now: now}
	e.svc = freshness.New(p, func() time.Time { return e.now })
	for _, s := range sources.All {
		s.HasConnector = true
		e.svc.Catalogue = append(e.svc.Catalogue[:len(e.svc.Catalogue):len(e.svc.Catalogue)], s)
	}
	return e
}

// col records a collection of src observed on ref: finished "done" or
// "failed", or "" running with a live worker (its run lock held until the test ends; see
// deadRun for one without).
func (e *env) col(src int16, ref time.Time, s collection.Scope, finish string) int64 {
	e.t.Helper()
	if finish == "" {
		run, err := collection.Start(bg, e.p, src, ref, time.Time{}, s)
		if err != nil {
			e.t.Fatal(err)
		}
		e.t.Cleanup(run.Release)
		return run.ID
	}
	// Observed on its reference date, so staleness does not depend on the
	// real day the test runs.
	c, err := collection.BeginObserved(bg, e.p, src, ref, ref, s)
	if err != nil {
		e.t.Fatal(err)
	}
	switch finish {
	case "done":
		err = collection.Finish(bg, e.p, c.ID, nil)
	case "failed":
		err = collection.Fail(bg, e.p, c.ID, errors.New("boom"))
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return c.ID
}

// collected gives src a done full collection 90 days old (reference date
// and database clock), so the scheduler keeps it fresh from now on: it is
// due by age, and it never answers a request made since.
func (e *env) collected(src int16) int64 {
	e.t.Helper()
	id := e.col(src, e.now.AddDate(0, 0, -90), full, "done")
	e.startedAt(id, 90*24*time.Hour, 90*24*time.Hour)
	return id
}

// request records a full request made ago (database clock), published or not.
func (e *env) request(src int16, ago time.Duration, published bool) {
	e.t.Helper()
	if _, err := e.p.Exec(bg, `INSERT INTO collection_request (source_id, requested_at, requested_by, full_scope, published_at)
		VALUES ($1, now() - make_interval(secs => $2), 'scheduler', true,
			CASE WHEN $3 THEN now() - make_interval(secs => $2) END)`, src, ago.Seconds(), published); err != nil {
		e.t.Fatal(err)
	}
}

// advance moves the test forward by d: the scheduler's clock (reference
// dates) moves ahead, and every instant stamped by the database clock
// (requests, collection starts and ends, checks) moves back, as if d had
// passed since.
func (e *env) advance(d time.Duration) {
	e.t.Helper()
	e.now = e.now.Add(d)
	secs := d.Seconds()
	for _, q := range []string{
		`UPDATE collection_request SET requested_at = requested_at - make_interval(secs => $1),
			published_at = published_at - make_interval(secs => $1)`,
		`UPDATE collection SET started_at = started_at - make_interval(secs => $1),
			finished_at = finished_at - make_interval(secs => $1)`,
		`UPDATE source_check SET last_checked_at = last_checked_at - make_interval(secs => $1)`,
	} {
		if _, err := e.p.Exec(bg, q, secs); err != nil {
			e.t.Fatal(err)
		}
	}
}

// advanceTo is advance up to the scheduler time t.
func (e *env) advanceTo(t time.Time) { e.advance(t.Sub(e.now)) }

func (e *env) tick(pub *fakePub) []string {
	e.t.Helper()
	got, err := e.svc.TickWith(bg, pub)
	if err != nil {
		e.t.Fatal(err)
	}
	sort.Strings(got)
	return got
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// freshOthers makes every source but keep fresh, so a test looks at one.
func (e *env) freshOthers(keep string) {
	for _, s := range sources.All {
		if s.Key != keep {
			e.col(s.ID, e.now.Add(-time.Hour).Truncate(24*time.Hour), full, "done")
		}
	}
}

// The scheduler never makes the first collection of a source (inverted
// from "never collected is requested"): the first national load of each
// source is an explicit request.
func TestTickNeverRequestsAFirstCollection(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	pub := &fakePub{}
	if got := e.tick(pub); len(got) != 0 || len(pub.reqs) != 0 {
		t.Fatalf("requested %v (published %d), want nothing before a first collection", got, len(pub.reqs))
	}
	// A first explicit collection that failed is not retried either.
	e.col(1, e.now, full, "failed")
	if got := e.tick(pub); len(got) != 0 {
		t.Fatalf("requested %v after a failed first collection, want nothing (the owner retries)", got)
	}
	// Once one is done, the scheduler keeps the source fresh.
	e.collected(2)
	if got := e.tick(pub); !eq(got, []string{"tse"}) {
		t.Fatalf("requested %v, want tse (done once, now due)", got)
	}
}

// A running collection that no live process holds (a killed worker) is
// failed by the tick, whatever its age.
func TestTickFailsADeadRunAtOnce(t *testing.T) {
	e := newEnv(t, day(2026, 6, 10))
	e.freshOthers("receita")
	e.collected(1)
	dead, err := collection.Begin(bg, e.p, 1, day(2026, 6, 10), full) // no run lock: dead
	if err != nil {
		t.Fatal(err)
	}
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("requested %v, want receita (its last run failed)", got)
	}
	var status, msg string
	if err := e.p.QueryRow(bg, `SELECT status, error FROM collection WHERE id = $1`, dead.ID).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || msg != collection.InterruptedError {
		t.Fatalf("status %q error %q", status, msg)
	}
}

func TestTickOnlyRequestsSourcesWithAConnector(t *testing.T) {
	p := testutil.DB(t)
	testutil.SeedSources(t, p)
	svc := freshness.New(p, func() time.Time { return day(2026, 6, 1) })
	// Every real source has a connector: a catalogue with one without.
	svc.Catalogue = nil
	for _, s := range sources.All {
		if s.Key == "camara" {
			s.HasConnector = false
		}
		svc.Catalogue = append(svc.Catalogue, s)
		// Collected once, long ago: due.
		if _, err := p.Exec(bg, `ALTER TABLE collection DISABLE TRIGGER collection_guard_update`); err != nil {
			t.Fatal(err)
		}
		c, err := collection.Begin(bg, p, s.ID, day(2026, 1, 1), full)
		if err != nil {
			t.Fatal(err)
		}
		if err := collection.Finish(bg, p, c.ID, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(bg, `UPDATE collection SET started_at = '2026-01-01' WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
	}
	pub := &fakePub{}
	got, err := svc.TickWith(bg, pub)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, s := range svc.Catalogue {
		if s.HasConnector {
			want = append(want, s.Key)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(want) == 0 || !eq(got, want) {
		t.Fatalf("requested %v, want %v", got, want)
	}
}

func TestTickPublishesAFullRequest(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1).Add(5*time.Hour))
	e.freshOthers("receita")
	e.collected(1)
	pub := &fakePub{}
	e.tick(pub)
	if len(pub.reqs) != 1 || pub.subj[0] != "ingest.request.receita" {
		t.Fatalf("subjects %v", pub.subj)
	}
	r := pub.reqs[0]
	if r.Source != "receita" || !r.Scope.Full || len(r.Scope.CNPJs) != 0 || !r.ReferenceDate.Equal(day(2026, 6, 1)) {
		t.Fatalf("request %+v", r)
	}
	var by string
	var pubAt *time.Time
	if err := e.p.QueryRow(bg, `SELECT requested_by, published_at FROM collection_request WHERE source_id = 1`).Scan(&by, &pubAt); err != nil {
		t.Fatal(err)
	}
	if by != "scheduler" || pubAt == nil {
		t.Fatalf("row by=%q published_at=%v", by, pubAt)
	}
}

func TestTickSkipsFreshSource(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.col(1, e.now.AddDate(0, 0, -10), full, "done") // receita publishes every 30 days
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("requested %v", got)
	}
}

func TestTickRequestsWhenLastDoneIsOlderThanPublishEvery(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.col(1, e.now.AddDate(0, 0, -31), full, "done")
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("requested %v", got)
	}
}

// Boundary: a done collection whose reference date is exactly PublishEvery
// old is due (the next edition is expected from that very instant); one
// second earlier it is not.
func TestTickBoundaryExactlyPublishEveryIsDue(t *testing.T) {
	ref := day(2026, 5, 2)
	every := 30 * 24 * time.Hour
	for _, tc := range []struct {
		name string
		now  time.Time
		want []string
	}{
		{"exactly", ref.Add(every), []string{"receita"}},
		{"one second before", ref.Add(every - time.Second), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.now)
			e.freshOthers("receita")
			e.col(1, ref, full, "done")
			if got := e.tick(&fakePub{}); !eq(got, tc.want) {
				t.Fatalf("requested %v, want %v", got, tc.want)
			}
		})
	}
}

// A slice never counts as a first collection: a slice-only source is still
// never requested by the scheduler (inverted with the first-collection rule).
func TestTickSliceOnlySourceCountsAsNeverCollected(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.col(1, e.now, collection.Scope{CNPJs: []string{"12345678"}, Depth: 1}, "done")
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("requested %v, want nothing (no full collection yet)", got)
	}
}

func TestTickSkipsWhileRunning(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	e.col(1, e.now, full, "") // running (live), and due by age
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("requested %v", got)
	}
}

func TestTickNeverFailsALiveRunForItsAge(t *testing.T) {
	e := newEnv(t, day(2026, 6, 10))
	e.freshOthers("receita")
	e.collected(1)
	// Running with its lock held (a live process) and 25 h old: left
	// running (only logged), and it still blocks a new request.
	id := e.col(1, day(2026, 5, 1), full, "")
	if _, err := e.p.Exec(bg, `UPDATE collection SET started_at = now() - interval '25 hours' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("requested %v while a live run is in progress", got)
	}
	var status string
	if err := e.p.QueryRow(bg, `SELECT status FROM collection WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("status %q, want running (a live run is never failed for its age)", status)
	}
	if err := collection.Finish(bg, e.p, id, nil); err != nil {
		t.Fatalf("Finish of the long live run: %v", err)
	}
}

// The same long run, once its process is gone (lock released), is failed
// by the tick and the source is requested in the same tick.
func TestTickFailsALongRunOnceItsLockIsFree(t *testing.T) {
	e := newEnv(t, day(2026, 6, 10))
	e.freshOthers("receita")
	e.collected(1)
	run, err := collection.Start(bg, e.p, 1, day(2026, 5, 1), time.Time{}, full)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Exec(bg, `UPDATE collection SET started_at = now() - interval '25 hours' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	run.Release() // the process died
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("requested %v", got)
	}
	var status, msg string
	if err := e.p.QueryRow(bg, `SELECT status, error FROM collection WHERE id = $1`, run.ID).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || msg != collection.InterruptedError {
		t.Fatalf("status %q error %q", status, msg)
	}
}

func TestTickKeepsRecentRunningCollection(t *testing.T) {
	e := newEnv(t, day(2026, 6, 10))
	e.freshOthers("receita")
	e.collected(1)
	id := e.col(1, day(2026, 5, 1), full, "")
	if _, err := e.p.Exec(bg, `UPDATE collection SET started_at = now() - interval '23 hours' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("requested %v", got)
	}
	var status string
	_ = e.p.QueryRow(bg, `SELECT status FROM collection WHERE id = $1`, id).Scan(&status)
	if status != "running" {
		t.Fatalf("status %q", status)
	}
}

func TestTickDoesNotRequestTwiceWithin24h(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("first tick requested %v", got)
	}
	e.advance(23 * time.Hour)
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("second tick requested %v", got)
	}
}

// startedAt moves a collection's start and finish (database clock) to
// these durations ago.
func (e *env) startedAt(id int64, started, finished time.Duration) {
	e.t.Helper()
	if _, err := e.p.Exec(bg, `UPDATE collection SET started_at = now() - make_interval(secs => $2),
		finished_at = now() - make_interval(secs => $3) WHERE id = $1`, id, started.Seconds(), finished.Seconds()); err != nil {
		e.t.Fatal(err)
	}
}

func TestTickQueuedRequestBlocksUntilACollectionStarts(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	// Published 30 h ago, still waiting in the queue: no duplicate.
	e.request(1, 30*time.Hour, true)
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("a queued request 30 h old was duplicated: %v", got)
	}
	// 73 h without any collection: requested again.
	e.advance(43 * time.Hour)
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("requested %v after 73 h, want receita", got)
	}
}

func TestTickQueuedRequestFollowedByAFailedCollectionIsRetried(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	e.request(1, 30*time.Hour, true)
	id := e.col(1, e.now.AddDate(0, 0, -2), full, "failed")
	e.startedAt(id, 29*time.Hour, 25*time.Hour)
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("requested %v, want receita (failure retried once a day)", got)
	}
}

func TestTickQueuedRequestFollowedByANothingNewCheckUnblocks(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	old := e.col(1, e.now.AddDate(0, 0, -40), full, "done") // due by age
	e.startedAt(old, 40*24*time.Hour, 40*24*time.Hour)
	e.request(1, 30*time.Hour, true)
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("requested %v while the request is unanswered", got)
	}
	if _, err := e.p.Exec(bg, `INSERT INTO source_check (source_id, last_checked_at, last_outcome)
		VALUES (1, now() - interval '29 hours', 'nothing_new')`); err != nil {
		t.Fatal(err)
	}
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("requested %v, want receita (the request was answered)", got)
	}
}

func TestTickRetriesFailedOncePerDay(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.col(1, e.now.AddDate(0, 0, -5), full, "done")   // would be fresh...
	e.col(1, e.now.AddDate(0, 0, -1), full, "failed") // ...but the latest failed
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("requested %v", got)
	}
	// The requested run starts and fails again.
	e.advance(time.Minute)
	e.col(1, e.now, full, "failed")
	e.advance(6 * time.Hour)
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("retried within a day: %v", got)
	}
	e.advance(19 * time.Hour)
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("no retry after a day: %v", got)
	}
}

func TestTickUnpublishedRequestBlocksOnlyBriefly(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	e.request(1, 5*time.Minute, false)
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("requested %v with a 5 min old unpublished request", got)
	}
	e.request(1, 11*time.Minute, false)
	if _, err := e.p.Exec(bg, `DELETE FROM collection_request WHERE requested_at > now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("an 11 min old unpublished request blocked: %v", got)
	}
}

func TestTickPublishFailureReturnsErrorAndLeavesRowUnpublished(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	boom := errors.New("nats down")
	got, err := e.svc.TickWith(bg, &fakePub{err: boom})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("requested %v despite the failure", got)
	}
	var n int
	if err := e.p.QueryRow(bg, `SELECT count(*) FROM collection_request WHERE source_id = 1 AND published_at IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unpublished rows %d (%v)", n, err)
	}
	// The retry window is short: 11 minutes later it is requested again.
	e.advance(11 * time.Minute)
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("retry requested %v", got)
	}
}

func TestTickConcurrentTicksRequestOnce(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	pub := &fakePub{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.TickWith(bg, pub); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(pub.reqs) != 1 {
		t.Fatalf("published %d requests, want 1", len(pub.reqs))
	}
	var n int
	_ = e.p.QueryRow(bg, `SELECT count(*) FROM collection_request`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d request rows, want 1", n)
	}
}

func TestTickThroughRealQueue(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	got, err := e.svc.Tick(bg, testutil.Queue(t))
	if err != nil || !eq(got, []string{"receita"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// The long-run query only feeds a log line: when it fails, the tick logs it
// and still requests what is due.
func TestTickGoesOnWhenTheLongRunQueryFails(t *testing.T) {
	restore := freshness.SetLongRunsSQL(`SELECT no_such_column FROM collection WHERE $1::text <> '' AND $2::float8 > 0`)
	defer restore()
	e := newEnv(t, day(2026, 6, 10))
	e.freshOthers("receita")
	e.collected(1)
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("requested %v, want receita despite the failing long-run query", got)
	}
}
