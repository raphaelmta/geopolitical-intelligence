package identity_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/identity"
)

func TestStrongMatchMergesWithConfidence(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "Ana Rara Peixoto", "123456")
	b := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	res := f.resolve(t, col)

	if f.entityOf(t, a) != f.entityOf(t, b) {
		t.Fatal("records with the same name and fragment were not merged")
	}
	// One distinct fragment for the name: the honest bound is capped just
	// below 1, so an automatic merge never reads as certain.
	want := 1 - 1.0/1e5
	for _, r := range []int64{a, b} {
		if c := f.confidence(t, r); !near(c, want) {
			t.Errorf("confidence of %d = %v, want %v", r, c, want)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision WHERE kind='auto' AND action='merge'`); n != 1 {
		t.Errorf("auto decisions = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision WHERE record_ids=$1 AND entity_id=$2`,
		[]int64{a, b}, f.entityOf(t, a)); n != 1 {
		t.Errorf("decision does not list both records and the target entity")
	}
	if res.AutoMerged != 1 || res.Proposed != 0 {
		t.Errorf("result = %+v, want 1 moved, 0 proposed", res)
	}
}

func TestNameOnlyNeverMerges(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "111111")
	b := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "222222")
	f.resolve(t, col)
	if f.entityOf(t, a) == f.entityOf(t, b) {
		t.Fatal("records sharing only a name were merged")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision`); n != 0 {
		t.Errorf("decisions = %d, want 0", n)
	}
}

func TestMissingFragmentNeverMerges(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "")
	b := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "")
	f.resolve(t, col)
	if f.entityOf(t, a) == f.entityOf(t, b) {
		t.Fatal("records without fragment were merged")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision`) + f.count(t, `SELECT count(*) FROM identity_proposal`); n != 0 {
		t.Errorf("decisions+proposals = %d, want 0", n)
	}
}

func TestEmptyNameNeverMerges(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "", "123456")
	b := f.person(t, col, receita, "00000002", "", "123456")
	c := f.person(t, col, receita, "00000003", "---", "123456") // normalizes to ""
	f.person(t, col, receita, "00000004", "", "111111")
	f.person(t, col, receita, "00000005", "", "222222")
	f.resolve(t, col)
	if f.entityOf(t, a) == f.entityOf(t, b) || f.entityOf(t, a) == f.entityOf(t, c) {
		t.Fatal("records with an empty name were merged")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision`) + f.count(t, `SELECT count(*) FROM identity_proposal`); n != 0 {
		t.Errorf("decisions+proposals = %d, want 0", n)
	}
	cands, err := f.r.PossibleSame(ctx, f.entityOf(t, a), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("PossibleSame of an empty name = %+v, want none", cands)
	}
}

func TestCommonNameGoesToQueueNotMerged(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	b := f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	res := f.resolve(t, col)

	if f.entityOf(t, a) == f.entityOf(t, b) {
		t.Fatal("common name was merged")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE status='pending' AND record_ids=$1
		AND group_key='JOSE DA SILVA|123456'`, []int64{a, b}); n != 1 {
		t.Errorf("pending proposals with both records = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal`); n != 1 {
		t.Errorf("proposals = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision`); n != 0 {
		t.Errorf("decisions = %d, want 0", n)
	}
	if res.Proposed != 1 || res.AutoMerged != 0 {
		t.Errorf("result = %+v, want 0 moved, 1 proposed", res)
	}
}

func TestMergeRepointsRelationships(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	b := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	f.resolve(t, col)
	if n := f.count(t, `SELECT count(DISTINCT src_entity_id) FROM relationship WHERE src_record_id IN ($1, $2)`, a, b); n != 1 {
		t.Fatalf("relationships point at %d entities, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE r.src_entity_id <> p.entity_id`); n != 0 {
		t.Errorf("%d relationships point away from their record's entity", n)
	}
}

func TestResolveTwiceIsIdempotent(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	f.person(t, col, receita, "00000003", "ANA RARA PEIXOTO", "123456")
	f.commonName(t, col, "JOSE DA SILVA")
	f.person(t, col, receita, "00000004", "JOSE DA SILVA", "123456")
	f.person(t, col, receita, "00000005", "JOSE DA SILVA", "123456")
	first := f.resolve(t, col)
	if first.AutoMerged != 2 || first.Proposed != 1 {
		t.Fatalf("first result = %+v, want 2 moved, 1 proposed", first)
	}
	before := snapshot(t, f)
	decisions := f.count(t, `SELECT count(*) FROM identity_decision`)

	second := f.resolve(t, col)
	if changes(second) != (identity.Result{}) {
		t.Errorf("second result = %+v, want zero", second)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision`); n != decisions {
		t.Errorf("decisions %d -> %d after re-resolve", decisions, n)
	}
	if after := snapshot(t, f); after != before {
		t.Errorf("state changed on re-resolve:\n%s\n---\n%s", before, after)
	}
}

func TestResolveInSmallBatchesMatchesOneBatch(t *testing.T) {
	one := newFixture(t, 2)
	colR, colT := seedConcurrent(t, one)
	one.resolve(t, colR)
	one.resolve(t, colT)

	identity.SetNamesPerBatch(t, 1)
	many := newFixture(t, 2)
	colR, colT = seedConcurrent(t, many)
	res := many.resolve(t, colR)
	if res.AutoMerged == 0 || res.Proposed == 0 {
		t.Fatalf("result = %+v, want merges and a proposal", res)
	}
	many.resolve(t, colT)
	if got, want := snapshot(t, many), snapshot(t, one); got != want {
		t.Errorf("batched state differs:\n%s\n---\n%s", got, want)
	}
}

func TestLockedRecordIsNotMoved(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	other := f.person(t, col, receita, "00000002", "BRUNO OUTRO NOME", "999999")
	// The analyst puts a with an unrelated record; Resolve must leave a there.
	ent, err := f.r.Merge(ctx, []int64{other, a}, "analyst@example", "same person, documented")
	if err != nil {
		t.Fatal(err)
	}
	if f.entityOf(t, a) != ent || f.entityOf(t, other) != ent {
		t.Fatal("merge did not group the records")
	}
	if c := f.confidence(t, a); c != 1 {
		t.Errorf("analyst-merged confidence = %v, want 1", c)
	}
	b := f.person(t, col, receita, "00000003", "ANA RARA PEIXOTO", "123456")
	c := f.person(t, col, receita, "00000004", "ANA RARA PEIXOTO", "123456")
	f.resolve(t, col)
	if f.entityOf(t, a) != ent {
		t.Error("resolve moved a locked record")
	}
	if f.entityOf(t, b) == ent || f.entityOf(t, b) != f.entityOf(t, c) {
		t.Error("unlocked records must merge among themselves only")
	}
}

func TestProposalConnectionsOrderTheQueue(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	f.commonName(t, col, "MARIA SOUZA")
	j1 := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	m1 := f.person(t, col, receita, "00000003", "MARIA SOUZA", "123456")
	f.person(t, col, receita, "00000004", "MARIA SOUZA", "123456")
	// Maria has two extra relationships, so she comes first in the queue.
	ent := f.entityOf(t, m1)
	for _, k := range []string{"x1", "x2"} {
		company, err := f.s.UpsertEntity(ctx, claims.KindCompany, k, k)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: claims.AdministratorOf, SrcEntityID: ent,
			DstEntityID: company, SrcRecordID: &m1, Kind: claims.Fact, Prov: claims.Provenance{CollectionID: col, RecordRef: k}}); err != nil {
			t.Fatal(err)
		}
	}
	f.resolve(t, col)
	queue, err := f.r.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 {
		t.Fatalf("queue = %+v, want 2 proposals", queue)
	}
	if queue[0].GroupKey != "MARIA SOUZA|123456" || queue[0].Connections != 4 {
		t.Errorf("first = %+v, want MARIA SOUZA with 4 connections", queue[0])
	}
	if queue[1].GroupKey != "JOSE DA SILVA|123456" || queue[1].Connections != 2 || queue[1].RecordIDs[0] != j1 {
		t.Errorf("second = %+v, want JOSE DA SILVA with 2 connections", queue[1])
	}
}

func TestCrossSourceStrongMatch(t *testing.T) {
	f := newFixture(t, 0)
	rf, ok := identity.CPFFragment("***222333**")
	if !ok {
		t.Fatal("receita fragment")
	}
	tf, ok := identity.CPFFragment("11122233344")
	if !ok {
		t.Fatal("tse fragment")
	}
	colR := f.collect(t, receita)
	a := f.person(t, colR, receita, "00000001", "Ana Rara Peixoto", rf)
	f.resolve(t, colR)
	colT := f.collect(t, tse)
	b := f.person(t, colT, tse, "cand-1", "ANA RARA PEIXOTO", tf)
	f.resolve(t, colT)
	if f.entityOf(t, a) != f.entityOf(t, b) {
		t.Fatal("cross-source strong match was not merged")
	}
}

func TestResolveOnlyTouchesNamesOfTheCollection(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	b := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	other := f.collect(t, tse)
	f.person(t, other, tse, "cand-1", "BRUNO OUTRO NOME", "999999")
	f.resolve(t, other)
	if f.entityOf(t, a) == f.entityOf(t, b) {
		t.Fatal("resolve merged names its collection did not touch")
	}
}

func TestPossibleSameListsNameOnlyCandidates(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	same := f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456") // common name: proposed, not merged
	diff := f.person(t, col, receita, "00000003", "JOSE DA SILVA", "111111")
	none := f.person(t, col, receita, "00000004", "JOSE DA SILVA", "")
	f.person(t, col, receita, "00000005", "JOSE DA SILVA", "222222")
	f.person(t, col, receita, "00000006", "MARIA SOUZA", "123456")
	f.resolve(t, col)

	cands, err := f.r.PossibleSame(ctx, f.entityOf(t, a), 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]identity.Candidate{}
	for _, c := range cands {
		got[c.EntityID] = c
	}
	if len(got) != 4 {
		t.Fatalf("candidates = %+v, want 4", cands)
	}
	if _, ok := got[f.entityOf(t, a)]; ok {
		t.Error("the entity itself is listed")
	}
	if c := got[f.entityOf(t, same)]; !c.SharedFragment || c.Records != 1 || c.Name != "JOSE DA SILVA" {
		t.Errorf("same-fragment candidate = %+v", c)
	}
	for _, r := range []int64{diff, none} {
		if c, ok := got[f.entityOf(t, r)]; !ok || c.SharedFragment {
			t.Errorf("candidate of record %d = %+v, ok=%v", r, c, ok)
		}
	}
	if !cands[0].SharedFragment {
		t.Error("shared-fragment candidates must come first")
	}
	if f.entityOf(t, a) == f.entityOf(t, diff) || f.entityOf(t, a) == f.entityOf(t, none) {
		t.Error("PossibleSame candidates were merged")
	}
	limited, err := f.r.PossibleSame(ctx, f.entityOf(t, a), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Errorf("limit 2 returned %d", len(limited))
	}
}

func TestPossibleSameIgnoresOrphanEntities(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	b := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456") // its entity becomes an orphan
	orphan := f.entityOf(t, b)
	c := f.person(t, col, receita, "00000003", "ANA RARA PEIXOTO", "777777")
	f.resolve(t, col)
	if f.entityOf(t, b) == orphan {
		t.Fatal("fixture: b should have moved")
	}
	cands, err := f.r.PossibleSame(ctx, f.entityOf(t, a), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].EntityID != f.entityOf(t, c) {
		t.Errorf("candidates = %+v, want only the entity of c", cands)
	}
	if cands, err := f.r.PossibleSame(ctx, orphan, 10); err != nil || len(cands) != 0 {
		t.Errorf("PossibleSame(orphan) = %+v, %v; want none", cands, err)
	}
}

func TestConcurrentResolvesMatchSerial(t *testing.T) {
	serial := newFixture(t, 2)
	r1, t1 := seedConcurrent(t, serial)
	serial.resolve(t, r1)
	serial.resolve(t, t1)

	for round := range 3 {
		conc := newFixture(t, 2)
		r2, t2 := seedConcurrent(t, conc)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, col := range []int64{r2, t2} {
			wg.Add(1)
			go func() { defer wg.Done(); _, errs[i] = conc.r.Resolve(ctx, col) }()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		if got, want := snapshot(t, conc), snapshot(t, serial); got != want {
			t.Errorf("round %d: concurrent state differs from serial:\n%s\n---\n%s", round, got, want)
		}
	}
}

func TestResolveRejectsNegativeThreshold(t *testing.T) {
	f := newFixture(t, -1)
	col := f.collect(t, receita)
	if _, err := f.r.Resolve(ctx, col); err == nil {
		t.Error("negative threshold must be an error")
	}
}

func TestMergedDestinationsKeepBothRelationships(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	colT := f.collect(t, tse)
	p1 := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	p2 := f.person(t, colT, tse, "cand-1", "ANA RARA PEIXOTO", "123456")
	rep := f.person(t, col, receita, "00000001", "BRUNO REPRESENTANTE", "999999")
	// Two open claims identical except for the destination record.
	for i, p := range []int64{p1, p2} {
		if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: claims.LegalRepOf,
			DstEntityID: f.entityOf(t, p), SrcRecordID: &rep, DstRecordID: &p, Kind: claims.Fact,
			Prov: claims.Provenance{CollectionID: col, RecordRef: fmt.Sprint("rep", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	orphan := f.entityOf(t, p2)
	f.resolve(t, colT)
	merged := f.entityOf(t, p1)
	if f.entityOf(t, p2) != merged {
		t.Fatal("not merged")
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE rel_type='legal_rep_of' AND dst_entity_id=$1`, merged); n != 2 {
		t.Errorf("legal_rep_of rows at the merged entity = %d, want 2", n)
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE dst_entity_id=$1 OR src_entity_id=$1`, orphan); n != 0 {
		t.Errorf("%d relationships left on the orphan entity", n)
	}
}

func TestAutoMergeNeverExtendsAnAnalystEntity(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	x := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	x1 := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	f.resolve(t, col)
	if f.entityOf(t, x) != f.entityOf(t, x1) {
		t.Fatal("fixture: X and X' should be merged")
	}
	y := f.person(t, col, receita, "00000003", "BRUNO OUTRO NOME", "999999")
	if _, err := f.r.Merge(ctx, []int64{x, y}, "analyst@example", "same person"); err != nil {
		t.Fatal(err)
	}
	col2 := f.collect(t, tse)
	x2 := f.person(t, col2, tse, "cand-1", "ANA RARA PEIXOTO", "123456")
	res := f.resolve(t, col2)
	if f.entityOf(t, x2) == f.entityOf(t, y) {
		t.Fatal("X'' was placed with Y through an analyst-built entity")
	}
	if res.AutoMerged != 0 || res.Proposed != 1 {
		t.Errorf("result = %+v, want 0 moved, 1 proposed", res)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE status='pending' AND record_ids=$1`, []int64{x1, x2}); n != 1 {
		t.Error("no pending proposal for X' and X''")
	}
}

func TestConfirmedKeyDoesNotJoinAnExtendedEntity(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	f.resolve(t, col)
	if err := f.r.Decide(ctx, f.proposalID(t), true, "analyst@example"); err != nil {
		t.Fatal(err)
	}
	z := f.person(t, col, receita, "00000003", "BRUNO OUTRO NOME", "999999")
	if _, err := f.r.Merge(ctx, []int64{a, z}, "analyst@example", ""); err != nil {
		t.Fatal(err)
	}
	col2 := f.collect(t, tse)
	c := f.person(t, col2, tse, "cand-1", "JOSE DA SILVA", "123456")
	f.resolve(t, col2)
	if f.entityOf(t, c) == f.entityOf(t, z) {
		t.Error("a confirmed key joined an entity that also holds another key")
	}
}

func TestFailedResolveIsCompletedByTheNext(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a1 := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	a2 := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	b1 := f.person(t, col, receita, "00000003", "BRUNO RARO PEIXOTO", "654321")
	b2 := f.person(t, col, receita, "00000004", "BRUNO RARO PEIXOTO", "654321")

	identity.SetNamesPerBatch(t, 1)
	cctx, cancel := context.WithCancel(ctx)
	identity.SetAfterBatch(t, cancel)
	if _, err := f.r.Resolve(cctx, col); err == nil {
		t.Fatal("resolve should fail after its first batch")
	}
	identity.SetAfterBatch(t, nil)
	if f.entityOf(t, a1) != f.entityOf(t, a2) || f.entityOf(t, b1) == f.entityOf(t, b2) {
		t.Fatal("fixture: only the first name should be resolved")
	}
	if err := collection.Fail(ctx, f.pool, col, errors.New("resolve failed")); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_resolve_done`); n != 0 {
		t.Fatal("a failed resolve was marked done")
	}

	later := f.collect(t, tse)
	f.person(t, later, tse, "cand-1", "CARLA OUTRA", "111222")
	res := f.resolve(t, later)
	if f.entityOf(t, b1) != f.entityOf(t, b2) {
		t.Error("the next resolve did not complete the failed one")
	}
	if res.AutoMerged != 1 {
		t.Errorf("result = %+v, want the 1 record moved for the failed collection", res)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_resolve_done WHERE collection_id IN ($1, $2)`, col, later); n != 2 {
		t.Errorf("resolve markers = %d, want 2", n)
	}
}

func TestResolveSweepsStalePointers(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	b := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	stale := f.person(t, col, receita, "00000002", "BRUNO OUTRO NOME", "999999")
	// Simulate a claim written with an entity the record has since left.
	if _, err := f.pool.Exec(ctx, `UPDATE relationship SET src_entity_id=$2 WHERE src_record_id=$1`, b, f.entityOf(t, stale)); err != nil {
		t.Fatal(err)
	}
	f.resolve(t, col)
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE src_record_id=$1 AND src_entity_id=$2`, b, f.entityOf(t, b)); n != 1 {
		t.Error("the stale pointer was not corrected")
	}
}

func TestMootProposalBecomesObsoleteAndReopens(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	b := f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	f.resolve(t, col)
	if _, err := f.r.Merge(ctx, []int64{a, b}, "analyst@example", ""); err != nil {
		t.Fatal(err)
	}
	col2 := f.collect(t, receita)
	f.person(t, col2, receita, "00000003", "JOSE DA SILVA", "333333")
	f.resolve(t, col2)
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE status='obsolete' AND decided_by='resolver'`); n != 1 {
		t.Fatal("moot proposal not closed as obsolete")
	}
	if q, err := f.r.Pending(ctx, 10); err != nil || len(q) != 0 {
		t.Errorf("Pending = %+v, %v; want none", q, err)
	}

	col3 := f.collect(t, tse)
	c := f.person(t, col3, tse, "cand-1", "JOSE DA SILVA", "123456")
	d := f.person(t, col3, tse, "cand-2", "JOSE DA SILVA", "123456")
	f.resolve(t, col3)
	q, err := f.r.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 1 || len(q[0].RecordIDs) != 2 || q[0].RecordIDs[0] != c || q[0].RecordIDs[1] != d {
		t.Errorf("Pending = %+v, want the reopened proposal with the new records", q)
	}
}

func TestNameCountIncludesUnlinkedRecords(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	b := f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	f.person(t, col, receita, "00000003", "JOSE DA SILVA", "111111")
	if _, err := f.pool.Exec(ctx, `INSERT INTO person_record (source_id, context_key, name, name_norm, cpf_fragment,
		first_collection_id, record_ref) VALUES (1, '00000004', 'JOSE DA SILVA', 'JOSE DA SILVA', '222222', $1, 'raw')`, col); err != nil {
		t.Fatal(err)
	}
	f.resolve(t, col)
	if f.entityOf(t, a) == f.entityOf(t, b) {
		t.Error("n ignored the unlinked record and merged a common name")
	}
}

func TestFailingEarlierCollectionDoesNotBlockResolve(t *testing.T) {
	f := newFixture(t, 0)
	early := f.collect(t, receita)
	e1 := f.person(t, early, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	e2 := f.person(t, early, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	if err := collection.Fail(ctx, f.pool, early, errors.New("load failed")); err != nil {
		t.Fatal(err)
	}
	later := f.collect(t, tse)
	l1 := f.person(t, later, tse, "cand-1", "BRUNO RARO PEIXOTO", "654321")
	l2 := f.person(t, later, tse, "cand-2", "BRUNO RARO PEIXOTO", "654321")

	identity.SetBeforeCollection(t, func(id int64) error {
		if id == early {
			return errors.New("poison")
		}
		return nil
	})
	if _, err := f.r.Resolve(ctx, later); err != nil {
		t.Fatalf("a failing earlier collection blocked the requested one: %v", err)
	}
	if f.entityOf(t, l1) != f.entityOf(t, l2) {
		t.Error("the requested collection was not resolved")
	}
	if f.entityOf(t, e1) == f.entityOf(t, e2) {
		t.Fatal("fixture: the earlier collection should have failed")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_resolve_done WHERE collection_id=$1`, early); n != 0 {
		t.Error("a failed earlier collection was marked done")
	}

	// The requested collection's own failure is still returned.
	identity.SetBeforeCollection(t, func(int64) error { return errors.New("own failure") })
	if _, err := f.r.Resolve(ctx, later); err == nil {
		t.Error("the requested collection's failure was swallowed")
	}

	identity.SetBeforeCollection(t, nil)
	f.resolve(t, later)
	if f.entityOf(t, e1) != f.entityOf(t, e2) {
		t.Error("the earlier collection was not retried")
	}
}

func TestSweepRunsOncePerCollection(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	b := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	other := f.person(t, col, receita, "00000002", "BRUNO OUTRO NOME", "999999")
	f.resolve(t, col)
	// After the collection is marked, a stale pointer is no longer the
	// sweep's business: a re-run performs no sweep writes.
	if _, err := f.pool.Exec(ctx, `UPDATE relationship SET src_entity_id=$2 WHERE src_record_id=$1`, b, f.entityOf(t, other)); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, f)
	f.resolve(t, col)
	if after := snapshot(t, f); after != before {
		t.Errorf("re-run on a marked collection wrote:\n%s\n---\n%s", before, after)
	}
}

func TestDefaultCommonNameThresholdIsConservative(t *testing.T) {
	if identity.DefaultCommonNameThreshold != 100 {
		t.Fatalf("DefaultCommonNameThreshold = %d, want 100", identity.DefaultCommonNameThreshold)
	}
}

// Confidence of an automatic merge: max(0.5, 1 - (n-1)/1e5), n distinct
// fragments for the name (the 9th CPF digit is the issuing region, so the
// effective fragment space is about 1e5), never 1.
func TestAutoMergeConfidenceIsAnHonestBound(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	for i := range 40 { // 40 other distinct fragments: n = 41
		f.person(t, col, receita, fmt.Sprintf("8%07d", i), "ANA RARA PEIXOTO", fmt.Sprintf("%06d", 700000+i))
	}
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	b := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	f.resolve(t, col)
	if f.entityOf(t, a) != f.entityOf(t, b) {
		t.Fatal("not merged")
	}
	if c, want := f.confidence(t, a), 1-40.0/1e5; !near(c, want) {
		t.Errorf("confidence = %v, want %v", c, want)
	}
}

func TestResolveReportsTheFragmentDistribution(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	// n = 1 (merges), n = 3 (proposed: above 2), n = 12 (no group).
	f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	f.commonName(t, col, "JOSE DA SILVA")
	f.person(t, col, receita, "00000003", "JOSE DA SILVA", "123456")
	f.person(t, col, receita, "00000004", "JOSE DA SILVA", "123456")
	for i := range 12 {
		f.person(t, col, receita, fmt.Sprintf("7%07d", i), "MARIA COMUM", fmt.Sprintf("%06d", 500000+i))
	}
	res := f.resolve(t, col)
	want := [5]int64{1, 1, 1, 0, 0} // 1 | 2-10 | 11-100 | 101-1000 | >1000
	if res.NamesByFragments != want {
		t.Errorf("names by fragments = %v, want %v", res.NamesByFragments, want)
	}
	if res.AutoMergedGroups != 1 || res.ProposalsCreated != 1 {
		t.Errorf("auto-merged groups = %d, proposals created = %d, want 1 and 1", res.AutoMergedGroups, res.ProposalsCreated)
	}
	// A second run creates nothing new.
	if again := f.resolve(t, col); again.AutoMergedGroups != 0 || again.ProposalsCreated != 0 {
		t.Errorf("re-run: %+v", again)
	}
}
