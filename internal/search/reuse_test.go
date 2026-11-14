package search_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"geoint/internal/claims"
	"geoint/internal/claimstest"
	"geoint/internal/collection"
	"geoint/internal/search"
	"geoint/internal/sources"
)

// Task 5 of docs/performance/2026-load-optimization-design.md: a refresh
// that finds nothing searchable changed builds nothing; a rebuild reuses
// name_norm and tokens of the live tables while they were built with the
// current build version. Either way the live tables must be, row for row,
// what a from-scratch build gives (claimstest.CheckSearchMatchesRebuild).

// refresh is what the worker runs after a collection.
func refresh(f *fx, s *search.Service) (search.Stats, bool) {
	f.t.Helper()
	st, rebuilt, err := s.Refresh(ctx)
	if err != nil {
		f.t.Fatalf("refresh: %v", err)
	}
	if rebuilt && st.Unchanged {
		f.t.Fatal("refresh reports both rebuilt and unchanged")
	}
	return st, rebuilt
}

func storedRows(f *fx) map[string]string {
	f.t.Helper()
	rows, err := claimstest.SearchRows(ctx, f.pool)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func TestRefreshWithNothingChangedIsSkipped(t *testing.T) {
	f := setup(t)
	populate(f, 400, 11)
	s := search.New(f.pool)
	if _, rebuilt := refresh(f, s); !rebuilt {
		t.Fatal("first refresh did not build")
	}
	g := generation(f)
	before := storedRows(f)
	// A collection that only re-sees its claims: last_seen moves, nothing
	// searchable does.
	if _, err := f.pool.Exec(ctx, `UPDATE relationship SET last_seen_collection_id = first_collection_id`); err != nil {
		t.Fatal(err)
	}
	// An entity whose name normalizes to nothing is never searchable either.
	f.entity(claims.KindCompany, "94000001", "  ")
	st, rebuilt := refresh(f, s)
	if rebuilt || !st.Unchanged {
		t.Fatalf("nothing changed: rebuilt=%v unchanged=%v, want a skip", rebuilt, st.Unchanged)
	}
	if generation(f) != g {
		t.Errorf("generation %d -> %d on a skip", g, generation(f))
	}
	if dirty(f, s) {
		t.Error("still dirty after a skip")
	}
	if l := leftovers(f); len(l) != 0 {
		t.Errorf("leftover tables: %v", l)
	}
	if d := claimstest.Diff(before, storedRows(f)); d != "" {
		t.Errorf("a skip changed the live tables:%s", d)
	}
	claimstest.CheckSearchMatchesRebuild(t, ctx, f.pool)
	// RebuildIfDirty when clean does nothing at all; Rebuild never skips.
	if st, rebuilt, err := s.RebuildIfDirty(ctx); err != nil || rebuilt || st.Unchanged {
		t.Errorf("clean state: rebuilt=%v unchanged=%v err=%v", rebuilt, st.Unchanged, err)
	}
	g = generation(f)
	if st := f.rebuild(s); st.Unchanged || !st.Reused || generation(f) != g+1 {
		t.Errorf("forced rebuild: unchanged=%v reused=%v generation %d -> %d", st.Unchanged, st.Reused, g, generation(f))
	}
}

// Every kind of change to an input of the build must rebuild, and the result
// must equal a from-scratch build. The changes are made with plain SQL, as
// the owner: the check compares data, so it does not matter which code path
// wrote them, nor whether a collection was involved.
func TestRefreshAfterEachKindOfChangeRebuilds(t *testing.T) {
	cases := []struct {
		name   string
		change func(w *world)
		skip   bool // the change touches nothing searchable: a skip is right
	}{
		{name: "new searchable entity", change: func(w *world) {
			w.f.entity(claims.KindForeign, "FX-NOVA", "OUTRA FORASTEIRA")
		}},
		{name: "new entity whose name normalizes to nothing", skip: true, change: func(w *world) {
			w.f.entity(claims.KindParty, "PT-VAZIO", " ")
		}},
		{name: "relationship added between existing entities (degree only)", change: func(w *world) {
			w.f.link(w.a, w.b, 1)
		}},
		{name: "relationship added and another closed (same degrees for the hub)", change: func(w *world) {
			w.f.link(w.a, w.hub, 1)
			w.close(`src_entity_id = $1 AND dst_entity_id = $2 AND id = (SELECT max(id) FROM relationship WHERE src_entity_id = $1 AND dst_entity_id = $2)`, w.b, w.hub)
		}},
		{name: "relationship closed", change: func(w *world) {
			w.close(`src_entity_id = $1 AND dst_entity_id = $2`, w.a, w.hub)
		}},
		{name: "relationship re-pointed (identity merge of its end)", change: func(w *world) {
			w.exec(`UPDATE relationship SET src_entity_id = $2 WHERE src_entity_id = $1`, w.pb, w.pa)
		}},
		{name: "person entity emptied by a merge", change: func(w *world) {
			w.exec(`UPDATE person_record SET entity_id = $2 WHERE entity_id = $1`, w.pb, w.pa)
		}},
		{name: "person entity emptied and its relationships re-pointed", change: func(w *world) {
			w.exec(`UPDATE person_record SET entity_id = $2 WHERE entity_id = $1`, w.pb, w.pa)
			w.exec(`UPDATE relationship SET src_entity_id = $2 WHERE src_entity_id = $1`, w.pb, w.pa)
		}},
		{name: "empty person entity gets a record (split)", change: func(w *world) {
			w.exec(`INSERT INTO person_record (source_id, context_key, name, name_norm, entity_id, first_collection_id, record_ref)
				VALUES ($1, 'ctx-split', 'X', '', $2, $3, 'test:split')`, w.f.srcID, w.x, w.f.col)
		}},
		{name: "a record moves between two person entities that keep records", change: func(w *world) {
			w.exec(`INSERT INTO person_record (source_id, context_key, name, name_norm, entity_id, first_collection_id, record_ref)
				VALUES ($1, 'ctx-extra', 'X', '', $2, $3, 'test:extra')`, w.f.srcID, w.pb, w.f.col)
			w.exec(`UPDATE person_record SET entity_id = $1 WHERE context_key = 'ctx-extra'`, w.pa)
		}, skip: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			w := &world{f: f}
			w.hub = f.entity(claims.KindCompany, "95000000", "CENTRAL ALFA")
			w.a = f.entity(claims.KindCompany, "95000001", "ALFA PRIMEIRA")
			w.b = f.entity(claims.KindCompany, "95000002", "ALFA SEGUNDA")
			w.pa = f.entity(claims.KindPerson, "", "ALFA PESSOA")
			w.pb = f.entity(claims.KindPerson, "", "ALFA PESSOA")
			f.link(w.a, w.hub, 2)
			f.link(w.b, w.hub, 3)
			f.link(w.pb, w.a, 1)
			f.entity(claims.KindForeign, "FX1", "ALFA FORASTEIRA")
			populate(f, 150, 12)
			// An empty person entity, as identity merges leave them (made
			// last: f.entity and populate give every person entity a record).
			if err := f.pool.QueryRow(ctx, `INSERT INTO entity (kind, name) VALUES ('person', 'ALFA VAZIA') RETURNING id`).Scan(&w.x); err != nil {
				t.Fatal(err)
			}
			s := search.New(f.pool)
			if _, rebuilt := refresh(f, s); !rebuilt {
				t.Fatal("first refresh did not build")
			}
			if st, rebuilt := refresh(f, s); rebuilt || !st.Unchanged {
				t.Fatalf("second refresh without a change: rebuilt=%v unchanged=%v", rebuilt, st.Unchanged)
			}
			g := generation(f)
			c.change(w)
			st, rebuilt := refresh(f, s)
			if c.skip {
				if rebuilt || !st.Unchanged || generation(f) != g {
					t.Errorf("rebuilt=%v unchanged=%v generation %d -> %d, want a skip", rebuilt, st.Unchanged, g, generation(f))
				}
			} else if !rebuilt || !st.Reused || generation(f) != g+1 {
				t.Errorf("rebuilt=%v reused=%v generation %d -> %d, want a rebuild reusing the live tables",
					rebuilt, st.Reused, g, generation(f))
			}
			claimstest.CheckSearchMatchesRebuild(t, ctx, f.pool)
		})
	}
}

// world is the data set of TestRefreshAfterEachKindOfChangeRebuilds.
type world struct {
	f                    *fx
	hub, a, b, pa, pb, x int64
}

func (w *world) exec(sql string, args ...any) {
	w.f.t.Helper()
	if _, err := w.f.pool.Exec(ctx, sql, args...); err != nil {
		w.f.t.Fatal(err)
	}
}

// close closes the relationships matching where (a later collection no
// longer sees them).
func (w *world) close(where string, args ...any) {
	w.f.t.Helper()
	src, _ := sources.ByKey("receita")
	col, err := collection.Begin(ctx, w.f.pool, src.ID, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), collection.Scope{Full: true})
	if err != nil {
		w.f.t.Fatal(err)
	}
	w.exec(`UPDATE relationship SET closed_collection_id = `+strconv.FormatInt(col.ID, 10)+` WHERE `+where, args...)
}

func TestRefreshWithAnotherFrequentThresholdRebuilds(t *testing.T) {
	f := setup(t)
	populate(f, 300, 13)
	if _, rebuilt := refresh(f, search.NewForTest(f.pool, 1<<40, time.Minute)); !rebuilt {
		t.Fatal("first refresh did not build")
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM search_frequent_token`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d frequent tokens with an unreachable threshold (%v)", n, err)
	}
	s := search.NewForTest(f.pool, 20, time.Minute)
	if st, rebuilt := refresh(f, s); !rebuilt || st.FrequentTokens == 0 {
		t.Fatalf("another threshold: rebuilt=%v frequent tokens %d", rebuilt, st.FrequentTokens)
	}
	if st, rebuilt := refresh(f, s); rebuilt || !st.Unchanged {
		t.Fatalf("same threshold again: rebuilt=%v unchanged=%v, want a skip", rebuilt, st.Unchanged)
	}
}

// A live table that lost rows (or was emptied) is a difference like any other.
func TestRefreshAfterLiveTablesWereEmptiedRebuilds(t *testing.T) {
	f := setup(t)
	populate(f, 200, 14)
	s := search.New(f.pool)
	refresh(f, s)
	if _, err := f.pool.Exec(ctx, `DELETE FROM search_person WHERE search_rank IN (SELECT max(search_rank) FROM search_person)`); err != nil {
		t.Fatal(err)
	}
	if _, rebuilt := refresh(f, s); !rebuilt {
		t.Fatal("a live table missing a row was not rebuilt")
	}
	claimstest.CheckSearchMatchesRebuild(t, ctx, f.pool)
}

// corrupt overwrites the stored name_norm and tokens of one live row: only a
// rebuild that reuses the live tables carries the bogus values over, which
// makes reuse (and its absence) observable.
func corrupt(f *fx, entityID int64) {
	f.t.Helper()
	if _, err := f.pool.Exec(ctx, `UPDATE search_company SET name_norm = 'ZZ REUSED', tokens = 'zzreused'::tsvector
		WHERE entity_id = $1`, entityID); err != nil {
		f.t.Fatal(err)
	}
}

func liveName(f *fx, entityID int64) string {
	f.t.Helper()
	var n string
	if err := f.pool.QueryRow(ctx, `SELECT name_norm || ' / ' || tokens::text FROM search_company WHERE entity_id = $1`,
		entityID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestRebuildReusesOnlyWithTheSameBuildVersion(t *testing.T) {
	f := setup(t)
	id := f.entity(claims.KindCompany, "96000001", "ÔMEGA REUSO LTDA")
	populate(f, 200, 15)
	s := search.New(f.pool)
	if st := f.rebuild(s); st.Reused {
		t.Fatal("the first build after the migration reused (no build version stored yet)")
	}
	const fresh = "OMEGA REUSO LTDA / 'omega' 'reuso'"
	if got := liveName(f, id); got != fresh {
		t.Fatalf("live row %q, want %q", got, fresh)
	}

	// Same version: the values are taken from the live tables.
	corrupt(f, id)
	if st := f.rebuild(s); !st.Reused {
		t.Fatal("rebuild with the same build version did not reuse")
	}
	if got := liveName(f, id); got != "ZZ REUSED / 'zzreused'" {
		t.Fatalf("live row %q: the values were not taken from the live tables", got)
	}

	// Another stored version (the code of the build statements changed):
	// everything is computed again.
	if _, err := f.pool.Exec(ctx, `UPDATE search_state SET build_version = 'an older build'`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDirty(ctx); err != nil {
		t.Fatal(err)
	}
	st, rebuilt, err := s.RebuildIfDirty(ctx)
	if err != nil || !rebuilt || st.Reused || st.Unchanged {
		t.Fatalf("other build version: rebuilt=%v reused=%v unchanged=%v err=%v, want a full rebuild",
			rebuilt, st.Reused, st.Unchanged, err)
	}
	if got := liveName(f, id); got != fresh {
		t.Fatalf("live row %q after a full rebuild, want %q", got, fresh)
	}
	claimstest.CheckSearchMatchesRebuild(t, ctx, f.pool)
}

// A migration that replaces one of the normalisation functions changes the
// build version by itself: the next refresh is a full rebuild, not a skip,
// and its result follows the new definition.
func TestChangedSearchFunctionForcesAFullRebuild(t *testing.T) {
	changes := []struct {
		name, ddl string
		id        func(f *fx) int64
		want      string
	}{
		{name: "search_words", ddl: `CREATE OR REPLACE FUNCTION search_words(name_norm text)
			RETURNS TABLE (pos bigint, word text, searched boolean)
			LANGUAGE sql IMMUTABLE PARALLEL SAFE
			AS $$
			  SELECT u.i, u.w,
			    u.w NOT IN ('de', 'da', 'do', 'das', 'dos', 'e', 'a', 'o', 'em', 'ltda', 'me', 'epp', 'eireli', 's', 'sa', 'reuso')
			    AND u.w !~ '[0-9]{5,}'
			    AND NOT (d.n >= 9 AND u.w ~ '[0-9]')
			  FROM (SELECT length(regexp_replace(name_norm COLLATE "C", '[^0-9]+', '', 'g')) AS n) AS d,
			    unnest(array_remove(regexp_split_to_array(lower(name_norm COLLATE "C"), '[^a-z0-9]+'), ''))
			      WITH ORDINALITY AS u(w, i)
			$$`, want: "OMEGA REUSO LTDA / 'omega'"},
		{name: "search_norm", ddl: `CREATE OR REPLACE FUNCTION search_norm(t text) RETURNS text
			LANGUAGE sql IMMUTABLE PARALLEL SAFE
			RETURN nullif(btrim(regexp_replace(upper(public.unaccent(
			         'public.unaccent'::regdictionary, replace(coalesce(t, ''), 'LTDA', ''))),
			         '\s+', ' ', 'g')), '')`, want: "OMEGA REUSO / 'omega' 'reuso'"},
		{name: "search_tokens", ddl: `CREATE OR REPLACE FUNCTION search_tokens(name_norm text) RETURNS tsvector
			LANGUAGE sql IMMUTABLE PARALLEL SAFE
			RETURN array_to_tsvector(ARRAY(SELECT DISTINCT word FROM search_words(name_norm) WHERE searched AND word <> 'omega'))`,
			want: "OMEGA REUSO LTDA / 'reuso'"},
	}
	for _, c := range changes {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			id := f.entity(claims.KindCompany, "96000001", "ÔMEGA REUSO LTDA")
			populate(f, 100, 16)
			s := search.New(f.pool)
			refresh(f, s)
			if st, rebuilt := refresh(f, s); rebuilt || !st.Unchanged {
				t.Fatalf("no change: rebuilt=%v unchanged=%v, want a skip", rebuilt, st.Unchanged)
			}
			corrupt(f, id) // would survive a reuse
			if _, err := f.pool.Exec(ctx, c.ddl); err != nil {
				t.Fatal(err)
			}
			st, rebuilt := refresh(f, s)
			if !rebuilt || st.Reused {
				t.Fatalf("after replacing %s: rebuilt=%v reused=%v, want a full rebuild", c.name, rebuilt, st.Reused)
			}
			if got := liveName(f, id); got != c.want {
				t.Errorf("live row %q, want %q", got, c.want)
			}
			if st, rebuilt := refresh(f, s); rebuilt || !st.Unchanged {
				t.Errorf("the refresh after the full one: rebuilt=%v unchanged=%v, want a skip", rebuilt, st.Unchanged)
			}
			claimstest.CheckSearchMatchesRebuild(t, ctx, f.pool)
		})
	}
}

// A function replaced while a rebuild is running (a migration meanwhile):
// the tables built are not swapped in, the state stays dirty and the next
// rebuild computes everything with the new definition.
func TestSearchFunctionChangedDuringTheBuildIsNotSwappedIn(t *testing.T) {
	f := setup(t)
	id := f.entity(claims.KindCompany, "96000001", "ÔMEGA REUSO LTDA")
	populate(f, 100, 17)
	f.rebuild(search.New(f.pool))
	f.entity(claims.KindCompany, "96000002", "ÔMEGA NOVA") // something to rebuild
	g := generation(f)
	s := search.New(f.pool)
	s.SetBeforeSwap(func(int) {
		if _, err := f.pool.Exec(ctx, `CREATE OR REPLACE FUNCTION search_tokens(name_norm text) RETURNS tsvector
			LANGUAGE sql IMMUTABLE PARALLEL SAFE
			RETURN array_to_tsvector(ARRAY(SELECT DISTINCT word FROM search_words(name_norm) WHERE searched AND word <> 'omega'))`); err != nil {
			t.Error(err)
		}
	})
	if _, _, err := s.Refresh(ctx); err == nil {
		t.Fatal("tables built with the old definition were swapped in")
	}
	if generation(f) != g || !dirty(f, s) {
		t.Fatalf("generation %d -> %d, dirty=%v", g, generation(f), dirty(f, s))
	}
	if l := leftovers(f); len(l) != 0 {
		t.Errorf("leftover tables: %v", l)
	}
	st, rebuilt, err := search.New(f.pool).RebuildIfDirty(ctx)
	if err != nil || !rebuilt || st.Reused {
		t.Fatalf("next rebuild: rebuilt=%v reused=%v err=%v", rebuilt, st.Reused, err)
	}
	if got := liveName(f, id); got != "OMEGA REUSO LTDA / 'reuso'" {
		t.Errorf("live row %q", got)
	}
	claimstest.CheckSearchMatchesRebuild(t, ctx, f.pool)
}

// A crash in the middle of a build that would reuse: the old tables keep
// serving, unchanged, the state stays dirty, and the next rebuild is exact.
func TestInterruptedReusingRebuildLeavesTheOldTables(t *testing.T) {
	f := setup(t)
	populate(f, 300, 18)
	f.rebuild(search.New(f.pool))
	before := storedRows(f)
	added := f.entity(claims.KindCompany, "97000001", "SIRIUS REUSO")
	cctx, cancel := context.WithCancel(ctx)
	s := search.New(f.pool)
	s.SetBeforeSwap(func(int) { cancel() })
	if _, _, err := s.Refresh(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted refresh: %v", err)
	}
	if d := claimstest.Diff(before, storedRows(f)); d != "" {
		t.Errorf("the old tables changed:%s", d)
	}
	if !dirty(f, s) {
		t.Fatal("not dirty after an interrupted rebuild")
	}
	st, rebuilt, err := search.New(f.pool).RebuildIfDirty(ctx)
	if err != nil || !rebuilt || !st.Reused {
		t.Fatalf("catch-up: rebuilt=%v reused=%v err=%v", rebuilt, st.Reused, err)
	}
	if got := ids(f.query(s, "sirius reuso", "", 20)); !slices.Equal(got, []int64{added}) {
		t.Errorf("after catch-up: %v", got)
	}
	claimstest.CheckSearchMatchesRebuild(t, ctx, f.pool)
}

// A failed collection never refreshes search, but it counts as finished, so
// the next refresh is dirty and judges the data its writes left.
func TestChangesOfAFailedCollectionAreNotSkipped(t *testing.T) {
	f := setup(t)
	a := f.entity(claims.KindPerson, "", "HELENA FALHA")
	b := f.entity(claims.KindPerson, "", "HELENA FALHA")
	s := search.New(f.pool)
	refresh(f, s)
	src, _ := sources.ByKey("receita")
	failed, err := collection.Begin(ctx, f.pool, src.ID, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	// Its early identity pass merged b into a, then it failed.
	if _, err := f.pool.Exec(ctx, `UPDATE person_record SET entity_id = $1 WHERE entity_id = $2`, a, b); err != nil {
		t.Fatal(err)
	}
	if err := collection.Fail(ctx, f.pool, failed.ID, errors.New("failed after the early pass")); err != nil {
		t.Fatal(err)
	}
	// The next collection changes nothing itself.
	if !dirty(f, s) {
		t.Fatal("not dirty after a failed collection")
	}
	if _, rebuilt := refresh(f, s); !rebuilt {
		t.Fatal("the merge of the failed collection was skipped")
	}
	if got := ids(f.query(s, "helena falha", "", 20)); !slices.Equal(got, []int64{a}) {
		t.Errorf("after the refresh: %v, want [%d]", got, a)
	}
	claimstest.CheckSearchMatchesRebuild(t, ctx, f.pool)
}
