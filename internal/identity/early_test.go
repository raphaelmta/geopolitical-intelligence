package identity_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/identity"
)

// Tests of ResolveRecords, the early pass a loader runs between its person
// records and its relationships (task 3 of
// docs/performance/2026-load-optimization-design.md).

// record creates a person record with its own entity and no relationship.
func (f fixture) record(t *testing.T, col int64, source int16, ctxKey, name, frag string) int64 {
	t.Helper()
	rec, _, err := f.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{
		SourceID: source, ContextKey: ctxKey, Name: name, CPFFragment: frag,
		Prov: claims.Provenance{CollectionID: col, RecordRef: fmt.Sprintf("test:%s:%s:%s", ctxKey, name, frag)}})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// relate writes the partner_of claim of rec to the company of its context,
// as collection col: the entity of rec is read when the claim is written.
func (f fixture) relate(t *testing.T, col, rec int64, ctxKey string) {
	t.Helper()
	company, err := f.s.UpsertEntity(ctx, claims.KindCompany, "c"+ctxKey, "EMPRESA "+ctxKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: claims.PartnerOf,
		DstEntityID: company, SrcRecordID: &rec, Kind: claims.Fact,
		Prov: claims.Provenance{CollectionID: col, RecordRef: "test:rel:" + ctxKey}}); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) resolveRecords(t *testing.T, col int64) identity.Result {
	t.Helper()
	res, err := f.r.ResolveRecords(ctx, col)
	if err != nil {
		t.Fatalf("resolve records %d: %v", col, err)
	}
	return res
}

// stalePointers counts the relationship ends that differ from their record's
// current entity.
func (f fixture) stalePointers(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM relationship r
		LEFT JOIN person_record s ON s.id = r.src_record_id LEFT JOIN person_record d ON d.id = r.dst_record_id
		WHERE s.entity_id <> r.src_entity_id OR d.entity_id <> r.dst_entity_id`)
}

// earlyPeople are the records of the comparison below: a group that merges,
// a name above the threshold (two groups, proposed) and a lone record.
var earlyPeople = []struct{ ctxKey, name, frag string }{
	{"00000001", "ANA RARA PEIXOTO", "123456"},
	{"00000002", "ANA RARA PEIXOTO", "123456"},
	{"00000003", "ANA RARA PEIXOTO", "123456"},
	{"00000004", "JOSE DA SILVA", "111111"},
	{"00000005", "JOSE DA SILVA", "111111"},
	{"00000006", "JOSE DA SILVA", "222222"},
	{"00000007", "JOSE DA SILVA", "222222"},
	{"00000008", "JOSE DA SILVA", "333333"},
	{"00000009", "BRUNO SOZINHO", "654321"},
}

// The new order (records, ResolveRecords, relationships, Resolve) ends in
// the state of the old one (records, relationships, Resolve): same entities,
// confidences, proposals with their connections, relationship ends and
// decisions. In the new order the final Resolve moves nothing.
func TestResolveRecordsThenResolveMatchesResolveAlone(t *testing.T) {
	const decisions = `SELECT coalesce(string_agg(format('%s:%s:%s:%s:%s:%s', kind, action, record_ids, entity_id, confidence, reason),
		' ' ORDER BY record_ids), '') FROM identity_decision`
	state := func(f fixture) string {
		var d string
		if err := f.pool.QueryRow(ctx, decisions).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return snapshot(t, f) + " / " + d
	}

	old := newFixture(t, 2)
	colOld := old.collect(t, receita)
	var recs []int64
	for _, p := range earlyPeople {
		recs = append(recs, old.record(t, colOld, receita, p.ctxKey, p.name, p.frag))
	}
	for i, p := range earlyPeople {
		old.relate(t, colOld, recs[i], p.ctxKey)
	}
	resOld := old.resolve(t, colOld)
	if resOld.AutoMerged != 2 || resOld.ProposalsCreated != 2 {
		t.Fatalf("fixture: old order = %+v, want 2 records moved and 2 proposals", resOld)
	}

	f := newFixture(t, 2)
	col := f.collect(t, receita)
	recs = recs[:0]
	for _, p := range earlyPeople {
		recs = append(recs, f.record(t, col, receita, p.ctxKey, p.name, p.frag))
	}
	early := f.resolveRecords(t, col)
	if changes(early) != changes(resOld) {
		t.Errorf("early pass = %+v, want what Resolve alone reports: %+v", early, resOld)
	}
	if early.NamesByFragments != resOld.NamesByFragments {
		t.Errorf("early pass fragment distribution = %v, want %v", early.NamesByFragments, resOld.NamesByFragments)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_resolve_done`); n != 0 {
		t.Error("the early pass wrote the completion marker")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_proposal WHERE connections <> 0`); n != 0 {
		t.Errorf("%d proposals with connections before any relationship exists", n)
	}
	for i, p := range earlyPeople {
		f.relate(t, col, recs[i], p.ctxKey)
	}
	if n := f.stalePointers(t); n != 0 {
		t.Fatalf("%d relationship ends written at an entity their record had left", n)
	}
	final := f.resolve(t, col)
	if final.AutoMerged != 0 || final.AutoMergedGroups != 0 || final.ProposalsCreated != 0 {
		t.Errorf("final Resolve after the early pass = %+v, want nothing moved and no proposal created", final)
	}
	if final.Proposed != 2 {
		t.Errorf("final Resolve refreshed %d proposals, want 2 (their connections)", final.Proposed)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_resolve_done WHERE collection_id = $1`, col); n != 1 {
		t.Error("the final Resolve did not write the completion marker")
	}
	if got, want := state(f), state(old); got != want {
		t.Errorf("new order differs from the old one:\n%s\n---\n%s", got, want)
	}
	// And a re-run of either pass changes nothing.
	before := state(f)
	if res := f.resolveRecords(t, col); changes(res) != (identity.Result{}) {
		t.Errorf("early pass re-run = %+v, want no change", res)
	}
	if res := f.resolve(t, col); changes(res) != (identity.Result{}) {
		t.Errorf("Resolve re-run = %+v, want no change", res)
	}
	if after := state(f); after != before {
		t.Errorf("re-runs wrote:\n%s\n---\n%s", before, after)
	}
}

// The early pass uses the resolver's threshold, like Resolve: a name with
// two fragments merges at threshold 2 and is proposed at threshold 1.
func TestResolveRecordsUsesTheConfiguredThreshold(t *testing.T) {
	for _, tc := range []struct {
		threshold        int
		merged, proposed int64
	}{{2, 2, 0}, {1, 0, 2}} {
		f := newFixture(t, tc.threshold)
		col := f.collect(t, receita)
		f.record(t, col, receita, "00000001", "JOSE DA SILVA", "111111")
		f.record(t, col, receita, "00000002", "JOSE DA SILVA", "111111")
		f.record(t, col, receita, "00000003", "JOSE DA SILVA", "222222")
		f.record(t, col, receita, "00000004", "JOSE DA SILVA", "222222")
		res := f.resolveRecords(t, col)
		if res.AutoMerged != tc.merged || res.ProposalsCreated != tc.proposed {
			t.Errorf("threshold %d: %+v, want %d merged and %d proposals", tc.threshold, res, tc.merged, tc.proposed)
		}
	}
	f := newFixture(t, -1)
	if _, err := f.r.ResolveRecords(ctx, f.collect(t, receita)); err == nil {
		t.Error("a negative threshold was accepted")
	}
}

// A record of an earlier collection that the early pass moves has
// relationships already: they are re-pointed in the same batch, as in
// Resolve. Only records first seen by the collection being loaded are left
// out of the re-point.
func TestResolveRecordsRepointsRecordsOfEarlierCollections(t *testing.T) {
	f := newFixture(t, 0)
	// An earlier load that failed before its Resolve: two records of one
	// person, with relationships, in two entities.
	failed := f.collect(t, receita)
	a1 := f.person(t, failed, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	a2 := f.person(t, failed, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	if err := collection.Fail(ctx, f.pool, failed, errors.New("load failed")); err != nil {
		t.Fatal(err)
	}
	col := f.collect(t, receita)
	a3 := f.record(t, col, receita, "00000003", "ANA RARA PEIXOTO", "123456")
	if res := f.resolveRecords(t, col); res.AutoMerged != 2 {
		t.Fatalf("early pass = %+v, want 2 records moved (one of the failed collection, the new one)", res)
	}
	target := f.entityOf(t, a1)
	if f.entityOf(t, a2) != target || f.entityOf(t, a3) != target {
		t.Fatal("the three records are not in the entity of the first")
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE src_record_id = $1 AND src_entity_id = $2`, a2, target); n != 1 {
		t.Error("the relationship of the moved earlier record was not re-pointed by the early pass")
	}
	if n := f.stalePointers(t); n != 0 {
		t.Errorf("%d stale relationship ends after the early pass", n)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_resolve_done`); n != 0 {
		t.Error("the early pass wrote a completion marker")
	}
}

// If the collection has already written relationships when ResolveRecords
// runs (not the place a loader calls it from), nothing is left out: every
// moved record is re-pointed in its batch.
func TestResolveRecordsRepointsWhenTheCollectionWroteRelationships(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a1 := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	a2 := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	if res := f.resolveRecords(t, col); res.AutoMerged != 1 {
		t.Fatalf("early pass = %+v, want 1 record moved", res)
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE src_record_id = $1 AND src_entity_id = $2`, a2, f.entityOf(t, a1)); n != 1 {
		t.Error("the relationship of the moved record was not re-pointed")
	}
	if n := f.stalePointers(t); n != 0 {
		t.Errorf("%d stale relationship ends", n)
	}
}

// What the early pass leaves out, shown on a state no loader produces: a
// relationship written by ANOTHER collection that cites a record first seen
// by the collection being resolved, before that collection wrote any
// relationship. (A record is found by its source's key and loads of one
// source never overlap, so until the collection that created a record writes
// its relationships none cites it.) The early pass does not look for it;
// Resolve, which never leaves anything out, does.
func TestResolveRecordsLeavesOutOnlyNewRecords(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	other := f.collect(t, tse)
	a1 := f.record(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	a2 := f.record(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	f.relate(t, other, a2, "00000002")
	if res := f.resolveRecords(t, col); res.AutoMerged != 1 {
		t.Fatalf("early pass = %+v, want 1 record moved", res)
	}
	if f.entityOf(t, a1) != f.entityOf(t, a2) {
		t.Fatal("the records were not merged")
	}
	if n := f.stalePointers(t); n != 1 {
		t.Fatalf("%d stale ends, want 1: the early pass re-points no record first seen by its collection", n)
	}
	// The other collection's Resolve sweeps its own relationships.
	f.resolve(t, other)
	if n := f.stalePointers(t); n != 0 {
		t.Errorf("%d stale ends after the Resolve of the collection that wrote the relationship", n)
	}
}

// Resolve itself never leaves a record out of the re-point: a loader without
// an early pass writes the relationships of its new records first.
func TestResolveRepointsRecordsOfItsOwnCollection(t *testing.T) {
	f := newFixture(t, 0)
	col := f.collect(t, receita)
	a1 := f.person(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	a2 := f.person(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	// Stop after the first batch (the cancelled context fails the next
	// transaction), so only the batch's re-point can have done it: the sweep
	// never runs.
	identity.SetNamesPerBatch(t, 1)
	cctx, cancel := context.WithCancel(ctx)
	identity.SetAfterBatch(t, cancel)
	if _, err := f.r.Resolve(cctx, col); err == nil {
		t.Fatal("resolve should fail after its first batch")
	}
	if f.entityOf(t, a1) != f.entityOf(t, a2) {
		t.Fatal("the first batch did not merge the records")
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE src_record_id = $1 AND src_entity_id = $2`, a2, f.entityOf(t, a1)); n != 1 {
		t.Error("the batch of Resolve did not re-point the relationship of a record of its own collection")
	}
	if n := f.count(t, `SELECT count(*) FROM identity_resolve_done`); n != 0 {
		t.Error("completion marker written although the sweep failed")
	}
}

// A load that stops between the two passes (records merged by the early
// pass, collection failed, no completion marker) is completed by the Resolve
// of the next collection.
func TestEarlyPassWithoutResolveIsCompletedByTheNext(t *testing.T) {
	f := newFixture(t, 2)
	col := f.collect(t, receita)
	a1 := f.record(t, col, receita, "00000001", "ANA RARA PEIXOTO", "123456")
	a2 := f.record(t, col, receita, "00000002", "ANA RARA PEIXOTO", "123456")
	j1 := f.record(t, col, receita, "00000004", "JOSE DA SILVA", "111111")
	j2 := f.record(t, col, receita, "00000005", "JOSE DA SILVA", "111111")
	f.record(t, col, receita, "00000006", "JOSE DA SILVA", "222222")
	f.record(t, col, receita, "00000008", "JOSE DA SILVA", "333333")
	f.resolveRecords(t, col)
	// One relationship chunk is written, then the load dies.
	f.relate(t, col, a2, "00000002")
	f.relate(t, col, j1, "00000004")
	if err := collection.Fail(ctx, f.pool, col, errors.New("killed between the passes")); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_resolve_done`); n != 0 {
		t.Fatal("a collection that never finished its Resolve is marked")
	}

	// The repeat: the records exist already, the rest of the relationships
	// are written by the new collection.
	next := f.collect(t, receita)
	f.resolveRecords(t, next)
	f.relate(t, next, a1, "00000001")
	f.relate(t, next, j2, "00000005")
	res := f.resolve(t, next)
	if res.Recovered != 1 {
		t.Errorf("recovered = %d, want the failed collection", res.Recovered)
	}
	if res.AutoMerged != 0 {
		t.Errorf("the recovery moved %d records, want 0: the early pass had merged them", res.AutoMerged)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_resolve_done WHERE collection_id IN ($1, $2)`, col, next); n != 2 {
		t.Errorf("resolve markers = %d, want 2", n)
	}
	if f.entityOf(t, a1) != f.entityOf(t, a2) {
		t.Error("the merged records came apart")
	}
	if n := f.stalePointers(t); n != 0 {
		t.Errorf("%d stale relationship ends", n)
	}
	// The proposal of JOSE|111111 counts both relationships, one written by
	// each collection.
	if n := f.count(t, `SELECT connections FROM identity_proposal WHERE group_key = 'JOSE DA SILVA|111111' AND status = 'pending'`); n != 2 {
		t.Errorf("connections = %d, want 2", n)
	}
	if n := f.count(t, `SELECT count(*) FROM identity_decision WHERE kind = 'auto'`); n != 1 {
		t.Errorf("%d automatic decisions, want 1 (the early pass's merge, not repeated)", n)
	}
}
