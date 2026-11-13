package freshness_test

import (
	"testing"
	"time"

	"geoint/internal/freshness"
)

func (e *env) receita() freshness.SourceStatus {
	e.t.Helper()
	all, err := e.svc.List(bg)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, s := range all {
		if s.Key == "receita" {
			return s
		}
	}
	e.t.Fatal("receita not listed")
	return freshness.SourceStatus{}
}

// check records a successful "nothing new" check now, as the runner does.
func (e *env) check() time.Time {
	e.t.Helper()
	var at time.Time
	if err := e.p.QueryRow(bg, `INSERT INTO source_check (source_id, last_checked_at, last_outcome)
		VALUES (1, clock_timestamp(), 'nothing_new')
		ON CONFLICT (source_id) DO UPDATE SET last_checked_at = EXCLUDED.last_checked_at
		RETURNING last_checked_at`).Scan(&at); err != nil {
		e.t.Fatal(err)
	}
	return at
}

func TestFailedAttemptIsClearedByALaterSuccessfulCheck(t *testing.T) {
	e := newEnv(t, day(2026, 11, 12))
	e.col(1, day(2026, 11, 1), full, "done")
	doneAt := e.receita().LastCheckedAt
	if doneAt == nil {
		t.Fatal("last_checked_at is nil after a done collection")
	}

	// A failed attempt (e.g. the listing answered 503).
	e.col(1, day(2026, 11, 12), full, "failed")
	if st := e.receita(); st.Status != freshness.Failed || st.LastError != "boom" || !st.LastCheckedAt.Equal(*doneAt) {
		t.Errorf("after the failure: %s %q checked=%v", st.Status, st.LastError, st.LastCheckedAt)
	}

	// Next check finds nothing new: back to the edition's own status.
	at := e.check()
	st := e.receita()
	if st.Status != freshness.Fresh || st.LastError != "" || st.LastCheckedAt == nil || !st.LastCheckedAt.Equal(at) ||
		!st.LastReferenceDate.Equal(day(2026, 11, 1)) {
		t.Errorf("after the check: %s %q checked=%v ref=%v", st.Status, st.LastError, st.LastCheckedAt, st.LastReferenceDate)
	}
	// Stale by the edition date, not failed, once StaleAfter has passed.
	e.now = day(2026, 12, 20)
	if st := e.receita(); st.Status != freshness.Stale {
		t.Errorf("past StaleAfter: %s, want stale", st.Status)
	}

	// A new failure after the check: failed again.
	e.col(1, day(2026, 12, 20), full, "failed")
	if st := e.receita(); st.Status != freshness.Failed || !st.LastCheckedAt.Equal(at) {
		t.Errorf("new failure: %s checked=%v", st.Status, st.LastCheckedAt)
	}
}

func TestSchedulerDoesNotRetryAFailureClearedByACheck(t *testing.T) {
	e := newEnv(t, day(2026, 11, 13))
	e.freshOthers("receita")
	e.col(1, day(2026, 11, 1), full, "done")
	e.col(1, day(2026, 11, 12), full, "failed")
	if got := e.tick(&fakePub{}); !eq(got, []string{"receita"}) {
		t.Fatalf("failed, not checked since: requested %v, want receita", got)
	}
	e.advanceTo(day(2026, 11, 14))
	e.check()
	// The edition (2026-11-01) is fresh and the failure is cleared: not due.
	if got := e.tick(&fakePub{}); len(got) != 0 {
		t.Errorf("after a successful check: requested %v, want none", got)
	}
}
