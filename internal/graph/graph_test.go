package graph_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/graph"
	"geoint/internal/testutil"
)

// otherEnd returns the entity at the other end of e from id.
func otherEnd(e graph.Edge, id int64) int64 {
	if e.Src.ID == id {
		return e.Dst.ID
	}
	return e.Src.ID
}

func neighborIDs(edges []graph.Edge, id int64) []int64 {
	var out []int64
	for _, e := range edges {
		out = append(out, otherEnd(e, id))
	}
	slices.Sort(out)
	return out
}

func sorted(v ...int64) []int64 {
	out := slices.Clone(v)
	slices.Sort(out)
	return out
}

func mustNeighbors(t *testing.T, g *graph.Graph, id int64, o graph.Options) []graph.Edge {
	t.Helper()
	edges, next, err := g.Neighbors(ctx, id, o, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("unexpected next page %q", next)
	}
	return edges
}

func TestNeighborsReturnsOpenEdgesWithProvenance(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	edges := mustNeighbors(t, graph.New(p), ids.A, graph.Options{})
	if got, want := neighborIDs(edges, ids.A), sorted(ids.H, ids.C, ids.W, ids.F, ids.X); !slices.Equal(got, want) {
		t.Fatalf("neighbors of A = %v, want %v", got, want)
	}
	for _, e := range edges {
		if e.CollectionID == 0 || e.SourceKey != "receita" || e.Kind != claims.Fact || e.Type != claims.PartnerOf {
			t.Errorf("edge %+v: missing provenance or kind", e)
		}
		if e.Stale {
			t.Errorf("edge %d: Receita is fresh, got Stale", e.RelationshipID)
		}
		if e.Src.Name == "" || e.Dst.Name == "" || e.Src.Kind == "" {
			t.Errorf("edge %d: endpoints not filled: %+v", e.RelationshipID, e)
		}
		if e.RelationshipID == ids.RelHA && (e.ValidFrom == nil || e.Attrs["role"] != "22") {
			t.Errorf("H→A: valid_from or attrs missing: %+v", e)
		}
	}
}

func TestNeighborsIncludeClosedShowsInterval(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	edges := mustNeighbors(t, graph.New(p), ids.A, graph.Options{IncludeClosed: true})
	if got, want := neighborIDs(edges, ids.A), sorted(ids.H, ids.C, ids.W, ids.F, ids.X, ids.P3); !slices.Equal(got, want) {
		t.Fatalf("neighbors of A = %v, want %v", got, want)
	}
	for _, e := range edges {
		if e.RelationshipID != ids.RelP3A {
			continue
		}
		if e.ValidToBefore == nil || e.ValidToAfter == nil || !e.ValidToBefore.Equal(*date(2025, 7, 1)) {
			t.Fatalf("P3→A: closing interval missing: %+v", e)
		}
		return
	}
	t.Fatal("P3→A not returned")
}

func TestNeighborsClosedOnly(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	edges := mustNeighbors(t, graph.New(p), ids.A, graph.Options{ClosedOnly: true})
	if len(edges) != 1 || edges[0].RelationshipID != ids.RelP3A {
		t.Fatalf("closed edges of A = %+v, want only P3→A", edges)
	}
	if edges := mustNeighbors(t, graph.New(p), ids.H, graph.Options{ClosedOnly: true}); len(edges) != 0 {
		t.Fatalf("closed edges of H = %+v, want none", edges)
	}
}

func TestNeighborsAtDateFiltersByValidity(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	// 2015: P3 was a partner (closed later), H was not yet (valid_from 2020).
	edges := mustNeighbors(t, g, ids.A, graph.Options{At: date(2015, 1, 1)})
	if got, want := neighborIDs(edges, ids.A), sorted(ids.C, ids.W, ids.F, ids.X, ids.P3); !slices.Equal(got, want) {
		t.Fatalf("neighbors of A in 2015 = %v, want %v", got, want)
	}
	// After the closing interval P3 is gone and H is there.
	edges = mustNeighbors(t, g, ids.A, graph.Options{At: date(2026, 1, 1)})
	if got, want := neighborIDs(edges, ids.A), sorted(ids.H, ids.C, ids.W, ids.F, ids.X); !slices.Equal(got, want) {
		t.Fatalf("neighbors of A in 2026 = %v, want %v", got, want)
	}
	// Inside the closing interval the closed row is still shown.
	edges = mustNeighbors(t, g, ids.A, graph.Options{At: date(2025, 6, 15)})
	if !slices.Contains(neighborIDs(edges, ids.A), ids.P3) {
		t.Fatal("P3 must still show inside its closing interval")
	}
}

func TestNeighborsFiltersByRelType(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	edges := mustNeighbors(t, graph.New(p), ids.C, graph.Options{RelTypes: []claims.RelType{claims.AdministratorOf}})
	if len(edges) != 1 || edges[0].RelationshipID != ids.RelADMC {
		t.Fatalf("administrator edges of C = %+v", edges)
	}
}

func TestNeighborsCursorPagesEveryEdgeOnce(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	seen := map[int64]int{}
	cursor, pages := "", 0
	for {
		edges, next, err := g.Neighbors(ctx, ids.PT1, graph.Options{}, cursor, 250)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(edges) > 250 {
			t.Fatalf("page of %d edges, limit 250", len(edges))
		}
		for _, e := range edges {
			seen[e.RelationshipID]++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if pages != 3 || len(seen) != 601 {
		t.Fatalf("pages=%d distinct edges=%d, want 3 and 601", pages, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("edge %d returned %d times", id, n)
		}
	}
}

func TestNeighborsLimitDefaultAndMaximum(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	edges, next, err := g.Neighbors(ctx, ids.PT1, graph.Options{}, "", 0)
	if err != nil || len(edges) != 200 || next == "" {
		t.Fatalf("default page: %d edges, next %q, err %v", len(edges), next, err)
	}
	edges, _, err = g.Neighbors(ctx, ids.PT1, graph.Options{}, "", 5000)
	if err != nil || len(edges) != 601 {
		t.Fatalf("capped page: %d edges, err %v", len(edges), err)
	}
}

func TestNeighborsBadCursor(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	for _, c := range []string{"not base64!", "bm90LWEtbnVtYmVy", "LTU"} { // "not-a-number", "-5"
		if _, _, err := g.Neighbors(ctx, ids.A, graph.Options{}, c, 10); !errors.Is(err, graph.ErrBadCursor) {
			t.Errorf("cursor %q: err = %v, want ErrBadCursor", c, err)
		}
	}
}

func TestNeighborsStale(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	edges := mustNeighbors(t, graph.New(p), ids.P2, graph.Options{})
	if len(edges) != 2 {
		t.Fatalf("P2 edges = %d, want 2", len(edges))
	}
	for _, e := range edges {
		switch e.RelationshipID {
		case ids.RelP2POL:
			if !e.Stale || e.SourceKey != "tse" || e.CollectionID != ids.TSECol {
				t.Errorf("donation (TSE of 2020) must be stale: %+v", e)
			}
		case ids.RelP2C:
			if e.Stale || e.CollectionID != ids.ReceitaCol {
				t.Errorf("partnership (fresh Receita) must not be stale: %+v", e)
			}
		}
		if e.IdentityConfidence == nil || *e.IdentityConfidence != 0.99 {
			t.Errorf("edge %d: identity confidence = %v, want 0.99", e.RelationshipID, e.IdentityConfidence)
		}
	}
}

func TestSourceWithoutDoneCollectionIsStale(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	a := b.Company(rc, "91000001", "EMPRESA FICTICIA SOLO LTDA", "2062")
	// A Câmara collection that never finished: the source has no done collection.
	running, err := collection.Begin(ctx, p, 3, time.Now().UTC(), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	m := b.Entity(claims.KindPerson, "deputy:1", "DEPUTADO FICTICIO")
	b.Rel(running.ID, claims.HoldsMandate, ent(m), ent(a), nil, nil)
	edges := mustNeighbors(t, graph.New(p), a, graph.Options{})
	if len(edges) != 1 || !edges[0].Stale || edges[0].IdentityConfidence != nil {
		t.Fatalf("edge from a source with no done collection: %+v", edges)
	}
}

func TestUnknownEntityIsNotFound(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	const missing = 999_999_999
	if _, _, err := g.Neighbors(ctx, missing, graph.Options{}, "", 0); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("Neighbors: %v", err)
	}
	if _, err := g.Paths(ctx, ids.A, missing, graph.PathOptions{}, nil); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("Paths (b): %v", err)
	}
	if _, err := g.Paths(ctx, missing, ids.A, graph.PathOptions{}, nil); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("Paths (a): %v", err)
	}
	if _, err := g.ControlGroup(ctx, missing, graph.ControlOptions{Mode: graph.Documented}, nil); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("ControlGroup: %v", err)
	}
}

func TestSliceCollectionDoesNotRefreshSource(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	a := b.Company(rc, "98000001", "EMPRESA FICTICIA CVM SA", "2054")
	top := b.Company(rc, "98000002", "CONTROLADORA FICTICIA CVM SA", "2054")
	slice, err := collection.Begin(ctx, p, 5, time.Now().UTC(), collection.Scope{CNPJs: []string{"98000001"}, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Finish(ctx, p, slice.ID, nil); err != nil {
		t.Fatal(err)
	}
	b.Rel(slice.ID, claims.Controls, ent(top), ent(a), nil, nil)
	edges := mustNeighbors(t, graph.New(p), a, graph.Options{})
	if len(edges) != 1 || !edges[0].Stale || edges[0].SourceKey != "cvm" {
		t.Fatalf("CVM with only a done slice must be stale: %+v", edges)
	}
}
