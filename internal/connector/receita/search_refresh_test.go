package receita

import (
	"testing"
	"time"

	"geoint/internal/claimstest"
	"geoint/internal/collection"
	"geoint/internal/sources"
)

// The search refresh after every kind of event the worker meets (task 5 of
// docs/performance/2026-load-optimization-design.md): after each one the live
// search tables must be, row for row, what a from-scratch build gives, and a
// collection that changed nothing searchable must have been skipped.
//
// Reference dates are in 2044, used by no other test (the raw store refuses
// different bytes under a month it holds): April eqSearchFailed, September
// and November eqEdition2, every other month eqBase.
func TestSearchRefreshAfterEveryEvent(t *testing.T) {
	month := func(m time.Month) time.Time { return time.Date(2044, m, 1, 0, 0, 0, 0, time.UTC) }
	s := &eqSession{e: newEnv(t)}
	s.p = claimstest.NewPipeline(s.e.pool, s.e.raw, eqThreshold)
	generation := func() int {
		return s.e.count(t, `SELECT generation FROM search_state`)
	}
	// collect runs a collection through the worker's path and checks what its
	// search refresh did.
	collect := func(t *testing.T, ref time.Time, zips map[string][]byte, wantSkip bool) {
		t.Helper()
		g := generation()
		col, err := s.p.Collect(t.Context(), s.e.c, s.e.store(t, ref, zips), ref, full)
		if err != nil {
			t.Fatalf("collection of %s: %v", ref.Format(time.DateOnly), err)
		}
		if s.p.SearchErr != nil {
			t.Fatal(s.p.SearchErr)
		}
		r, ok := s.p.Refreshes[col.ID]
		switch {
		case !ok:
			t.Fatal("no search refresh after the collection")
		case wantSkip && (r.Rebuilt || !r.Stats.Unchanged || generation() != g):
			t.Fatalf("rebuilt=%v unchanged=%v generation %d -> %d, want a skip", r.Rebuilt, r.Stats.Unchanged, g, generation())
		case !wantSkip && (!r.Rebuilt || generation() != g+1):
			t.Fatalf("rebuilt=%v unchanged=%v generation %d -> %d, want a rebuild", r.Rebuilt, r.Stats.Unchanged, g, generation())
		}
	}
	matches := func(t *testing.T) {
		t.Helper()
		claimstest.CheckSearchMatchesRebuild(t, t.Context(), s.e.pool)
	}
	// Person entities in the live search tables that no longer have a record.
	emptiedIndexed := func() int {
		return s.e.count(t, `SELECT count(*) FROM search_person sp
			WHERE NOT EXISTS (SELECT 1 FROM person_record p WHERE p.entity_id = sp.entity_id)`)
	}

	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"first load", func(t *testing.T) {
			collect(t, month(1), eqBase(t), false)
			matches(t)
		}},
		{"re-load of the same edition changes nothing", func(t *testing.T) {
			collect(t, month(2), eqBase(t), true)
			matches(t)
		}},
		{"analyst Decide (confirm) between collections", func(t *testing.T) {
			if err := s.p.Resolver.Decide(t.Context(), s.proposal(t, "|700002"), true, "analyst-b"); err != nil {
				t.Fatal(err)
			}
			collect(t, month(3), eqBase(t), false)
			matches(t)
		}},
		{"a collection fails after its early identity pass, then one that changes nothing", func(t *testing.T) {
			// A worker with a higher threshold: the early pass merges the
			// group of CARLA|700001, whose two records the first load had
			// left in two indexed entities, and the load then fails.
			failing := &eqSession{e: s.e, p: claimstest.NewPipeline(s.e.pool, s.e.raw, 50)}
			failing.interrupted(t, month(4), fixtureZips(t, eqSearchFailed), func(p string) bool { return p == "early resolve" })
			if n := emptiedIndexed(); n == 0 {
				t.Fatal("the failed early pass emptied no indexed person entity: the step tests nothing")
			}
			collect(t, month(5), eqBase(t), false)
			if n := emptiedIndexed(); n != 0 {
				t.Errorf("%d emptied person entities still indexed", n)
			}
			matches(t)
		}},
		{"analyst Merge between collections", func(t *testing.T) {
			if _, err := s.p.Resolver.Merge(t.Context(), []int64{
				s.record(t, "98990028", "DEBORA", "161718"), s.record(t, "98990027", "DEBORA", ""),
			}, "analyst-a", "same partner (test)"); err != nil {
				t.Fatal(err)
			}
			if n := emptiedIndexed(); n == 0 {
				t.Fatal("the merge emptied no indexed person entity")
			}
			collect(t, month(6), eqBase(t), false)
			if n := emptiedIndexed(); n != 0 {
				t.Errorf("%d emptied person entities still indexed", n)
			}
			matches(t)
		}},
		{"analyst Split between collections", func(t *testing.T) {
			if _, err := s.p.Resolver.Split(t.Context(), []int64{s.record(t, "98990005", "JOSE", "123456")}, "analyst-a",
				"different person (test)"); err != nil {
				t.Fatal(err)
			}
			collect(t, month(7), eqBase(t), false)
			matches(t)
		}},
		{"nothing changed again", func(t *testing.T) {
			collect(t, month(8), eqBase(t), true)
			matches(t)
		}},
		{"re-load with changes", func(t *testing.T) {
			collect(t, month(9), fixtureZips(t, eqEdition2), false)
			if r := s.p.Refreshes[lastCollection(t, s)]; !r.Stats.Reused {
				t.Error("the rebuild did not reuse the live tables")
			}
			matches(t)
		}},
		{"a collection of another source only adds relationships", func(t *testing.T) {
			// Degrees change, no name, no entity: a TSE-like collection that
			// links existing entities.
			tse, _ := sources.ByKey("tse")
			col, err := collection.Begin(t.Context(), s.e.pool, tse.ID, month(1), full)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.e.pool.Exec(t.Context(), `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, attrs, kind,
					first_collection_id, record_ref)
				SELECT 'partner_of', e.id, (SELECT min(id) FROM entity WHERE kind = 'company'), jsonb_build_object('role', 'test'),
					'fact', $1, 'test:other-source:' || e.id
				FROM entity e WHERE e.kind = 'company' AND e.id > (SELECT min(id) FROM entity WHERE kind = 'company')`, col.ID); err != nil {
				t.Fatal(err)
			}
			if err := collection.Finish(t.Context(), s.e.pool, col.ID, nil); err != nil {
				t.Fatal(err)
			}
			g := generation()
			st, rebuilt, err := s.p.Search.RefreshCollection(t.Context(), col.ID)
			if err != nil || !rebuilt || generation() != g+1 {
				t.Fatalf("rebuilt=%v unchanged=%v err=%v, want a rebuild", rebuilt, st.Unchanged, err)
			}
			matches(t)
		}},
		{"the next edition, unchanged, is skipped", func(t *testing.T) {
			collect(t, month(11), fixtureZips(t, eqEdition2), true)
			matches(t)
		}},
	}
	for _, st := range steps {
		if !t.Run(st.name, st.run) {
			return // each step builds on the previous ones
		}
	}
}

// eqSearchFailed is eqBase with a third record of CARLA|700001 (in a company
// of the fixture), the edition whose load fails after its early pass in
// TestSearchRefreshAfterEveryEvent.
var eqSearchFailed = chain(eqIdentityRows, appendLines("Socios.csv",
	socio("98990011", "CARLA REPETIDA FICTÍCIA", "700001", "22", "20310115")))

func lastCollection(t *testing.T, s *eqSession) int64 {
	t.Helper()
	return s.e.id(t, `SELECT max(id) FROM collection WHERE status = 'done'`)
}
