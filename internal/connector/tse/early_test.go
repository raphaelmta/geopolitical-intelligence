package tse

import (
	"testing"
	"time"

	"geoint/internal/claimstest"
	"geoint/internal/collection"
)

// Tests of the early identity pass (task 3 of
// docs/performance/2026-load-optimization-design.md): the loader resolves
// its new person records after both record phases and before the claims.

// earlyRun is a schema whose collections run through the worker's path,
// with every re-point of a relationship logged.
type earlyRun struct {
	*eqSession
	cols []collection.Collection
}

func newEarlyRun(t *testing.T, noEarly bool) *earlyRun {
	t.Helper()
	s := &eqSession{e: newEnv(t)}
	if err := claimstest.WatchRepoints(t.Context(), s.e.pool); err != nil {
		t.Fatal(err)
	}
	s.p = claimstest.NewPipeline(s.e.pool, s.e.raw, eqThreshold)
	s.p.NoEarlyResolve = noEarly
	return &earlyRun{eqSession: s}
}

func (r *earlyRun) full(t *testing.T, ref time.Time, zips map[string][]byte) collection.Collection {
	t.Helper()
	col, err := r.p.Collect(t.Context(), r.e.c, r.e.store(t, ref, zips), ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatalf("collection of %s: %v", ref.Format(time.DateOnly), err)
	}
	r.cols = append(r.cols, col)
	return col
}

func (r *earlyRun) repoints(t *testing.T) int {
	t.Helper()
	n, err := claimstest.Repoints(t.Context(), r.e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *earlyRun) stale(t *testing.T) int {
	t.Helper()
	n, err := claimstest.StalePointers(t.Context(), r.e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *earlyRun) dump(t *testing.T) map[string]string {
	t.Helper()
	_, d, err := claimstest.CanonicalDump(t.Context(), r.e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// With the early pass the candidacy and donor records are merged before any
// claim cites them: the final Resolve moves none and no relationship is ever
// re-pointed. Without it (the order before task 3) Resolve moves the same
// records and re-points their relationships. Both end in the same claims.
func TestEarlyResolveLeavesNothingToRepoint(t *testing.T) {
	early := newEarlyRun(t, false)
	old := newEarlyRun(t, true)
	for _, r := range []*earlyRun{early, old} {
		r.full(t, eqRefA, fixtureZips(t, nil))
		r.full(t, eqRefB, fixtureZips(t, eqEdition2))
		if n := r.stale(t); n != 0 {
			t.Errorf("%d relationship ends differ from their record's entity", n)
		}
		if n := r.e.count(t, `SELECT count(*) FROM identity_resolve_done`); n != 2 {
			t.Errorf("%d completion markers, want 2", n)
		}
	}

	var moved int64
	for i, col := range early.cols {
		e, ok := early.p.Early[col.ID]
		if !ok {
			t.Fatalf("collection %d: the loader did not run the early pass", i+1)
		}
		moved += e.AutoMerged
		if f := early.p.Final[col.ID]; f.AutoMerged != 0 || f.AutoMergedGroups != 0 || f.ProposalsCreated != 0 {
			t.Errorf("collection %d: the final Resolve moved or proposed after the early pass: %+v", i+1, f)
		}
		o := old.p.Final[col.ID]
		if e.AutoMerged != o.AutoMerged || e.AutoMergedGroups != o.AutoMergedGroups || e.ProposalsCreated != o.ProposalsCreated {
			t.Errorf("collection %d: early pass %+v, Resolve without it %+v", i+1, e, o)
		}
	}
	if moved == 0 {
		t.Fatal("the early passes moved no record; the fixture must have records to merge")
	}
	if n := early.repoints(t); n != 0 {
		t.Errorf("%d relationships re-pointed although the records were resolved first", n)
	}

	if len(old.p.Early) != 0 {
		t.Fatal("the run without the early pass ran it")
	}
	if n := old.repoints(t); int64(n) < moved {
		t.Errorf("without the early pass %d relationships were re-pointed for %d moved records: the Resolve of a loader "+
			"that writes relationships first must re-point them", n, moved)
	}
	if d := claimstest.Diff(old.dump(t), early.dump(t)); d != "" {
		t.Errorf("claims with the early pass differ from the ones without (- without, + with):%s", d)
	}
}

// A load that dies between the two passes (here: in its first relationship
// chunk, which rolls back) leaves merged records, no relationship, a failed
// collection and no completion marker. The next collection completes it,
// and ends in the claims of the old order interrupted at the same place.
func TestCrashBetweenEarlyResolveAndRelationshipsIsCompletedByTheNext(t *testing.T) {
	early := newEarlyRun(t, false)
	early.interrupted(t, eqRefA, fixtureZips(t, nil), "relationship staging", 1)
	failed := early.e.id(t, `SELECT id FROM collection WHERE status = 'failed'`)
	res, ok := early.p.Early[failed]
	if !ok || res.AutoMerged == 0 {
		t.Fatalf("early pass of the interrupted load: ran = %v, %+v; want records moved", ok, res)
	}
	if n := early.e.count(t, `SELECT count(*) FROM relationship`); n != 0 {
		t.Fatalf("%d relationships written before the interruption", n)
	}
	if n := early.e.count(t, `SELECT count(*) FROM identity_resolve_done`); n != 0 {
		t.Fatal("the early pass marked the collection resolved")
	}
	decisions := early.e.count(t, `SELECT count(*) FROM identity_decision`)

	col := early.full(t, eqRefA, fixtureZips(t, nil))
	final := early.p.Final[col.ID]
	if final.Recovered != 1 || final.AutoMerged != 0 {
		t.Errorf("the next Resolve: %+v, want the interrupted collection recovered and no record moved", final)
	}
	if n := early.e.count(t, `SELECT count(*) FROM identity_resolve_done WHERE collection_id IN ($1, $2)`, failed, col.ID); n != 2 {
		t.Errorf("%d completion markers, want one for each collection", n)
	}
	if n := early.e.count(t, `SELECT count(*) FROM identity_decision`); n != decisions {
		t.Errorf("%d decisions after the repeat, %d before: a merge was decided twice", n, decisions)
	}
	if n := early.stale(t); n != 0 {
		t.Errorf("%d relationship ends differ from their record's entity", n)
	}
	if n := early.repoints(t); n != 0 {
		t.Errorf("%d relationships re-pointed", n)
	}

	old := newEarlyRun(t, true)
	old.interrupted(t, eqRefA, fixtureZips(t, nil), "relationship staging", 1)
	oldCol := old.full(t, eqRefA, fixtureZips(t, nil))
	if got := old.p.Final[oldCol.ID]; got.AutoMerged != res.AutoMerged || got.Recovered != 1 {
		t.Fatalf("old order: recovery %+v, want the %d records the early pass moved", got, res.AutoMerged)
	}
	if d := claimstest.Diff(old.dump(t), early.dump(t)); d != "" {
		t.Errorf("claims differ from the old order interrupted at the same place (- old, + new):%s", d)
	}
}
