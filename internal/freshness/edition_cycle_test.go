package freshness_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/freshness"
	"geoint/internal/rawstore"
)

// editionSource is a fake connector of a source published as monthly
// editions; published is the newest edition the "server" has.
type editionSource struct {
	published time.Time
	loads     []time.Time
	failNext  bool // the next listing fails (server error)
}

func (s *editionSource) Key() string { return "receita" }

func (s *editionSource) ResolveReference(_ context.Context, requested time.Time, _ *pgxpool.Pool) (connector.Edition, error) {
	if s.failNext {
		s.failNext = false
		return connector.Edition{}, errors.New("list editions: unexpected status 503")
	}
	if s.published.IsZero() || s.published.After(requested) {
		return connector.Edition{}, connector.ErrNoEdition
	}
	// Published on its first day: the observed date is the reference date.
	return connector.Edition{ReferenceDate: s.published, ObservedDate: s.published}, nil
}

func (s *editionSource) Fetch(context.Context, time.Time, rawstore.Store) ([]rawstore.File, error) {
	return nil, nil
}

func (s *editionSource) Load(_ context.Context, col collection.Collection, _ []rawstore.File, _ rawstore.Store, _ *pgxpool.Pool) (connector.Stats, error) {
	s.loads = append(s.loads, col.ReferenceDate)
	return connector.Stats{"rows_read": 1, "rows_skipped": 0}, nil
}

// TestScheduledEditionCycle follows the scheduler and the runner through a
// month: edition A is loaded; while A stays the newest, daily requests find
// nothing new without creating collections or a failed status; once B is
// published the next request loads it, dated B.
func TestScheduledEditionCycle(t *testing.T) {
	a := day(2026, 10, 1)
	b := day(2026, 11, 1)
	e := newEnv(t, day(2026, 10, 14))
	e.freshOthers("receita")
	src := &editionSource{published: a}
	finished := 0
	hooks := connector.Hooks{Finished: func(context.Context, int64) { finished++ }}

	// process ticks and runs every published request, as the worker does.
	failedRuns := 0
	process := func() (requested int, nothingNew int) {
		t.Helper()
		pub := &fakePub{}
		e.tick(pub)
		for _, r := range pub.reqs {
			if r.Source != "receita" {
				continue
			}
			requested++
			_, err := connector.Collect(bg, src, r, e.p, nil, hooks)
			var runErr *connector.RunError
			switch {
			case errors.Is(err, connector.ErrNothingNew):
				nothingNew++
			case errors.As(err, &runErr):
				failedRuns++
			case err != nil:
				t.Fatalf("collect: %v", err)
			}
		}
		return requested, nothingNew
	}
	collections := func() int {
		var n int
		if err := e.p.QueryRow(bg, `SELECT count(*) FROM collection WHERE source_id = 1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	status := func() freshness.SourceStatus {
		t.Helper()
		all, err := e.svc.List(bg)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range all {
			if s.Key == "receita" {
				return s
			}
		}
		t.Fatal("receita not listed")
		return freshness.SourceStatus{}
	}

	// Never collected: the scheduler does not make the first collection.
	if req, _ := process(); req != 0 || len(src.loads) != 0 {
		t.Fatalf("first tick: requested=%d loads=%v, want nothing", req, src.loads)
	}
	// The owner asks on 2026-10-14; it loads edition A dated A.
	first := connector.Request{Source: "receita", ReferenceDate: e.now, Scope: full}
	if _, err := connector.Collect(bg, src, first, e.p, nil, hooks); err != nil || len(src.loads) != 1 || !src.loads[0].Equal(a) {
		t.Fatalf("first explicit collection: %v, loads=%v", err, src.loads)
	}
	if st := status(); st.Status != freshness.Fresh || !st.LastReferenceDate.Equal(a) {
		t.Errorf("after A: %s %v", st.Status, st.LastReferenceDate)
	}

	// Not due before A + 30 days.
	e.advanceTo(day(2026, 10, 30))
	if req, _ := process(); req != 0 {
		t.Errorf("requested %d times before A + PublishEvery", req)
	}

	// Due from 2026-10-31; one request per day finds nothing new, except on
	// 2026-11-05 when the listing fails: that day the source is failed, and
	// the next day's successful check clears it.
	failDay := day(2026, 11, 5)
	for d := 31; d <= 31+16; d++ {
		e.advanceTo(a.AddDate(0, 0, d-1)) // 2026-10-31 .. 2026-11-16
		src.failNext = e.now.Equal(failDay)
		req, none := process()
		switch {
		case e.now.Equal(failDay):
			if req != 1 || none != 0 || failedRuns != 1 {
				t.Fatalf("%s: requested=%d nothing new=%d failed=%d", e.now.Format(time.DateOnly), req, none, failedRuns)
			}
			if st := status(); st.Status != freshness.Failed || st.LastError == "" {
				t.Errorf("after the failed check: %s %q", st.Status, st.LastError)
			}
		default:
			if req != 1 || none != 1 {
				t.Fatalf("%s: requested=%d nothing new=%d, want 1 and 1", e.now.Format(time.DateOnly), req, none)
			}
			if st := status(); st.Status == freshness.Failed || st.LastCheckedAt == nil || !st.LastReferenceDate.Equal(a) {
				t.Errorf("%s: status %s checked=%v ref=%v; a successful check is not failed",
					e.now.Format(time.DateOnly), st.Status, st.LastCheckedAt, st.LastReferenceDate)
			}
		}
	}
	// One done collection (A) and the failed attempt; nothing new creates nothing.
	if n := collections(); n != 2 || len(src.loads) != 1 || finished != 1 {
		t.Errorf("collections=%d loads=%d finished=%d, want 2, 1, 1", n, len(src.loads), finished)
	}
	// Past A + StaleAfter (45 days) with no new edition: stale, not failed.
	if st := status(); st.Status != freshness.Stale || !st.LastReferenceDate.Equal(a) {
		t.Errorf("while A is the newest past 45 days: %s %v", st.Status, st.LastReferenceDate)
	}

	// Edition B is published; the next daily request loads it, dated B.
	src.published = b
	e.advanceTo(day(2026, 11, 17))
	if req, none := process(); req != 1 || none != 0 {
		t.Fatalf("after B: requested=%d nothing new=%d", req, none)
	}
	if len(src.loads) != 2 || !src.loads[1].Equal(b) {
		t.Fatalf("loads = %v, want B second", src.loads)
	}
	if st := status(); st.Status != freshness.Fresh || !st.LastReferenceDate.Equal(b) {
		t.Errorf("after B: %s %v", st.Status, st.LastReferenceDate)
	}
}
