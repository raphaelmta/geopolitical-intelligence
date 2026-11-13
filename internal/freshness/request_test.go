package freshness_test

import (
	"errors"
	"testing"
	"time"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/freshness"
	"geoint/internal/sources"
)

var receitaSrc, _ = sources.ByKey("receita")

// ask is an admin's request of a collection of receita with scope.
func (e *env) ask(pub *fakePub, scope collection.Scope) error {
	e.t.Helper()
	return e.svc.Request(bg, pub, receitaSrc, connector.Request{Source: "receita", ReferenceDate: day(2026, 6, 1), Scope: scope}, "sub-admin")
}

type requestRow struct {
	by        string
	full      bool
	published bool
	fromDB    bool // requested_at is the database clock
}

func (e *env) requestRows() []requestRow {
	e.t.Helper()
	rows, err := e.p.Query(bg, `SELECT requested_by, full_scope, published_at IS NOT NULL,
		requested_at BETWEEN now() - interval '1 minute' AND now() FROM collection_request ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []requestRow
	for rows.Next() {
		var r requestRow
		if err := rows.Scan(&r.by, &r.full, &r.published, &r.fromDB); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestRequestRecordsAndPublishes(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	pub := &fakePub{}
	if err := e.ask(pub, full); err != nil {
		t.Fatal(err)
	}
	if len(pub.reqs) != 1 || pub.subj[0] != "ingest.request.receita" || !pub.reqs[0].Scope.Full {
		t.Fatalf("published %v %+v", pub.subj, pub.reqs)
	}
	rows := e.requestRows()
	if len(rows) != 1 || rows[0] != (requestRow{by: "sub-admin", full: true, published: true, fromDB: true}) {
		t.Fatalf("rows %+v", rows)
	}
}

func TestRequestFullIsBlockedWhileARequestWaits(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	pub := &fakePub{}
	if err := e.ask(pub, full); err != nil {
		t.Fatal(err)
	}
	// Still waiting two days later.
	e.advance(48 * time.Hour)
	if err := e.ask(pub, full); !errors.Is(err, freshness.ErrRequestBlocked) {
		t.Fatalf("err = %v, want ErrRequestBlocked", err)
	}
	if len(pub.reqs) != 1 || len(e.requestRows()) != 1 {
		t.Fatalf("published %d, rows %d; want 1 and 1", len(pub.reqs), len(e.requestRows()))
	}
	// An unpublished request (in flight) blocks too, for a short while.
	f := newEnv(t, day(2026, 6, 1))
	f.request(1, 5*time.Minute, false)
	if err := f.ask(pub, full); !errors.Is(err, freshness.ErrRequestBlocked) {
		t.Fatalf("in flight: err = %v, want ErrRequestBlocked", err)
	}
}

func TestRequestFullIsBlockedWhileRunning(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	id := e.col(1, day(2026, 5, 1), full, "") // running
	if err := e.ask(&fakePub{}, full); !errors.Is(err, freshness.ErrRequestBlocked) {
		t.Fatalf("err = %v, want ErrRequestBlocked", err)
	}
	// A live run blocks whatever its age.
	if _, err := e.p.Exec(bg, `UPDATE collection SET started_at = now() - interval '25 hours' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := e.ask(&fakePub{}, full); !errors.Is(err, freshness.ErrRequestBlocked) {
		t.Fatalf("long live run: err = %v, want ErrRequestBlocked", err)
	}
}

// A run left behind by a killed worker (its lock free) does not block,
// whatever its age.
func TestRequestFullIsNotBlockedByADeadRun(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	run, err := collection.Start(bg, e.p, 1, day(2026, 5, 1), time.Time{}, full)
	if err != nil {
		t.Fatal(err)
	}
	run.Release() // the process died
	if err := e.ask(&fakePub{}, full); err != nil {
		t.Fatalf("dead run: %v", err)
	}
}

// An admin's explicit request is not held back by the scheduler's 24 h
// rule: once the waiting request started (or was answered by a check), a
// new one is accepted at once.
func TestRequestFullIsAllowedOnceTheLastOneStarted(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	pub := &fakePub{}
	if err := e.ask(pub, full); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	e.col(1, day(2026, 6, 1), full, "done")
	if err := e.ask(pub, full); err != nil {
		t.Fatalf("after a collection started and finished: %v", err)
	}
	e.advance(time.Minute)
	if _, err := e.p.Exec(bg, `INSERT INTO source_check (source_id, last_checked_at, last_outcome) VALUES (1, now(), 'nothing_new')`); err != nil {
		t.Fatal(err)
	}
	if err := e.ask(pub, full); err != nil {
		t.Fatalf("after a nothing-new check: %v", err)
	}
	if len(pub.reqs) != 3 {
		t.Fatalf("published %d, want 3", len(pub.reqs))
	}
}

func TestRequestSliceIsNotBlockedAndDoesNotBlock(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	pub := &fakePub{}
	slice := collection.Scope{CNPJs: []string{"12345678"}, Depth: 1}
	if err := e.ask(pub, slice); err != nil {
		t.Fatal(err)
	}
	// A slice request is no full request: the scheduler still asks for one.
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("tick after a slice request: %v", got)
	}
	// A full request waits now; a slice is still accepted.
	if err := e.ask(pub, slice); err != nil {
		t.Fatalf("slice while a full request waits: %v", err)
	}
	rows := e.requestRows()
	if len(rows) != 3 || rows[0].full || !rows[1].full || rows[2].full || !rows[2].published {
		t.Fatalf("rows %+v", rows)
	}
}

func TestTickAfterAManualRequestDoesNotDuplicate(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	if err := e.ask(&fakePub{}, full); err != nil {
		t.Fatal(err)
	}
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("tick after a manual request: %v", got)
	}
}

// A slice collection that starts after a full request is not that request.
func TestSliceCollectionDoesNotAnswerAFullRequest(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	e.freshOthers("receita")
	e.collected(1)
	e.request(1, 30*time.Hour, true)
	id := e.col(1, day(2026, 6, 1), collection.Scope{CNPJs: []string{"12345678"}, Depth: 1}, "done")
	e.startedAt(id, 29*time.Hour, 28*time.Hour)
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Fatalf("a slice collection unblocked a waiting full request: %v", got)
	}
}

func TestPendingRequestAt(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	if st := e.receita(); st.PendingRequestAt != nil {
		t.Fatalf("pending %v before any request", st.PendingRequestAt)
	}
	if err := e.ask(&fakePub{}, full); err != nil {
		t.Fatal(err)
	}
	var at time.Time
	if err := e.p.QueryRow(bg, `SELECT requested_at FROM collection_request`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if st := e.receita(); st.PendingRequestAt == nil || !st.PendingRequestAt.Equal(at) {
		t.Fatalf("pending %v, want %v", st.PendingRequestAt, at)
	}
	e.advance(time.Minute)
	e.col(1, day(2026, 6, 1), full, "") // the collection started
	if st := e.receita(); st.PendingRequestAt != nil {
		t.Fatalf("pending %v after the collection started", st.PendingRequestAt)
	}
}

func TestRequestPublishFailureLeavesTheRowUnpublished(t *testing.T) {
	e := newEnv(t, day(2026, 6, 1))
	boom := errors.New("nats down")
	if err := e.ask(&fakePub{err: boom}, full); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if rows := e.requestRows(); len(rows) != 1 || rows[0].published {
		t.Fatalf("rows %+v", rows)
	}
}
