package identity_test

import (
	"errors"
	"testing"
	"time"

	"geoint/internal/identity"
)

// proposalOfThree writes three records of a common name (each in its own
// entity) and the pending proposal to join them.
func proposalOfThree(t *testing.T) (f fixture, p, a, b, c int64) {
	t.Helper()
	f = newFixture(t, 2)
	col := f.collect(t, receita)
	f.commonName(t, col, "JOSE DA SILVA")
	a = f.person(t, col, receita, "00000001", "JOSE DA SILVA", "123456")
	b = f.person(t, col, receita, "00000002", "JOSE DA SILVA", "123456")
	c = f.person(t, col, receita, "00000003", "JOSE DA SILVA", "123456")
	f.resolve(t, col)
	return f, f.proposalID(t), a, b, c
}

// unchanged fails unless the identity state is still before and no
// decision was written since decisions were counted.
func unchanged(t *testing.T, f fixture, before string, decisions int) {
	t.Helper()
	if after := snapshot(t, f); after != before {
		t.Errorf("a refused operation changed state:\nbefore %s\nafter  %s", before, after)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision`); n != decisions {
		t.Errorf("a refused operation wrote %d decisions", n-decisions)
	}
}

func target(id int64) identity.DecisionExpectation {
	return identity.DecisionExpectation{CheckTarget: true, TargetEntityID: id}
}

func TestDecideWithMatchingExpectationsConfirms(t *testing.T) {
	f, p, a, b, c := proposalOfThree(t)
	exp := target(f.entityOf(t, a))
	exp.RecordIDs = []int64{c, a, b}
	if err := f.r.DecideExpecting(ctx, p, true, "analyst@example", exp); err != nil {
		t.Fatal(err)
	}
	if f.entityOf(t, b) != f.entityOf(t, a) || f.entityOf(t, c) != f.entityOf(t, a) {
		t.Error("the records were not merged")
	}
}

func TestDecideWithAnotherTargetDoesNothing(t *testing.T) {
	f, p, a, b, c := proposalOfThree(t)
	before, decisions := snapshot(t, f), f.count(t, `SELECT count(*) FROM identity_decision`)
	for name, exp := range map[string]identity.DecisionExpectation{
		"other entity":   target(f.entityOf(t, b)),
		"no target seen": target(0),
		"other with ids": {CheckTarget: true, TargetEntityID: f.entityOf(t, c), RecordIDs: []int64{a, b, c}},
	} {
		if err := f.r.DecideExpecting(ctx, p, true, "analyst@example", exp); !errors.Is(err, identity.ErrExpectationFailed) {
			t.Errorf("%s: err = %v, want ErrExpectationFailed", name, err)
		}
	}
	unchanged(t, f, before, decisions)
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE id=$1 AND status='pending'`, p); n != 1 {
		t.Error("the proposal is no longer pending")
	}
}

func TestDecideWithStaleRecordIDsDoesNothing(t *testing.T) {
	f, p, a, b, c := proposalOfThree(t)
	seenTarget := f.entityOf(t, a)
	// c is split away (and locked) after the analyst loaded the queue.
	if _, err := f.r.Split(ctx, []int64{c}, "analyst@example", ""); err != nil {
		t.Fatal(err)
	}
	before, decisions := snapshot(t, f), f.count(t, `SELECT count(*) FROM identity_decision`)
	for _, confirm := range []bool{true, false} {
		exp := target(seenTarget)
		exp.RecordIDs = []int64{a, b, c}
		if err := f.r.DecideExpecting(ctx, p, confirm, "analyst@example", exp); !errors.Is(err, identity.ErrExpectationFailed) {
			t.Errorf("confirm=%v: err = %v, want ErrExpectationFailed", confirm, err)
		}
	}
	unchanged(t, f, before, decisions)
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE id=$1 AND status='pending'`, p); n != 1 {
		t.Error("the proposal is no longer pending")
	}
	// What is on screen now: a and b free.
	exp := target(seenTarget)
	exp.RecordIDs = []int64{b, a}
	if err := f.r.DecideExpecting(ctx, p, true, "analyst@example", exp); err != nil {
		t.Fatalf("with the current records: %v", err)
	}
}

func TestRejectWithExpectations(t *testing.T) {
	f, p, a, b, c := proposalOfThree(t)
	before, decisions := snapshot(t, f), f.count(t, `SELECT count(*) FROM identity_decision`)
	// A record set that is not the free one: refused.
	if err := f.r.DecideExpecting(ctx, p, false, "analyst@example",
		identity.DecisionExpectation{RecordIDs: []int64{a, b}}); !errors.Is(err, identity.ErrExpectationFailed) {
		t.Fatalf("err = %v, want ErrExpectationFailed", err)
	}
	unchanged(t, f, before, decisions)
	// The target is ignored by a reject (it moves nothing into an entity).
	exp := target(999999)
	exp.RecordIDs = []int64{a, b, c}
	if err := f.r.DecideExpecting(ctx, p, false, "analyst@example", exp); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE id=$1 AND status='rejected'`, p); n != 1 {
		t.Error("proposal not rejected")
	}
}

func TestDecideWithoutExpectationsIsUnchanged(t *testing.T) {
	f, p, a, b, _ := proposalOfThree(t)
	if err := f.r.DecideExpecting(ctx, p, true, "analyst@example", identity.DecisionExpectation{}); err != nil {
		t.Fatal(err)
	}
	if f.entityOf(t, a) != f.entityOf(t, b) {
		t.Error("not merged")
	}
}

// The expectation is evaluated after the identity lock: a change committed
// by a transaction that held the lock while Decide waited is seen.
func TestDecideExpectationIsCheckedAfterTheLock(t *testing.T) {
	f, p, a, _, c := proposalOfThree(t)
	seen := f.entityOf(t, a)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := identity.LockTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	// Under the lock, a moves into c's entity: the target changes.
	if _, err := tx.Exec(ctx, `UPDATE person_record SET entity_id = $2 WHERE id = $1`, a, f.entityOf(t, c)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.r.DecideExpecting(ctx, p, true, "analyst@example", target(seen)) }()
	waitBlockedBy(t, f, pid)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, identity.ErrExpectationFailed) {
		t.Fatalf("err = %v, want ErrExpectationFailed", err)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE id=$1 AND status='pending'`, p); n != 1 {
		t.Error("the proposal is no longer pending")
	}
}

// waitBlockedBy waits until some session waits on a lock held by pid.
func waitBlockedBy(t *testing.T, f fixture, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f.count(t, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid))`, pid) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the operation never waited for the lock")
}

// entities writes four records of different people: a and b in one entity
// (e1), c alone (e2), d alone (e3).
func entities(t *testing.T) (f fixture, a, b, c, d, e1, e2, e3 int64) {
	t.Helper()
	f = newFixture(t, 0)
	col := f.collect(t, receita)
	a = f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "111111")
	b = f.person(t, col, receita, "00000002", "ANA R PEIXOTO", "222222")
	c = f.person(t, col, receita, "00000003", "ANA PEIXOTO", "333333")
	d = f.person(t, col, receita, "00000004", "BIA PEIXOTO", "444444")
	e1, e2, e3 = f.entityOf(t, a), f.entityOf(t, c), f.entityOf(t, d)
	f.move(t, b, e1)
	return f, a, b, c, d, e1, e2, e3
}

// move puts rec in entity (as a later automatic merge would) and re-points
// its relationships.
func (f fixture) move(t *testing.T, rec, entity int64) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `UPDATE person_record SET entity_id = $2 WHERE id = $1`, rec, entity); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE relationship SET src_entity_id = $2 WHERE src_record_id = $1`, rec, entity); err != nil {
		t.Fatal(err)
	}
}

func TestMergeExpectingExactCover(t *testing.T) {
	f, a, b, c, _, e1, e2, _ := entities(t)
	ent, err := f.r.MergeExpecting(ctx, []int64{a, b, c}, []int64{e2, e1}, "analyst@example", "same person")
	if err != nil {
		t.Fatal(err)
	}
	if ent != e1 || f.entityOf(t, c) != e1 || f.entityOf(t, b) != e1 {
		t.Fatalf("merged into %d", ent)
	}
}

func TestMergeExpectingRefusesAChangedPicture(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(t *testing.T, f fixture, a, b, c, d, e1, e2, e3 int64)
		ids    func(a, b, c, d int64) []int64
	}{
		"a record left one entity": {
			change: func(t *testing.T, f fixture, _, b, _, _, _, _, e3 int64) { f.move(t, b, e3) },
			ids:    func(a, b, c, _ int64) []int64 { return []int64{a, b, c} },
		},
		"a record joined one entity": {
			change: func(t *testing.T, f fixture, _, _, _, d, _, e2, _ int64) { f.move(t, d, e2) },
			ids:    func(a, b, c, _ int64) []int64 { return []int64{a, b, c} },
		},
		"a record of a third entity": {
			ids: func(a, b, c, d int64) []int64 { return []int64{a, b, c, d} },
		},
		"a record left behind": {
			ids: func(a, _, c, _ int64) []int64 { return []int64{a, c} },
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, b, c, d, e1, e2, e3 := entities(t)
			if tc.change != nil {
				tc.change(t, f, a, b, c, d, e1, e2, e3)
			}
			before, decisions := snapshot(t, f), f.count(t, `SELECT count(*) FROM identity_decision`)
			_, err := f.r.MergeExpecting(ctx, tc.ids(a, b, c, d), []int64{e1, e2}, "analyst@example", "same person")
			if !errors.Is(err, identity.ErrExpectationFailed) {
				t.Fatalf("err = %v, want ErrExpectationFailed", err)
			}
			unchanged(t, f, before, decisions)
			if n := f.count(t, `SELECT count(*) FROM person_record WHERE locked_by_decision_id IS NOT NULL`); n != 0 {
				t.Errorf("%d records locked", n)
			}
		})
	}
}

func TestSplitExpecting(t *testing.T) {
	f, a, b, _, _, e1, e2, _ := entities(t)
	before, decisions := snapshot(t, f), f.count(t, `SELECT count(*) FROM identity_decision`)
	if _, err := f.r.SplitExpecting(ctx, []int64{b}, e2, "analyst@example", "x"); !errors.Is(err, identity.ErrExpectationFailed) {
		t.Errorf("wrong entity: err = %v, want ErrExpectationFailed", err)
	}
	if _, err := f.r.SplitExpecting(ctx, []int64{a, b}, e1, "analyst@example", "x"); !errors.Is(err, identity.ErrExpectationFailed) {
		t.Errorf("every record: err = %v, want ErrExpectationFailed", err)
	}
	unchanged(t, f, before, decisions)
	ent, err := f.r.SplitExpecting(ctx, []int64{b}, e1, "analyst@example", "x")
	if err != nil {
		t.Fatal(err)
	}
	if ent == e1 || f.entityOf(t, b) != ent || f.entityOf(t, a) != e1 {
		t.Fatalf("split into %d", ent)
	}
}
