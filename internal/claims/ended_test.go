package claims_test

import (
	"errors"
	"testing"
	"time"

	"geoint/internal/claims"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// mandate is a holds_mandate claim from a person record to a public body.
func (f fixture) mandate(t *testing.T, col int64, role, from string) (claims.RelationshipInput, int64) {
	t.Helper()
	body, err := f.s.UpsertEntity(ctx, claims.KindPublicBody, "camara", "Câmara dos Deputados")
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := f.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{SourceID: 1, ContextKey: "dep:1", Name: "MARIA EXEMPLO",
		Prov: claims.Provenance{CollectionID: col, RecordRef: "deputado_1.json:/dados"}})
	if err != nil {
		t.Fatal(err)
	}
	vf := day(from)
	return claims.RelationshipInput{Type: claims.HoldsMandate, DstEntityID: body, SrcRecordID: &rec,
		Attrs: map[string]any{"role": role}, Kind: claims.Fact, ValidFrom: &vf,
		Prov: claims.Provenance{CollectionID: col, RecordRef: "h.json:/dados/1"}}, rec
}

type relRow struct {
	closed            *int64
	lastSeen          *int64
	toAfter, toBefore *time.Time
}

func (f fixture) rel(t *testing.T, id int64) relRow {
	t.Helper()
	var r relRow
	err := f.pool.QueryRow(ctx, `SELECT closed_collection_id, last_seen_collection_id, valid_to_after, valid_to_before
		FROM relationship WHERE id = $1`, id).Scan(&r.closed, &r.lastSeen, &r.toAfter, &r.toBefore)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAssertEndedRelationshipStoresClosedRow(t *testing.T) {
	f := newFixture(t)
	c := f.collect(t)
	in, _ := f.mandate(t, c, "56:PT:MG", "2019-02-01")
	id, err := f.s.AssertEndedRelationship(ctx, in, day("2023-01-31"))
	if err != nil {
		t.Fatal(err)
	}
	r := f.rel(t, id)
	if r.closed == nil || *r.closed != c {
		t.Errorf("closed_collection_id = %v, want %d", r.closed, c)
	}
	if r.toAfter == nil || r.toBefore == nil || !r.toAfter.Equal(day("2023-01-31")) || !r.toBefore.Equal(day("2023-01-31")) {
		t.Errorf("valid_to = %v..%v, want 2023-01-31", r.toAfter, r.toBefore)
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NULL`); n != 0 {
		t.Errorf("%d open rows, want 0", n)
	}
}

func TestAssertEndedRelationshipTwiceIsIdempotent(t *testing.T) {
	f := newFixture(t)
	c1 := f.collect(t)
	in, _ := f.mandate(t, c1, "56:PT:MG", "2019-02-01")
	id1, err := f.s.AssertEndedRelationship(ctx, in, day("2023-01-31"))
	if err != nil {
		t.Fatal(err)
	}
	c2 := f.collect(t)
	in.Prov.CollectionID = c2
	id2, err := f.s.AssertEndedRelationship(ctx, in, day("2023-01-31"))
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("second assert gave id %d, want %d", id2, id1)
	}
	if n := f.count(t, `SELECT count(*) FROM relationship`); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
	r := f.rel(t, id1)
	if r.lastSeen == nil || *r.lastSeen != c2 {
		t.Errorf("last_seen = %v, want %d", r.lastSeen, c2)
	}
	if *r.closed != c1 {
		t.Errorf("closed_collection_id changed to %d", *r.closed)
	}
}

func TestAssertEndedRelationshipClosesTheOpenRow(t *testing.T) {
	f := newFixture(t)
	c1 := f.collect(t)
	in, _ := f.mandate(t, c1, "57:PT:MG", "2023-02-01")
	open, err := f.s.AssertRelationship(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	c2 := f.collect(t)
	in.Prov.CollectionID = c2
	in.Prov.RecordRef = "h.json:/dados/9"
	id, err := f.s.AssertEndedRelationship(ctx, in, day("2026-05-19"))
	if err != nil {
		t.Fatal(err)
	}
	if id != open {
		t.Errorf("ended assert gave id %d, want the open row %d", id, open)
	}
	if n := f.count(t, `SELECT count(*) FROM relationship`); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
	r := f.rel(t, id)
	if r.closed == nil || *r.closed != c2 || !r.toBefore.Equal(day("2026-05-19")) || !r.toAfter.Equal(day("2026-05-19")) {
		t.Errorf("row = closed %v, valid_to %v..%v; want closed by %d at 2026-05-19", r.closed, r.toAfter, r.toBefore, c2)
	}
	if *r.lastSeen != c2 {
		t.Errorf("last_seen = %d, want %d", *r.lastSeen, c2)
	}
}

func TestAssertEndedRelationshipWithAnotherEndAddsARow(t *testing.T) {
	f := newFixture(t)
	c := f.collect(t)
	in, _ := f.mandate(t, c, "56:PT:MG", "2019-02-01")
	a, err := f.s.AssertEndedRelationship(ctx, in, day("2022-01-31"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.s.AssertEndedRelationship(ctx, in, day("2023-01-31"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("a different declared end must be a different claim")
	}
}

func TestAssertEndedRelationshipValidates(t *testing.T) {
	f := newFixture(t)
	c := f.collect(t)
	in, _ := f.mandate(t, c, "56:PT:MG", "2019-02-01")
	in.Prov.RecordRef = ""
	if _, err := f.s.AssertEndedRelationship(ctx, in, day("2023-01-31")); !errors.Is(err, claims.ErrNoProvenance) {
		t.Errorf("no provenance: got %v", err)
	}
	in.Prov.RecordRef = "x:1"
	for _, end := range []string{"2019-02-01", "2019-01-31"} {
		if _, err := f.s.AssertEndedRelationship(ctx, in, day(end)); !errors.Is(err, claims.ErrEmptyPeriod) {
			t.Errorf("end %s with valid_from 2019-02-01: got %v, want ErrEmptyPeriod", end, err)
		}
	}
	bad := int64(999999)
	in.SrcRecordID = &bad
	if _, err := f.s.AssertEndedRelationship(ctx, in, day("2023-01-31")); !errors.Is(err, claims.ErrUnknownRecord) {
		t.Errorf("unknown record: got %v", err)
	}
}

func TestFindPersonRecordByContext(t *testing.T) {
	f := newFixture(t)
	c := f.collect(t)
	_, found, err := f.s.FindPersonRecordByContext(ctx, 1, "dep:1")
	if err != nil || found {
		t.Fatalf("empty: found %v, err %v", found, err)
	}
	_, rec := f.mandate(t, c, "x", "2019-02-01")
	got, found, err := f.s.FindPersonRecordByContext(ctx, 1, "dep:1")
	if err != nil || !found || got != rec {
		t.Fatalf("got %d found %v err %v, want %d", got, found, err, rec)
	}
}
