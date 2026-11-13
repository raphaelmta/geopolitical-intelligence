package identity_test

import (
	"errors"
	"testing"

	"geoint/internal/identity"
)

func TestSplitSurvivesNextResolve(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	b := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	f.resolve(t, col)
	merged := f.entityOf(t, a)

	ent, err := f.r.Split(ctx, []int64{b}, "analyst@example", "different people: different mothers")
	if err != nil {
		t.Fatal(err)
	}
	if ent == merged || f.entityOf(t, b) != ent {
		t.Fatalf("split did not move b to a new entity (got %d, merged %d)", ent, merged)
	}
	if c := f.confidence(t, a); c != 1 {
		t.Errorf("confidence of the record left alone = %v, want 1", c)
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE src_record_id=$1 AND src_entity_id=$2`, b, ent); n != 1 {
		t.Errorf("split did not re-point b's relationship")
	}
	if n := f.count(t, `SELECT count(*) FROM person_record p JOIN identity_decision d ON d.id = p.locked_by_decision_id
		WHERE p.id=$1 AND d.kind='analyst' AND d.action='split' AND d.decided_by='analyst@example'`, b); n != 1 {
		t.Errorf("b is not locked by an analyst split decision")
	}

	col2 := f.collect(t, receita)
	c := f.person(t, col2, receita, "00000003", "ANA RARA PEIXOTO", "123456")
	f.resolve(t, col2)
	f.resolve(t, col)
	if f.entityOf(t, b) != ent {
		t.Error("resolve moved the split record")
	}
	if f.entityOf(t, a) == f.entityOf(t, b) {
		t.Error("split records were merged again")
	}
	// The split put the key ANA RARA PEIXOTO|123456 on both sides, so it is
	// ambiguous: a new record with it is proposed, not merged with a.
	if f.entityOf(t, c) == f.entityOf(t, a) {
		t.Error("a new record with a key split by an analyst was merged automatically")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE status='pending' AND record_ids=$1`, []int64{a, c}); n != 1 {
		t.Error("no pending proposal for the ambiguous key")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_ambiguous_key WHERE group_key='ANA RARA PEIXOTO|123456'`); n != 1 {
		t.Error("split did not mark the key ambiguous")
	}
}

func TestSplitOfAWholeEntityIsNotAmbiguous(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	if _, err := f.r.Split(ctx, []int64{a}, "analyst@example", ""); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_ambiguous_key`); n != 0 {
		t.Error("a key on one side only was marked ambiguous")
	}
}

func TestRejectSeparatesWhatWasAlreadyMerged(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	b := f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	f.resolve(t, col) // n = 1: merged
	if f.entityOf(t, a) != f.entityOf(t, b) {
		t.Fatal("fixture: a and b should be merged")
	}
	col2 := f.collect(t, receita)
	f.commonName(t, col2, "JOSE DA SILVA") // n = 3, above the threshold
	c := f.person(t, col2, receita, "00000003", "JOSE DA SILVA", "123456")
	f.resolve(t, col2)
	if f.entityOf(t, c) == f.entityOf(t, a) {
		t.Fatal("common name merged")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE status='pending' AND record_ids=$1`, []int64{a, b, c}); n != 1 {
		t.Fatal("no pending proposal with all three records")
	}
	if err := f.r.Decide(ctx, f.proposalID(t), false, "analyst@example"); err != nil {
		t.Fatal(err)
	}
	ents := map[int64]bool{f.entityOf(t, a): true, f.entityOf(t, b): true, f.entityOf(t, c): true}
	if len(ents) != 3 {
		t.Errorf("after reject the records share entities: %v", ents)
	}
	for _, r := range []int64{a, b, c} {
		if c := f.confidence(t, r); c != 1 {
			t.Errorf("confidence of %d = %v, want 1", r, c)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE r.src_entity_id <> p.entity_id`); n != 0 {
		t.Errorf("%d relationships left behind", n)
	}
	if n := f.count(t, `SELECT count(*) FROM person_record WHERE id IN ($1,$2,$3) AND locked_by_decision_id IS NULL`, a, b, c); n != 0 {
		t.Error("rejected records are not locked")
	}
}

func TestDecideActsOnRecordsAsTheyAreNow(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	b := f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	c := f.person(t, col, receita, "00000003", "JOSE DA SILVA", "123456")
	f.resolve(t, col)
	p := f.proposalID(t)
	split, err := f.r.Split(ctx, []int64{c}, "analyst@example", "")
	if err != nil {
		t.Fatal(err)
	}
	var cLock int64
	if err := f.pool.QueryRow(ctx, `SELECT locked_by_decision_id FROM person_record WHERE id=$1`, c).Scan(&cLock); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Decide(ctx, p, true, "analyst@example"); err != nil {
		t.Fatal(err)
	}
	if f.entityOf(t, a) != f.entityOf(t, b) {
		t.Error("the free records were not merged")
	}
	if f.entityOf(t, c) != split || f.count(t, `SELECT count(*) FROM person_record WHERE id=$1 AND locked_by_decision_id=$2`, c, cLock) != 1 {
		t.Error("a record locked after the proposal was moved or re-locked")
	}

	// Fewer than two free records left: confirm refuses, the proposal stays pending.
	g := newFixture(t, 2)
	col = g.collect(t, receita)
	g.commonName(t, col, "JOSE DA SILVA")
	g.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	y := g.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	g.resolve(t, col)
	if _, err := g.r.Split(ctx, []int64{y}, "analyst@example", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.r.Decide(ctx, g.proposalID(t), true, "analyst@example"); !errors.Is(err, identity.ErrProposalStale) {
		t.Errorf("err = %v, want ErrProposalStale", err)
	}
	if n := g.count(t, `SELECT count(*) FROM identity_proposal WHERE status='pending'`); n != 1 {
		t.Error("a refused confirm changed the proposal")
	}
}

func TestAnalystMergeRepointsAndLocks(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "111111")
	b := f.person(t, col, receita, "00000002", "ANA R PEIXOTO", "222222")
	ent, err := f.r.Merge(ctx, []int64{b, a, b}, "analyst@example", "")
	if err != nil {
		t.Fatal(err)
	}
	if ent != f.entityOf(t, a) || f.entityOf(t, b) != ent {
		t.Fatal("merge target must be the entity of the lowest record id")
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE src_record_id IN ($1,$2) AND src_entity_id=$3`, a, b, ent); n != 2 {
		t.Errorf("re-pointed relationships = %d, want 2", n)
	}
	if n := f.count(t, `SELECT count(*) FROM person_record WHERE id IN ($1,$2) AND locked_by_decision_id IS NOT NULL`, a, b); n != 2 {
		t.Errorf("locked records = %d, want 2", n)
	}
}

func TestAnalystOperationsValidate(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "111111")
	b := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "222222")
	cases := map[string]func() error{
		"merge one":        func() error { _, err := f.r.Merge(ctx, []int64{a, a}, "u", ""); return err },
		"merge no user":    func() error { _, err := f.r.Merge(ctx, []int64{a, b}, "", ""); return err },
		"merge missing id": func() error { _, err := f.r.Merge(ctx, []int64{a, 999999}, "u", ""); return err },
		"split none":       func() error { _, err := f.r.Split(ctx, nil, "u", ""); return err },
		"split no user":    func() error { _, err := f.r.Split(ctx, []int64{a}, " ", ""); return err },
		"split missing id": func() error { _, err := f.r.Split(ctx, []int64{a, 999999}, "u", ""); return err },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fn(); err == nil {
				t.Error("want an error")
			}
		})
	}
	if f.entityOf(t, a) == f.entityOf(t, b) {
		t.Error("a failed operation changed state")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision`) +
		f.count(t, `SELECT count(*) FROM person_record WHERE locked_by_decision_id IS NOT NULL`); n != 0 {
		t.Errorf("failed operations left %d decisions/locks", n)
	}
}

func TestRejectedProposalIsNeverReproposed(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	b := f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	f.resolve(t, col)
	p := f.proposalID(t)
	if err := f.r.Decide(ctx, p, false, "analyst@example"); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE id=$1 AND status='rejected'
		AND decided_by='analyst@example' AND decided_at IS NOT NULL`, p); n != 1 {
		t.Fatal("proposal not rejected")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision WHERE kind='analyst' AND action='reject' AND record_ids=$1`,
		[]int64{a, b}); n != 1 {
		t.Error("no reject decision")
	}

	col2 := f.collect(t, receita)
	c := f.person(t, col2, receita, "00000003", "JOSE DA SILVA", "123456")
	d := f.person(t, col2, receita, "00000004", "JOSE DA SILVA", "123456")
	res := f.resolve(t, col2)
	if res.Proposed != 0 || res.AutoMerged != 0 {
		t.Errorf("result = %+v, want nothing", res)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal`); n != 1 {
		t.Errorf("proposals = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE status='rejected' AND record_ids=$1`, []int64{a, b}); n != 1 {
		t.Error("rejected proposal changed")
	}
	ents := map[int64]bool{}
	for _, r := range []int64{a, b, c, d} {
		ents[f.entityOf(t, r)] = true
	}
	if len(ents) != 4 {
		t.Errorf("records of a rejected group share entities: %v", ents)
	}
}

func TestConfirmedProposalSurvivesRecollection(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	b := f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	f.resolve(t, col)
	p := f.proposalID(t)
	if err := f.r.Decide(ctx, p, true, "analyst@example"); err != nil {
		t.Fatal(err)
	}
	ent := f.entityOf(t, a)
	if f.entityOf(t, b) != ent {
		t.Fatal("confirm did not merge")
	}
	if c := f.confidence(t, b); c != 1 {
		t.Errorf("confirmed confidence = %v, want 1", c)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE id=$1 AND status='confirmed'`, p); n != 1 {
		t.Fatal("proposal not confirmed")
	}

	col2 := f.collect(t, tse)
	c := f.person(t, col2, tse, "cand-1", "José da Silva", "123456")
	f.person(t, col2, tse, "cand-2", "JOSE DA SILVA", "333333") // n grows to 4
	res := f.resolve(t, col2)
	if f.entityOf(t, a) != ent || f.entityOf(t, b) != ent {
		t.Error("recollection moved confirmed records")
	}
	if f.entityOf(t, c) != ent {
		t.Error("new record with the confirmed key did not join the confirmed entity")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal`); n != 1 {
		t.Errorf("proposals = %d, want 1", n)
	}
	if res.AutoMerged != 1 || res.Proposed != 0 {
		t.Errorf("result = %+v, want 1 moved", res)
	}
	if again := f.resolve(t, col2); changes(again) != (identity.Result{}) {
		t.Errorf("re-resolve = %+v, want zero", again)
	}
}

func TestDecideOnNonPendingProposal(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	f.resolve(t, col)
	p := f.proposalID(t)
	if err := f.r.Decide(ctx, p, false, ""); err == nil {
		t.Error("decide without user must fail")
	}
	if err := f.r.Decide(ctx, p, false, "analyst@example"); err != nil {
		t.Fatal(err)
	}
	decisions := f.count(t, `SELECT count(*) FROM identity_decision`)
	for _, confirm := range []bool{true, false} {
		if err := f.r.Decide(ctx, p, confirm, "analyst@example"); !errors.Is(err, identity.ErrProposalNotPending) {
			t.Errorf("decide(confirm=%v) on rejected proposal: err = %v, want ErrProposalNotPending", confirm, err)
		}
	}
	if err := f.r.Decide(ctx, 999999, true, "analyst@example"); !errors.Is(err, identity.ErrProposalNotFound) {
		t.Errorf("decide on missing proposal: err = %v, want ErrProposalNotFound", err)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision`); n != decisions {
		t.Error("a refused decide wrote a decision")
	}
	if _, err := f.pool.Exec(ctx, `UPDATE identity_proposal SET status='pending', decided_by=NULL, decided_at=NULL, decision_id=NULL`); err == nil {
		t.Error("a decided proposal could be reopened")
	}
	if _, err := f.pool.Exec(ctx, `UPDATE identity_decision SET reason='x'`); err == nil {
		t.Error("a decision could be changed")
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM identity_decision`); err == nil {
		t.Error("a decision could be deleted")
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM identity_proposal`); err == nil {
		t.Error("a proposal could be deleted")
	}
}

func TestSplitOfAConfirmedKeyStopsAutoJoin(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	a := f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	c := f.person(t, col, receita, "00000003", "JOSE DA SILVA", "123456")
	f.resolve(t, col)
	if err := f.r.Decide(ctx, f.proposalID(t), true, "analyst@example"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Split(ctx, []int64{c}, "analyst@example", "different person"); err != nil {
		t.Fatal(err)
	}
	col2 := f.collect(t, tse)
	d := f.person(t, col2, tse, "cand-1", "JOSE DA SILVA", "123456")
	res := f.resolve(t, col2)
	if e := f.entityOf(t, d); e == f.entityOf(t, a) || e == f.entityOf(t, c) {
		t.Error("a new record joined an entity of a confirmed key that an analyst split")
	}
	if res.AutoMerged != 0 {
		t.Errorf("result = %+v, want nothing moved", res)
	}
}
