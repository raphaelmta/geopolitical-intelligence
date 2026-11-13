package graph_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"geoint/internal/claims"
	"geoint/internal/graph"
	"geoint/internal/testutil"
)

// entities returns the entity sequence a path walks, starting at from.
func entities(path []graph.Hop, from int64) []int64 {
	out := []int64{from}
	for _, h := range path {
		cur := out[len(out)-1]
		want := h.Edge.Src.ID
		if h.Forward {
			want = h.Edge.Dst.ID
		}
		if (h.Forward && h.Edge.Src.ID != cur) || (!h.Forward && h.Edge.Dst.ID != cur) {
			return nil // the hop does not start where the previous one ended
		}
		out = append(out, want)
	}
	return out
}

func TestPathsFindsPoliticianToCompany(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	var progress []float64
	res, err := graph.New(p).Paths(ctx, ids.POL, ids.H, graph.PathOptions{}, func(f float64) { progress = append(progress, f) })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 1 || res.Truncated {
		t.Fatalf("paths = %d (truncated %v), want 1", len(res.Paths), res.Truncated)
	}
	path := res.Paths[0]
	if got, want := entities(path, ids.POL), []int64{ids.POL, ids.P2, ids.C, ids.A, ids.H}; !slices.Equal(got, want) {
		t.Fatalf("path walks %v, want POL←P2→C←A←H %v", got, want)
	}
	if forward := []bool{path[0].Forward, path[1].Forward, path[2].Forward, path[3].Forward}; !slices.Equal(forward, []bool{false, true, false, false}) {
		t.Errorf("directions = %v", forward)
	}
	for _, h := range path {
		if h.Edge.Kind == "" || h.Edge.CollectionID == 0 {
			t.Errorf("hop edge not filled: %+v", h.Edge)
		}
	}
	if len(progress) == 0 || progress[len(progress)-1] != 1 || !slices.IsSorted(progress) {
		t.Errorf("progress = %v, want increasing and ending at 1", progress)
	}
}

func TestPathsDoesNotExpandHubAndReportsIt(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	res, err := graph.New(p).Paths(ctx, ids.POL, ids.Candidates[0], graph.PathOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 0 {
		t.Fatalf("found %d paths through the hub", len(res.Paths))
	}
	if len(res.StoppedHubs) != 1 || res.StoppedHubs[0].Entity.ID != ids.PT1 || res.StoppedHubs[0].Degree != 601 {
		t.Fatalf("stopped hubs = %+v, want PT1 with degree 601", res.StoppedHubs)
	}
	if res.StoppedHubs[0].Entity.Kind != claims.KindParty || res.StoppedHubs[0].Entity.Name == "" {
		t.Errorf("hub entity not filled: %+v", res.StoppedHubs[0].Entity)
	}
	// A higher HubDegree lets the search through the party.
	res, err = graph.New(p).Paths(ctx, ids.POL, ids.Candidates[0], graph.PathOptions{HubDegree: 1000}, nil)
	if err != nil || len(res.Paths) != 1 || len(res.Paths[0]) != 2 {
		t.Fatalf("with HubDegree 1000: %+v, %v", res, err)
	}
}

func TestPathsHubEndpointIsExpanded(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	res, err := graph.New(p).Paths(ctx, ids.PT1, ids.P2, graph.PathOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 1 || len(res.Paths[0]) != 2 || len(res.StoppedHubs) != 0 {
		t.Fatalf("PT1→P2: %+v", res)
	}
}

func TestPathsRespectsMaxDepth(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	res, err := g.Paths(ctx, ids.POL, ids.H, graph.PathOptions{MaxDepth: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 0 {
		t.Fatalf("MaxDepth 2 found %d paths", len(res.Paths))
	}
	if res, err = g.Paths(ctx, ids.POL, ids.H, graph.PathOptions{MaxDepth: 4}, nil); err != nil || len(res.Paths) != 1 {
		t.Fatalf("MaxDepth 4: %d paths, err %v", len(res.Paths), err)
	}
	if _, err := g.Paths(ctx, ids.POL, ids.H, graph.PathOptions{MaxDepth: -1}, nil); !errors.Is(err, graph.ErrBadOptions) {
		t.Fatalf("MaxDepth -1: err = %v, want ErrBadOptions", err)
	}
}

func TestPathsCarriesIdentityConfidence(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	res, err := graph.New(p).Paths(ctx, ids.POL, ids.H, graph.PathOptions{}, nil)
	if err != nil || len(res.Paths) != 1 {
		t.Fatalf("paths: %+v, %v", res, err)
	}
	for i, h := range res.Paths[0] {
		touchesP2 := h.Edge.Src.ID == ids.P2 || h.Edge.Dst.ID == ids.P2
		switch {
		case touchesP2 && (h.Edge.IdentityConfidence == nil || *h.Edge.IdentityConfidence != 0.99):
			t.Errorf("hop %d: identity confidence = %v, want 0.99", i, h.Edge.IdentityConfidence)
		case !touchesP2 && h.Edge.IdentityConfidence != nil:
			t.Errorf("hop %d between companies: identity confidence = %v, want nil", i, *h.Edge.IdentityConfidence)
		}
	}
}

func TestPathsSameEndpointsIsEmpty(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	res, err := graph.New(p).Paths(ctx, ids.A, ids.A, graph.PathOptions{}, nil)
	if err != nil || len(res.Paths) != 0 || res.Truncated {
		t.Fatalf("a == b: %+v, %v", res, err)
	}
}

// diamond builds S–M1–T and S–M2–T (two equal shortest paths), plus a
// second, parallel edge S–M1 that must not double the paths.
func diamond(t *testing.T) (*graph.Graph, int64, int64, []int64) {
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	s := b.Company(rc, "92000001", "EMPRESA FICTICIA S LTDA", "2062")
	tt := b.Company(rc, "92000002", "EMPRESA FICTICIA T LTDA", "2062")
	m1r, m1 := b.Person(rc, "92000001", "PESSOA FICTICIA M UM", "***101010**")
	m2r, m2 := b.Person(rc, "92000001", "PESSOA FICTICIA M DOIS", "***202020**")
	b.Rel(rc, claims.PartnerOf, rec(m1r, m1), ent(s), nil, nil)
	b.Rel(rc, claims.AdministratorOf, rec(m1r, m1), ent(s), nil, nil)
	b.Rel(rc, claims.PartnerOf, rec(m2r, m2), ent(s), nil, nil)
	b.Rel(rc, claims.PartnerOf, rec(m1r, m1), ent(tt), nil, nil)
	b.Rel(rc, claims.PartnerOf, rec(m2r, m2), ent(tt), nil, nil)
	return graph.New(p), s, tt, []int64{m1, m2}
}

func TestPathsReturnsAllShortestPaths(t *testing.T) {
	t.Parallel()
	g, s, tt, mids := diamond(t)
	res, err := g.Paths(ctx, s, tt, graph.PathOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 2 || res.Truncated {
		t.Fatalf("paths = %d (truncated %v), want 2", len(res.Paths), res.Truncated)
	}
	var via []int64
	for _, path := range res.Paths {
		walk := entities(path, s)
		if len(walk) != 3 || walk[2] != tt {
			t.Fatalf("bad path %v", walk)
		}
		via = append(via, walk[1])
		if walk[1] == mids[0] && path[0].Edge.Type != claims.PartnerOf {
			t.Errorf("parallel edges: want the lowest relationship id (partner_of), got %s", path[0].Edge.Type)
		}
	}
	if slices.Sort(via); !slices.Equal(via, mids) {
		t.Fatalf("paths go through %v, want %v", via, mids)
	}
	res, err = g.Paths(ctx, s, tt, graph.PathOptions{MaxPaths: 1}, nil)
	if err != nil || len(res.Paths) != 1 || !res.Truncated {
		t.Fatalf("MaxPaths 1: %d paths, truncated %v, err %v", len(res.Paths), res.Truncated, err)
	}
}

func TestPathsRelTypeFilter(t *testing.T) {
	t.Parallel()
	g, s, tt, _ := diamond(t)
	res, err := g.Paths(ctx, s, tt, graph.PathOptions{Options: graph.Options{RelTypes: []claims.RelType{claims.AdministratorOf}}}, nil)
	if err != nil || len(res.Paths) != 0 {
		t.Fatalf("administrator-only paths: %+v, %v", res, err)
	}
}

func TestPathsVisitCapTruncates(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	g.SetBounds(5, 50_000)
	res, err := g.Paths(ctx, ids.POL, ids.H, graph.PathOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Paths) != 0 {
		t.Fatalf("visit cap 5: truncated %v, %d paths; want truncated and none", res.Truncated, len(res.Paths))
	}
}

func TestPathsHubNextToBothEndpoints(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	s := b.Company(rc, "95000001", "EMPRESA FICTICIA PONTA S LTDA", "2062")
	tt := b.Company(rc, "95000002", "EMPRESA FICTICIA PONTA T LTDA", "2062")
	hub := b.Entity(claims.KindPerson, "hub:1", "PESSOA FICTICIA MUITO LIGADA")
	b.Rel(rc, claims.PartnerOf, ent(hub), ent(s), nil, nil)
	b.Rel(rc, claims.PartnerOf, ent(hub), ent(tt), nil, nil)
	for i, cnpj := range []string{"95000003", "95000004", "95000005"} {
		q := b.Company(rc, cnpj, "EMPRESA FICTICIA Q"+string(rune('1'+i))+" LTDA", "2062")
		b.Rel(rc, claims.PartnerOf, ent(hub), ent(q), nil, nil)
	}
	res, err := graph.New(p).Paths(ctx, s, tt, graph.PathOptions{HubDegree: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 0 {
		t.Fatalf("found %d paths through the hub", len(res.Paths))
	}
	if len(res.StoppedHubs) != 1 || res.StoppedHubs[0].Entity.ID != hub || res.StoppedHubs[0].Degree != 5 {
		t.Fatalf("stopped hubs = %+v, want the hub once with degree 5", res.StoppedHubs)
	}
}

func TestPathsOddLength(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	res, err := graph.New(p).Paths(ctx, ids.P2, ids.H, graph.PathOptions{}, nil)
	if err != nil || len(res.Paths) != 1 {
		t.Fatalf("P2→H: %+v, %v", res, err)
	}
	if got, want := entities(res.Paths[0], ids.P2), []int64{ids.P2, ids.C, ids.A, ids.H}; !slices.Equal(got, want) {
		t.Fatalf("path walks %v, want %v", got, want)
	}
}

func TestPathsCancelledContext(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := graph.New(p).Paths(cctx, ids.POL, ids.H, graph.PathOptions{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestPathsThroughClosedEdge(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	// P3's only edge, P3 → A, is closed.
	res, err := g.Paths(ctx, ids.P3, ids.C, graph.PathOptions{}, nil)
	if err != nil || len(res.Paths) != 0 {
		t.Fatalf("open edges only: %+v, %v", res, err)
	}
	for name, o := range map[string]graph.Options{"At 2015": {At: date(2015, 1, 1)}, "IncludeClosed": {IncludeClosed: true}} {
		res, err := g.Paths(ctx, ids.P3, ids.C, graph.PathOptions{Options: o}, nil)
		if err != nil || len(res.Paths) != 1 {
			t.Fatalf("%s: %+v, %v", name, res, err)
		}
		if got, want := entities(res.Paths[0], ids.P3), []int64{ids.P3, ids.A, ids.C}; !slices.Equal(got, want) {
			t.Fatalf("%s: path walks %v, want %v", name, got, want)
		}
	}
	// In 2026 the closed edge is no longer valid.
	if res, err := g.Paths(ctx, ids.P3, ids.C, graph.PathOptions{Options: graph.Options{At: date(2026, 1, 1)}}, nil); err != nil || len(res.Paths) != 0 {
		t.Fatalf("At 2026: %+v, %v", res, err)
	}
}

// The diamond's first level holds 4 entities and links (S, T, M1, M2); the
// second, from T, reaches 6 and is the level on which the sides meet. A cap
// crossed on that level truncates: no paths, never a partial set.
func TestPathsVisitCapCrossedOnTheMeetingLevel(t *testing.T) {
	t.Parallel()
	g, s, tt, _ := diamond(t)
	g.SetBounds(5, 50_000)
	res, err := g.Paths(ctx, s, tt, graph.PathOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Paths) != 0 {
		t.Fatalf("cap 5: truncated %v, %d paths; want truncated and none", res.Truncated, len(res.Paths))
	}
	g.SetBounds(6, 50_000)
	if res, err = g.Paths(ctx, s, tt, graph.PathOptions{}, nil); err != nil || res.Truncated || len(res.Paths) != 2 {
		t.Fatalf("cap 6: %d paths, truncated %v, err %v; want both paths", len(res.Paths), res.Truncated, err)
	}
}

// S–H–T is the shortest path but H is a hub, the only meeting candidate of
// its level; the search goes on and finds S–M–N–T.
func TestPathsAllMeetingCandidatesHubsFindsLongerPath(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	s := b.Company(rc, "98000001", "EMPRESA FICTICIA DESVIO S LTDA", "2062")
	tt := b.Company(rc, "98000002", "EMPRESA FICTICIA DESVIO T LTDA", "2062")
	hub := b.Entity(claims.KindPerson, "hub:98", "PESSOA FICTICIA CENTRAL")
	m := b.Entity(claims.KindPerson, "mid:98m", "PESSOA FICTICIA EME")
	n := b.Company(rc, "98000003", "EMPRESA FICTICIA ENE LTDA", "2062")
	b.Rel(rc, claims.PartnerOf, ent(hub), ent(s), nil, nil)
	b.Rel(rc, claims.PartnerOf, ent(hub), ent(tt), nil, nil)
	for i, cnpj := range []string{"98000004", "98000005"} {
		q := b.Company(rc, cnpj, "EMPRESA FICTICIA DESVIO Q"+string(rune('1'+i))+" LTDA", "2062")
		b.Rel(rc, claims.PartnerOf, ent(hub), ent(q), nil, nil)
	}
	b.Rel(rc, claims.PartnerOf, ent(m), ent(s), nil, nil)
	b.Rel(rc, claims.PartnerOf, ent(m), ent(n), nil, nil)
	b.Rel(rc, claims.PartnerOf, ent(n), ent(tt), nil, nil)

	res, err := graph.New(p).Paths(ctx, s, tt, graph.PathOptions{HubDegree: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 1 || res.Truncated {
		t.Fatalf("paths = %d (truncated %v), want 1", len(res.Paths), res.Truncated)
	}
	if got, want := entities(res.Paths[0], s), []int64{s, m, n, tt}; !slices.Equal(got, want) {
		t.Fatalf("path walks %v, want S–M–N–T %v", got, want)
	}
	if len(res.StoppedHubs) != 1 || res.StoppedHubs[0].Entity.ID != hub || res.StoppedHubs[0].Degree != 4 {
		t.Fatalf("stopped hubs = %+v, want the hub with degree 4", res.StoppedHubs)
	}
}
