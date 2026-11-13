package graph_test

import (
	"testing"
	"time"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/graph"
	"geoint/internal/graph/graphtest"
	"geoint/internal/identity"
	"geoint/internal/testutil"
)

func byRel(edges []graph.Edge) map[int64]graph.Edge {
	out := map[int64]graph.Edge{}
	for _, e := range edges {
		out[e.RelationshipID] = e
	}
	return out
}

// Every edge says on what its person ends' identity rests: one record
// (none), records joined automatically (auto) or by an analyst (analyst).
func TestEdgesCarryIdentityBasisAndClosed(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b, rc := freshReceita(t, p)
	co1 := b.Company(rc, "97000001", "EMPRESA FICTICIA BASE UM LTDA", "2062")
	co2 := b.Company(rc, "97000002", "EMPRESA FICTICIA BASE DOIS LTDA", "2062")
	co3 := b.Company(rc, "97000003", "EMPRESA FICTICIA BASE TRES LTDA", "2062")
	co4 := b.Company(rc, "97000004", "EMPRESA FICTICIA BASE QUATRO LTDA", "2062")
	co5 := b.Company(rc, "97000005", "EMPRESA FICTICIA BASE CINCO LTDA", "2062")
	hold := b.Company(rc, "97000006", "HOLDING FICTICIA BASE SA", "2054")

	// One record.
	r1, e1 := b.Person(rc, "97000001", "PESSOA FICTICIA SOZINHA", "***101010**")
	relSingle := b.Rel(rc, claims.PartnerOf, rec(r1, e1), ent(co1), nil, nil)
	relCompanies := b.Rel(rc, claims.PartnerOf, ent(hold), ent(co1), nil, nil)

	// Two records joined automatically (as the resolver does it).
	ra, ea := b.Person(rc, "97000002", "PESSOA FICTICIA AUTOMATICA", "***202020**")
	rb, eb := b.Person(rc, "97000003", "PESSOA FICTICIA AUTOMATICA", "***202020**")
	relAutoA := b.Rel(rc, claims.PartnerOf, rec(ra, ea), ent(co2), nil, nil)
	relAutoB := b.Rel(rc, claims.PartnerOf, rec(rb, eb), ent(co3), nil, nil)
	if _, err := p.Exec(ctx, `UPDATE person_record SET entity_id = $1, identity_confidence = 0.99999 WHERE id = $2`, ea, rb); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE relationship SET src_entity_id = $1 WHERE id = $2`, ea, relAutoB); err != nil {
		t.Fatal(err)
	}

	// Two records merged by an analyst (name only: different fragments).
	rx, ex := b.Person(rc, "97000004", "PESSOA FICTICIA ANALISADA", "***303030**")
	ry, ey := b.Person(rc, "97000005", "PESSOA FICTICIA ANALISADA", "***404040**")
	relAnalystX := b.Rel(rc, claims.PartnerOf, rec(rx, ex), ent(co4), nil, nil)
	relAnalystY := b.Rel(rc, claims.PartnerOf, rec(ry, ey), ent(co5), nil, nil)
	merged, err := (&identity.Resolver{Pool: p}).Merge(ctx, []int64{rx, ry}, "sub-analyst", "same person (test)")
	if err != nil {
		t.Fatal(err)
	}

	// A closed edge.
	if _, err := p.Exec(ctx, `UPDATE relationship SET closed_collection_id = $2 WHERE id = $1`, relCompanies, rc); err != nil {
		t.Fatal(err)
	}

	g := graph.New(p)
	all := graph.Options{IncludeClosed: true}
	edges := byRel(mustNeighbors(t, g, co1, all))
	for _, e := range mustNeighbors(t, g, ea, all) {
		edges[e.RelationshipID] = e
	}
	for _, e := range mustNeighbors(t, g, merged, all) {
		edges[e.RelationshipID] = e
	}
	cases := []struct {
		name   string
		rel    int64
		basis  string
		closed bool
	}{
		{"single record", relSingle, graph.IdentityNone, false},
		{"companies only", relCompanies, graph.IdentityNone, true},
		{"automatic join A", relAutoA, graph.IdentityAuto, false},
		{"automatic join B", relAutoB, graph.IdentityAuto, false},
		{"analyst merge X", relAnalystX, graph.IdentityAnalyst, false},
		{"analyst merge Y", relAnalystY, graph.IdentityAnalyst, false},
	}
	for _, c := range cases {
		e, ok := edges[c.rel]
		if !ok {
			t.Errorf("%s: edge %d not returned", c.name, c.rel)
			continue
		}
		if e.IdentityBasis != c.basis || e.Closed != c.closed {
			t.Errorf("%s: identity_basis %q closed %v, want %q %v", c.name, e.IdentityBasis, e.Closed, c.basis, c.closed)
		}
	}
	if e := edges[relAnalystX]; e.IdentityConfidence == nil || *e.IdentityConfidence != 1 {
		t.Errorf("analyst merge: identity confidence %v, want 1 (the basis, not the number, tells it apart)", e.IdentityConfidence)
	}

	// The same values reach a path.
	res, err := g.Paths(ctx, co4, co5, graph.PathOptions{}, nil)
	if err != nil || len(res.Paths) != 1 {
		t.Fatalf("paths: %+v, %v", res, err)
	}
	for _, h := range res.Paths[0] {
		if h.Edge.IdentityBasis != graph.IdentityAnalyst {
			t.Errorf("path hop %d: identity_basis %q, want analyst", h.Edge.RelationshipID, h.Edge.IdentityBasis)
		}
	}
}

// Staleness counts from the observed (publication) date for an edition
// source and from the reference date otherwise, as internal/freshness does.
func TestStalenessCountsFromThePublicationDateOfEditions(t *testing.T) {
	t.Parallel()
	p := testutil.DB(t)
	b := graphtest.NewBuilder(t, p)
	now := time.Now().UTC().Truncate(24 * time.Hour)
	begin := func(src int16, ref, observed time.Time) int64 {
		t.Helper()
		c, err := collection.BeginObserved(ctx, p, src, ref, observed, collection.Scope{Full: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := collection.Finish(ctx, p, c.ID, nil); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	// Receita (editions, stale after 45 days): the edition of 60 days ago was
	// published yesterday. TSE (no editions, stale after 30 days): data of 60
	// days ago, observed today.
	rc := begin(1, now.AddDate(0, 0, -60), now.AddDate(0, 0, -1))
	tc := begin(2, now.AddDate(0, 0, -60), now)
	co := b.Company(rc, "97100001", "EMPRESA FICTICIA EDICAO LTDA", "2062")
	pol := b.Entity(claims.KindPerson, "candidate:9710", "POLITICO FICTICIO EDICAO")
	party := b.Entity(claims.KindParty, "party:9710", "PARTIDO FICTICIO EDICAO")
	hold := b.Company(rc, "97100002", "HOLDING FICTICIA EDICAO SA", "2054")
	relR := b.Rel(rc, claims.PartnerOf, ent(hold), ent(co), nil, nil)
	relT := b.Rel(tc, claims.CandidateFor, ent(pol), ent(party), nil, nil)

	g := graph.New(p)
	if e := byRel(mustNeighbors(t, g, co, graph.Options{}))[relR]; e.Stale {
		t.Errorf("receita edition published yesterday: stale")
	}
	if e := byRel(mustNeighbors(t, g, pol, graph.Options{}))[relT]; !e.Stale {
		t.Errorf("tse data of 60 days ago: not stale")
	}
	var editions bool
	if err := p.QueryRow(ctx, `SELECT editions FROM source WHERE id = 1`).Scan(&editions); err != nil || !editions {
		t.Fatalf("receita editions = %v, %v; the test needs it true", editions, err)
	}
}
