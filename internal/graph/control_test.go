package graph_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"geoint/internal/claims"
	"geoint/internal/graph"
	"geoint/internal/testutil"
)

func refIDs(refs []graph.EntityRef) []int64 {
	var out []int64
	for _, r := range refs {
		out = append(out, r.ID)
	}
	slices.Sort(out)
	return out
}

func memberIDs(ms []graph.Member) []int64 {
	var out []int64
	for _, m := range ms {
		out = append(out, m.Entity.ID)
	}
	slices.Sort(out)
	return out
}

func stops(ss []graph.Stop) map[int64]string {
	out := map[int64]string{}
	for _, s := range ss {
		out[s.Entity.ID] = s.Reason
	}
	return out
}

func TestControlDocumentedFollowsOnlySupportedControl(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	res, err := graph.New(p).ControlGroup(ctx, ids.H, graph.ControlOptions{Mode: graph.Documented}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Root.ID != ids.H {
		t.Errorf("root = %+v", res.Root)
	}
	if got := refIDs(res.Controllers); !slices.Equal(got, []int64{ids.P1}) {
		t.Fatalf("controllers = %v, want [P1 %d]", got, ids.P1)
	}
	members := memberIDs(res.Members)
	for _, id := range []int64{ids.A, ids.B, ids.C} {
		if slices.Contains(members, id) {
			t.Errorf("members %v include %d: no percentage and several partners", members, id)
		}
	}
	if !slices.Equal(members, []int64{ids.H}) {
		t.Errorf("members = %v, want only H", members)
	}
	if len(res.Stops) != 0 {
		t.Errorf("stops = %+v", res.Stops)
	}
}

func TestControlDocumentedFollowsMajorityAndControls(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	top := b.Company(rc, "93000001", "CONTROLADORA FICTICIA SA", "2054")
	mid := b.Company(rc, "93000002", "INTERMEDIARIA FICTICIA SA", "2054")
	low := b.Company(rc, "93000003", "OPERADORA FICTICIA SA", "2054")
	minor := b.Company(rc, "93000004", "MINORITARIA FICTICIA SA", "2054")
	odd := b.Company(rc, "93000005", "PERCENTUAL ESTRANHO SA", "2054")
	other := b.Company(rc, "93000006", "OUTRA SOCIA FICTICIA SA", "2054")
	viaControls := b.Rel(rc, claims.Controls, ent(top), ent(mid), nil, nil)
	viaPercent := b.Rel(rc, claims.ShareholderOf, ent(mid), ent(low), map[string]any{"percent": "75,5"}, nil)
	b.Rel(rc, claims.ShareholderOf, ent(other), ent(low), map[string]any{"percent": 24.5}, nil)
	b.Rel(rc, claims.ShareholderOf, ent(low), ent(minor), map[string]any{"percent": 50}, nil)
	b.Rel(rc, claims.ShareholderOf, ent(low), ent(odd), map[string]any{"percent": "muito"}, nil)
	b.Rel(rc, claims.PartnerOf, ent(other), ent(minor), nil, nil)

	res, err := graph.New(p).ControlGroup(ctx, low, graph.ControlOptions{Mode: graph.Documented}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := refIDs(res.Controllers); !slices.Equal(got, []int64{top}) {
		t.Fatalf("controllers = %v, want [%d]", got, top)
	}
	if got := memberIDs(res.Members); !slices.Equal(got, sorted(mid, low)) {
		t.Fatalf("members = %v, want mid and low", got)
	}
	for _, m := range res.Members {
		if m.Entity.ID == low && (m.Depth != 2 || !slices.Equal(m.Via, []int64{viaControls, viaPercent})) {
			t.Errorf("low: depth %d via %v, want 2 via [%d %d]", m.Depth, m.Via, viaControls, viaPercent)
		}
	}
}

func TestControlCorporateLinksStopsAtFundForeignAndCrowd(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	var progress []float64
	res, err := graph.New(p).ControlGroup(ctx, ids.C, graph.ControlOptions{Mode: graph.CorporateLinks},
		func(f float64) { progress = append(progress, f) })
	if err != nil {
		t.Fatal(err)
	}
	controllers := refIDs(res.Controllers)
	if !slices.Contains(controllers, ids.P1) || !slices.Contains(controllers, ids.P2) {
		t.Fatalf("controllers = %v, want P1 %d and P2 %d", controllers, ids.P1, ids.P2)
	}
	members := memberIDs(res.Members)
	for _, id := range []int64{ids.H, ids.A, ids.B, ids.C} {
		if !slices.Contains(members, id) {
			t.Errorf("members %v miss %d", members, id)
		}
	}
	want := map[int64]string{ids.F: "fund", ids.X: "foreign", ids.W: "too_many_partners"}
	if got := stops(res.Stops); len(got) != len(want) || got[ids.F] != "fund" || got[ids.X] != "foreign" || got[ids.W] != "too_many_partners" {
		t.Fatalf("stops = %v, want %v", got, want)
	}
	if len(progress) == 0 || progress[len(progress)-1] != 1 || !slices.IsSorted(progress) {
		t.Errorf("progress = %v", progress)
	}
	for _, e := range res.Edges {
		if e.CollectionID == 0 || e.Kind == "" {
			t.Errorf("edge not filled: %+v", e)
		}
	}
}

func TestControlAdministratorDoesNotPropagate(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	res, err := graph.New(p).ControlGroup(ctx, ids.C, graph.ControlOptions{Mode: graph.CorporateLinks}, nil)
	if err != nil {
		t.Fatal(err)
	}
	hasAdmin := false
	for _, e := range res.Edges {
		hasAdmin = hasAdmin || e.RelationshipID == ids.RelADMC
	}
	if !hasAdmin {
		t.Error("the administrator edge of C must be in Edges")
	}
	for _, id := range []int64{ids.ADM, ids.Z} {
		if slices.Contains(refIDs(res.Controllers), id) || slices.Contains(memberIDs(res.Members), id) {
			t.Errorf("%d reached through an administrator", id)
		}
	}
}

func TestControlCycleTerminates(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	k1 := b.Company(rc, "94000001", "CICLO FICTICIO UM SA", "2054")
	k2 := b.Company(rc, "94000002", "CICLO FICTICIO DOIS SA", "2054")
	b.Rel(rc, claims.PartnerOf, ent(k1), ent(k2), nil, nil)
	b.Rel(rc, claims.PartnerOf, ent(k2), ent(k1), nil, nil)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, mode := range []graph.ControlMode{graph.Documented, graph.CorporateLinks} {
		res, err := graph.New(p).ControlGroup(cctx, k1, graph.ControlOptions{Mode: mode}, nil)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if got := memberIDs(res.Members); !slices.Equal(got, sorted(k1, k2)) {
			t.Errorf("%s: members = %v, want both companies", mode, got)
		}
	}
}

func TestControlBadOptions(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	for _, o := range []graph.ControlOptions{{Mode: "everything"}, {Mode: graph.Documented, MaxPartners: -1}} {
		if _, err := g.ControlGroup(ctx, ids.C, o, nil); !errors.Is(err, graph.ErrBadOptions) {
			t.Errorf("%+v: err = %v, want ErrBadOptions", o, err)
		}
	}
}

func TestControlTooLargeWhileClimbing(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	g.SetBounds(2_000_000, 2)
	res, err := g.ControlGroup(ctx, ids.C, graph.ControlOptions{Mode: graph.CorporateLinks}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Controllers) != 0 {
		t.Errorf("controllers = %v, want none", refIDs(res.Controllers))
	}
	if got := memberIDs(res.Members); !slices.Equal(got, []int64{ids.C}) {
		t.Errorf("members = %v, want only the root", got)
	}
	if got := stops(res.Stops); got[ids.C] != graph.StopTooLarge || len(res.Stops) != 1 {
		t.Errorf("stops = %v, want only the root too_large", got)
	}
	// Nothing of the abandoned climb is left: no edge leads to an entity
	// the result does not list.
	if len(res.Edges) != 0 {
		t.Errorf("edges = %d, want none (the climb was abandoned)", len(res.Edges))
	}
}

func TestControlTooLargeWhileDescending(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	g := graph.New(p)
	g.SetBounds(2_000_000, 2)
	// Climb: H, P1. Descent: H, then A and B (3 members, past 2), then stop.
	res, err := g.ControlGroup(ctx, ids.H, graph.ControlOptions{Mode: graph.CorporateLinks}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := refIDs(res.Controllers); !slices.Equal(got, []int64{ids.P1}) {
		t.Errorf("controllers = %v, want P1", got)
	}
	if got := memberIDs(res.Members); !slices.Equal(got, sorted(ids.H, ids.A, ids.B)) {
		t.Errorf("members = %v, want H, A, B (C is past the bound)", got)
	}
	if got := stops(res.Stops); got[ids.H] != graph.StopTooLarge {
		t.Errorf("stops = %v, want the root too_large", got)
	}
}

func TestControlCycleAtTopViaAndDepth(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	k1 := b.Company(rc, "96000001", "CICLO FICTICIO TOPO UM SA", "2054")
	k2 := b.Company(rc, "96000002", "CICLO FICTICIO TOPO DOIS SA", "2054")
	root := b.Company(rc, "96000003", "CONTROLADA FICTICIA SA", "2054")
	k1k2 := b.Rel(rc, claims.PartnerOf, ent(k1), ent(k2), nil, nil)
	b.Rel(rc, claims.PartnerOf, ent(k2), ent(k1), nil, nil)
	k2r := b.Rel(rc, claims.PartnerOf, ent(k2), ent(root), nil, nil)
	want := map[int64]graph.Member{
		k1:   {Depth: 0},
		k2:   {Depth: 1, Via: []int64{k1k2}},
		root: {Depth: 2, Via: []int64{k1k2, k2r}},
	}
	for _, mode := range []graph.ControlMode{graph.Documented, graph.CorporateLinks} {
		res, err := graph.New(p).ControlGroup(ctx, root, graph.ControlOptions{Mode: mode}, nil)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if len(res.Controllers) != 0 || len(res.Members) != 3 {
			t.Fatalf("%s: controllers %v, members %+v", mode, refIDs(res.Controllers), res.Members)
		}
		for _, m := range res.Members {
			w := want[m.Entity.ID]
			if m.Depth != w.Depth || !slices.Equal(m.Via, w.Via) {
				t.Errorf("%s: member %d depth %d via %v, want depth %d via %v", mode, m.Entity.ID, m.Depth, m.Via, w.Depth, w.Via)
			}
		}
	}
}

func TestControlCorporateLinksFollowsControls(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	top := b.Company(rc, "97000001", "CONTROLADORA FICTICIA DOIS SA", "2054")
	sub := b.Company(rc, "97000002", "SUBSIDIARIA FICTICIA SA", "2054")
	b.Rel(rc, claims.Controls, ent(top), ent(sub), nil, nil)
	res, err := graph.New(p).ControlGroup(ctx, sub, graph.ControlOptions{Mode: graph.CorporateLinks}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := refIDs(res.Controllers); !slices.Equal(got, []int64{top}) {
		t.Fatalf("controllers = %v, want [%d]", got, top)
	}
}

func TestControlRootMustBeCompany(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	ids := seed(t, p)
	if _, err := graph.New(p).ControlGroup(ctx, ids.P1, graph.ControlOptions{Mode: graph.Documented}, nil); !errors.Is(err, graph.ErrBadOptions) {
		t.Fatalf("person root: err = %v, want ErrBadOptions", err)
	}
}
