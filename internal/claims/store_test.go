package claims_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/testutil"
)

var ctx = context.Background()

// fixture is a migrated schema with a store and the Receita source.
type fixture struct {
	pool *pgxpool.Pool
	s    *claims.Store
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	return fixture{pool: pool, s: claims.NewStore(pool)}
}

func (f fixture) collect(t *testing.T) int64 {
	t.Helper()
	c, err := collection.Begin(ctx, f.pool, 1, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func (f fixture) company(t *testing.T, key string) int64 {
	t.Helper()
	id, err := f.s.UpsertEntity(ctx, claims.KindCompany, key, "ACME LTDA")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRelationshipWithoutProvenanceIsRejected(t *testing.T) {
	f := newFixture(t)
	a, b := f.company(t, "00000001"), f.company(t, "00000002")
	c := f.collect(t)
	for name, prov := range map[string]claims.Provenance{
		"empty":         {},
		"no ref":        {CollectionID: c},
		"no collection": {RecordRef: "row-1"},
	} {
		_, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: "partner_of", SrcEntityID: a, DstEntityID: b, Kind: "fact", Prov: prov})
		if !errors.Is(err, claims.ErrNoProvenance) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestInferenceWithoutConfidenceIsRejected(t *testing.T) {
	f := newFixture(t)
	a, b := f.company(t, "00000001"), f.company(t, "00000002")
	c := f.collect(t)
	_, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{
		Type: "controls", SrcEntityID: a, DstEntityID: b, Kind: "inference",
		Prov: claims.Provenance{CollectionID: c, RecordRef: "r"},
	})
	if !errors.Is(err, claims.ErrInferenceNeedsConfidence) {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteIsForbiddenByDatabase(t *testing.T) {
	f := newFixture(t)
	a, b := f.company(t, "00000001"), f.company(t, "00000002")
	c := f.collect(t)
	if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{
		Type: "controls", SrcEntityID: a, DstEntityID: b, Kind: "fact",
		Prov: claims.Provenance{CollectionID: c, RecordRef: "r"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{SourceID: 1, ContextKey: "00000001", Name: "Maria", Prov: claims.Provenance{CollectionID: c, RecordRef: "p"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutCompanyVersion(ctx, claims.CompanyVersionInput{EntityID: a, Name: "ACME", Prov: claims.Provenance{CollectionID: c, RecordRef: "v"}}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"relationship", "entity", "person_record", "company_version", "collection"} {
		_, err := f.pool.Exec(ctx, "DELETE FROM "+table)
		if err == nil || !strings.Contains(err.Error(), "never deleted") {
			t.Errorf("DELETE FROM %s: got %v", table, err)
		}
		_, err = f.pool.Exec(ctx, "TRUNCATE "+table+" CASCADE")
		if err == nil || !strings.Contains(err.Error(), "never deleted") {
			t.Errorf("TRUNCATE %s: got %v", table, err)
		}
	}
}

func TestProvenanceConstraintHoldsForRawSQL(t *testing.T) {
	f := newFixture(t)
	a, b := f.company(t, "00000001"), f.company(t, "00000002")
	c := f.collect(t)
	_, err := f.pool.Exec(ctx, `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, kind, first_collection_id, record_ref)
		VALUES ('partner_of', $1, $2, 'fact', $3, NULL)`, a, b, c)
	if err == nil || !strings.Contains(err.Error(), "null value") {
		t.Fatalf("record_ref NULL: got %v", err)
	}
	_, err = f.pool.Exec(ctx, `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, kind, first_collection_id, record_ref)
		VALUES ('partner_of', $1, $2, 'fact', NULL, 'x')`, a, b)
	if err == nil || !strings.Contains(err.Error(), "null value") {
		t.Fatalf("first_collection_id NULL: got %v", err)
	}
	_, err = f.pool.Exec(ctx, `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, kind, first_collection_id, record_ref)
		VALUES ('controls', $1, $2, 'inference', $3, 'x')`, a, b, c)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("inference without confidence: got %v", err)
	}
}

func TestAssertSameRelationshipTwiceIsIdempotent(t *testing.T) {
	f := newFixture(t)
	person, company := f.company(t, "00000001"), f.company(t, "00000002")
	c1, c2 := f.collect(t), f.collect(t)
	in := claims.RelationshipInput{
		Type: "partner_of", SrcEntityID: person, DstEntityID: company, Kind: "fact",
		Attrs: map[string]any{"role": "49"},
	}
	in.Prov = claims.Provenance{CollectionID: c1, RecordRef: "row-1"}
	id1, err := f.s.AssertRelationship(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Prov = claims.Provenance{CollectionID: c2, RecordRef: "row-1"}
	id2, err := f.s.AssertRelationship(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("ids differ: %d vs %d", id1, id2)
	}
	if n := f.count(t, "SELECT count(*) FROM relationship"); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	var first, last int64
	if err := f.pool.QueryRow(ctx, "SELECT first_collection_id, last_seen_collection_id FROM relationship WHERE id=$1", id1).Scan(&first, &last); err != nil {
		t.Fatal(err)
	}
	if first != c1 || last != c2 {
		t.Fatalf("first=%d last=%d, want %d/%d", first, last, c1, c2)
	}

	// A different role is a different relationship.
	in.Attrs = map[string]any{"role": "22"}
	id3, err := f.s.AssertRelationship(ctx, in)
	if err != nil || id3 == id1 {
		t.Fatalf("different role: id=%d err=%v", id3, err)
	}
}

func TestAssertRelationshipWithoutRoleOrValidFromIsIdempotent(t *testing.T) {
	f := newFixture(t)
	a, b := f.company(t, "00000001"), f.company(t, "00000002")
	c1, c2 := f.collect(t), f.collect(t)
	in := claims.RelationshipInput{Type: "controls", SrcEntityID: a, DstEntityID: b, Kind: "fact"}
	in.Prov = claims.Provenance{CollectionID: c1, RecordRef: "r"}
	if _, err := f.s.AssertRelationship(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.Prov.CollectionID = c2
	if _, err := f.s.AssertRelationship(ctx, in); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM relationship"); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestCompanyVersionChangeClosesOldAndKeepsIt(t *testing.T) {
	f := newFixture(t)
	e := f.company(t, "00000001")
	c1, c2, c3 := f.collect(t), f.collect(t), f.collect(t)
	capital := 1000.50
	in := claims.CompanyVersionInput{EntityID: e, Name: "ACME", UF: "SP", Capital: &capital, Prov: claims.Provenance{CollectionID: c1, RecordRef: "v"}}
	if err := f.s.PutCompanyVersion(ctx, in); err != nil {
		t.Fatal(err)
	}
	// Unchanged in c2: still one row, seen again.
	in.Prov.CollectionID = c2
	if err := f.s.PutCompanyVersion(ctx, in); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM company_version"); n != 1 {
		t.Fatalf("rows after unchanged = %d, want 1", n)
	}
	var last int64
	if err := f.pool.QueryRow(ctx, "SELECT last_seen_collection_id FROM company_version").Scan(&last); err != nil || last != c2 {
		t.Fatalf("last_seen = %d err=%v, want %d", last, err, c2)
	}
	// Name changes in c3.
	in.Name = "ACME RENAMED"
	in.Prov.CollectionID = c3
	if err := f.s.PutCompanyVersion(ctx, in); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM company_version"); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
	if n := f.count(t, "SELECT count(*) FROM company_version WHERE name='ACME' AND closed_collection_id=$1 AND first_collection_id=$2", c3, c1); n != 1 {
		t.Fatal("old version not closed by c3 / not kept")
	}
	if n := f.count(t, "SELECT count(*) FROM company_version WHERE name='ACME RENAMED' AND closed_collection_id IS NULL AND first_collection_id=$1", c3); n != 1 {
		t.Fatal("new version not open")
	}
}

func TestPutCompanyVersionRequiresProvenance(t *testing.T) {
	f := newFixture(t)
	e := f.company(t, "00000001")
	err := f.s.PutCompanyVersion(ctx, claims.CompanyVersionInput{EntityID: e, Name: "ACME"})
	if !errors.Is(err, claims.ErrNoProvenance) {
		t.Fatalf("got %v", err)
	}
}

func TestUpsertEntityIsIdempotentPerKindAndKey(t *testing.T) {
	f := newFixture(t)
	a, err := f.s.UpsertEntity(ctx, claims.KindCompany, "00000001", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.s.UpsertEntity(ctx, claims.KindCompany, "00000001", "ACME OTHER NAME")
	if err != nil || a != b {
		t.Fatalf("same key: %d vs %d err=%v", a, b, err)
	}
	c, err := f.s.UpsertEntity(ctx, claims.KindParty, "00000001", "PARTY")
	if err != nil || c == a {
		t.Fatalf("different kind: %d vs %d err=%v", c, a, err)
	}
	if _, err := f.s.UpsertEntity(ctx, claims.KindCompany, "", "X"); err == nil {
		t.Fatal("empty natural key accepted")
	}
}

func TestUpsertPersonRecordCreatesSingletonEntity(t *testing.T) {
	f := newFixture(t)
	c := f.collect(t)
	in := claims.PersonRecordInput{SourceID: 1, ContextKey: "00000001", Name: "Maria  da Silva", CPFFragment: "***123456**", Prov: claims.Provenance{CollectionID: c, RecordRef: "row-7"}}
	rec, ent, err := f.s.UpsertPersonRecord(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	var key *string
	if err := f.pool.QueryRow(ctx, "SELECT kind, natural_key FROM entity WHERE id=$1", ent).Scan(&kind, &key); err != nil {
		t.Fatal(err)
	}
	if kind != "person" || key != nil {
		t.Fatalf("entity kind=%q key=%v", kind, key)
	}
	var conf float32
	var entityID int64
	if err := f.pool.QueryRow(ctx, "SELECT identity_confidence, entity_id FROM person_record WHERE id=$1", rec).Scan(&conf, &entityID); err != nil {
		t.Fatal(err)
	}
	if conf != 1 || entityID != ent {
		t.Fatalf("confidence=%v entity=%d want 1/%d", conf, entityID, ent)
	}

	// Same record again: same ids, no new rows.
	rec2, ent2, err := f.s.UpsertPersonRecord(ctx, in)
	if err != nil || rec2 != rec || ent2 != ent {
		t.Fatalf("second upsert: %d/%d err=%v", rec2, ent2, err)
	}
	if n := f.count(t, "SELECT count(*) FROM entity WHERE kind='person'"); n != 1 {
		t.Fatalf("person entities = %d, want 1", n)
	}
	// Same name in another context is another record.
	in.ContextKey = "00000002"
	rec3, ent3, err := f.s.UpsertPersonRecord(ctx, in)
	if err != nil || rec3 == rec || ent3 == ent {
		t.Fatalf("other context: %d/%d err=%v", rec3, ent3, err)
	}
}

func TestUpsertPersonRecordEmptyNameIsStoredAsEmptyString(t *testing.T) {
	f := newFixture(t)
	c := f.collect(t)
	in := claims.PersonRecordInput{SourceID: 1, ContextKey: "00000001", Prov: claims.Provenance{CollectionID: c, RecordRef: "row-1"}}
	rec, _, err := f.s.UpsertPersonRecord(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	rec2, _, err := f.s.UpsertPersonRecord(ctx, in)
	if err != nil || rec2 != rec {
		t.Fatalf("got %d err=%v, want %d", rec2, err, rec)
	}
	if n := f.count(t, "SELECT count(*) FROM person_record WHERE name_norm='' AND cpf_fragment='' AND name=''"); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestOnlyPointerColumnsMayBeUpdated(t *testing.T) {
	f := newFixture(t)
	a, b := f.company(t, "00000001"), f.company(t, "00000002")
	c1, c2 := f.collect(t), f.collect(t)
	rel, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{
		Type: "partner_of", SrcEntityID: a, DstEntityID: b, Kind: "fact",
		Prov: claims.Provenance{CollectionID: c1, RecordRef: "r"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := f.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{SourceID: 1, ContextKey: "00000001", Name: "Maria", Prov: claims.Provenance{CollectionID: c1, RecordRef: "p"}})
	if err != nil {
		t.Fatal(err)
	}

	allowed := []struct {
		query string
		args  []any
	}{
		{"UPDATE relationship SET src_entity_id=$2, dst_entity_id=$2 WHERE id=$1", []any{rel, b}},
		{"UPDATE relationship SET last_seen_collection_id=$2, closed_collection_id=$2 WHERE id=$1", []any{rel, c2}},
		{"UPDATE relationship SET valid_to_after='2020-01-01', valid_to_before='2021-01-01' WHERE id=$1", []any{rel}},
	}
	for _, tc := range allowed {
		if _, err := f.pool.Exec(ctx, tc.query, tc.args...); err != nil {
			t.Errorf("allowed update %q: %v", tc.query, err)
		}
	}
	forbidden := []string{
		"UPDATE relationship SET rel_type='controls' WHERE id=$1",
		"UPDATE relationship SET attrs='{\"role\":\"x\"}' WHERE id=$1",
		"UPDATE relationship SET kind='inference' WHERE id=$1",
		"UPDATE relationship SET confidence=0.5 WHERE id=$1",
		"UPDATE relationship SET valid_from='2000-01-01' WHERE id=$1",
		"UPDATE relationship SET record_ref='other' WHERE id=$1",
		"UPDATE relationship SET first_collection_id=last_seen_collection_id WHERE id=$1",
	}
	for _, q := range forbidden {
		_, err := f.pool.Exec(ctx, q, rel)
		if err == nil || !strings.Contains(err.Error(), "claim columns are immutable") {
			t.Errorf("forbidden update %q: got %v", q, err)
		}
	}

	// person_record: pointers are allowed, claim columns are not.
	// locked_by_decision_id references identity_decision (migration 0002).
	var decision int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity_decision (kind, action, record_ids, entity_id, confidence, decided_by)
		VALUES ('analyst', 'merge', ARRAY[$1::bigint], $2, 1, 'test') RETURNING id`, rec, a).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE person_record SET entity_id=$2, identity_confidence=0.7, locked_by_decision_id=$3 WHERE id=$1", rec, a, decision); err != nil {
		t.Errorf("allowed person_record update: %v", err)
	}
	for _, q := range []string{
		"UPDATE person_record SET name='Other' WHERE id=$1",
		"UPDATE person_record SET name_norm='OTHER' WHERE id=$1",
		"UPDATE person_record SET cpf_fragment='x' WHERE id=$1",
		"UPDATE person_record SET context_key='x' WHERE id=$1",
		"UPDATE person_record SET record_ref='x' WHERE id=$1",
	} {
		_, err := f.pool.Exec(ctx, q, rec)
		if err == nil || !strings.Contains(err.Error(), "claim columns are immutable") {
			t.Errorf("forbidden update %q: got %v", q, err)
		}
	}
}

func TestUpdateGuardsDenyByDefault(t *testing.T) {
	f := newFixture(t)
	a, b := f.company(t, "00000001"), f.company(t, "00000002")
	c := f.collect(t)
	rel, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: "controls", SrcEntityID: a, DstEntityID: b, Kind: "fact", Prov: claims.Provenance{CollectionID: c, RecordRef: "r"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"relationship", "person_record", "company_version"} {
		if _, err := f.pool.Exec(ctx, "ALTER TABLE "+table+" ADD COLUMN extra text"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = f.pool.Exec(ctx, "UPDATE relationship SET extra='x' WHERE id=$1", rel)
	if err == nil || !strings.Contains(err.Error(), "claim columns are immutable") {
		t.Fatalf("relationship.extra: got %v", err)
	}
	if _, _, err := f.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{SourceID: 1, ContextKey: "k", Name: "M", Prov: claims.Provenance{CollectionID: c, RecordRef: "p"}}); err != nil {
		t.Fatal(err)
	}
	_, err = f.pool.Exec(ctx, "UPDATE person_record SET extra='x'")
	if err == nil || !strings.Contains(err.Error(), "claim columns are immutable") {
		t.Fatalf("person_record.extra: got %v", err)
	}
	if err := f.s.PutCompanyVersion(ctx, claims.CompanyVersionInput{EntityID: a, Name: "ACME", Prov: claims.Provenance{CollectionID: c, RecordRef: "v"}}); err != nil {
		t.Fatal(err)
	}
	_, err = f.pool.Exec(ctx, "UPDATE company_version SET extra='x'")
	if err == nil || !strings.Contains(err.Error(), "claim columns are immutable") {
		t.Fatalf("company_version.extra: got %v", err)
	}
}

func TestCompanyVersionClaimColumnsAreImmutable(t *testing.T) {
	f := newFixture(t)
	e := f.company(t, "00000001")
	c := f.collect(t)
	if err := f.s.PutCompanyVersion(ctx, claims.CompanyVersionInput{EntityID: e, Name: "ACME", Prov: claims.Provenance{CollectionID: c, RecordRef: "v"}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE company_version SET name='X'",
		"UPDATE company_version SET capital=1",
		"UPDATE company_version SET record_ref='x'",
		"UPDATE company_version SET first_collection_id=first_collection_id + 1",
	} {
		_, err := f.pool.Exec(ctx, q)
		if err == nil || !strings.Contains(err.Error(), "claim columns are immutable") {
			t.Errorf("%q: got %v", q, err)
		}
	}
	if _, err := f.pool.Exec(ctx, "UPDATE company_version SET last_seen_collection_id=$1, closed_collection_id=$1", c); err != nil {
		t.Errorf("bookkeeping update: %v", err)
	}
}

func TestClosedRowsStayClosedAndLastSeenNeverDecreases(t *testing.T) {
	f := newFixture(t)
	a, b := f.company(t, "00000001"), f.company(t, "00000002")
	c1, c2 := f.collect(t), f.collect(t)
	_, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: "controls", SrcEntityID: a, DstEntityID: b, Kind: "fact", Prov: claims.Provenance{CollectionID: c2, RecordRef: "r"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutCompanyVersion(ctx, claims.CompanyVersionInput{EntityID: a, Name: "ACME", Prov: claims.Provenance{CollectionID: c2, RecordRef: "v"}}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"relationship", "company_version"} {
		_, err := f.pool.Exec(ctx, "UPDATE "+table+" SET last_seen_collection_id=$1", c1)
		if err == nil || !strings.Contains(err.Error(), "cannot decrease") {
			t.Errorf("%s decrease: got %v", table, err)
		}
		if _, err := f.pool.Exec(ctx, "UPDATE "+table+" SET closed_collection_id=$1", c2); err != nil {
			t.Fatalf("%s close: %v", table, err)
		}
		for _, q := range []string{"closed_collection_id=NULL", "closed_collection_id=" + fmt.Sprint(c1)} {
			_, err := f.pool.Exec(ctx, "UPDATE "+table+" SET "+q)
			if err == nil || !strings.Contains(err.Error(), "cannot be reopened") {
				t.Errorf("%s %s: got %v", table, q, err)
			}
		}
	}
}

func TestEmptyRecordRefAndConfidenceRangeRejected(t *testing.T) {
	f := newFixture(t)
	a, b := f.company(t, "00000001"), f.company(t, "00000002")
	c := f.collect(t)
	for _, q := range []string{
		`INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, kind, first_collection_id, record_ref) VALUES ('controls', %[1]d, %[2]d, 'fact', %[3]d, '')`,
		`INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, kind, first_collection_id, record_ref, confidence) VALUES ('controls', %[1]d, %[2]d, 'fact', %[3]d, 'x', 1.5)`,
		`INSERT INTO person_record (source_id, context_key, first_collection_id, record_ref) VALUES (1, 'k', %[3]d, '')`,
		`INSERT INTO person_record (source_id, context_key, first_collection_id, record_ref, identity_confidence) VALUES (1, 'k', %[3]d, 'x', -0.1)`,
		`INSERT INTO company_version (entity_id, first_collection_id, record_ref) VALUES (%[1]d, %[3]d, '')`,
	} {
		_, err := f.pool.Exec(ctx, fmt.Sprintf(q, a, b, c))
		if err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Errorf("%q: got %v", q, err)
		}
	}
	bad := 1.5
	_, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: "controls", SrcEntityID: a, DstEntityID: b, Kind: "inference", Confidence: &bad, Prov: claims.Provenance{CollectionID: c, RecordRef: "r"}})
	if !errors.Is(err, claims.ErrConfidenceRange) {
		t.Fatalf("got %v", err)
	}
	// NaN and infinities compare false with everything: still out of range.
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: "controls", SrcEntityID: a, DstEntityID: b, Kind: "inference", Confidence: &v, Prov: claims.Provenance{CollectionID: c, RecordRef: "r"}})
		if !errors.Is(err, claims.ErrConfidenceRange) {
			t.Errorf("confidence %v: got %v, want ErrConfidenceRange", v, err)
		}
	}
}

func TestUpsertPersonRecordDoesNotRewriteExistingRow(t *testing.T) {
	f := newFixture(t)
	c := f.collect(t)
	in := claims.PersonRecordInput{SourceID: 1, ContextKey: "00000001", Name: "Maria", Prov: claims.Provenance{CollectionID: c, RecordRef: "r"}}
	rec, ent, err := f.s.UpsertPersonRecord(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	xmin := func() string {
		var x string
		if err := f.pool.QueryRow(ctx, "SELECT xmin::text FROM person_record WHERE id=$1", rec).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	before := xmin()
	in.Prov.RecordRef = "other"
	rec2, ent2, err := f.s.UpsertPersonRecord(ctx, in)
	if err != nil || rec2 != rec || ent2 != ent {
		t.Fatalf("got %d/%d err=%v", rec2, ent2, err)
	}
	if after := xmin(); after != before {
		t.Fatalf("row rewritten: xmin %s -> %s", before, after)
	}
}

func TestRelationshipTakesEntityFromRecord(t *testing.T) {
	f := newFixture(t)
	c := f.collect(t)
	prov := claims.Provenance{CollectionID: c, RecordRef: "r"}
	rec, ent, err := f.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{
		SourceID: 1, ContextKey: "11111111", Name: "ANA", CPFFragment: "123456", Prov: prov})
	if err != nil {
		t.Fatal(err)
	}
	company := f.company(t, "11111111")
	stale := f.company(t, "22222222")
	for _, given := range []int64{stale, 0} {
		id, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: claims.PartnerOf,
			SrcEntityID: given, DstEntityID: company, SrcRecordID: &rec, Kind: claims.Fact, Prov: prov})
		if err != nil {
			t.Fatalf("given entity %d: %v", given, err)
		}
		if n := f.count(t, `SELECT count(*) FROM relationship WHERE id=$1 AND src_entity_id=$2`, id, ent); n != 1 {
			t.Errorf("given entity %d: the record's entity was not used", given)
		}
	}
	// Destination side, and a missing record.
	if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: claims.LegalRepOf,
		SrcEntityID: company, DstEntityID: stale, DstRecordID: &rec, Kind: claims.Fact, Prov: prov}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE dst_record_id=$1 AND dst_entity_id=$2`, rec, ent); n != 1 {
		t.Error("destination entity not taken from the record")
	}
	missing := int64(999999)
	var unlinked int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO person_record (source_id, context_key, name, name_norm, cpf_fragment,
		first_collection_id, record_ref) VALUES (1, '33333333', 'BIA', 'BIA', '654321', $1, 'raw') RETURNING id`, c).Scan(&unlinked); err != nil {
		t.Fatal(err)
	}
	for _, r := range []int64{missing, unlinked} {
		if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: claims.PartnerOf,
			SrcEntityID: ent, DstEntityID: company, SrcRecordID: &r, Kind: claims.Fact, Prov: prov}); !errors.Is(err, claims.ErrUnknownRecord) {
			t.Errorf("source record %d: err = %v, want ErrUnknownRecord", r, err)
		}
		if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: claims.LegalRepOf,
			SrcEntityID: company, DstEntityID: ent, DstRecordID: &r, Kind: claims.Fact, Prov: prov}); !errors.Is(err, claims.ErrUnknownRecord) {
			t.Errorf("destination record %d: err = %v, want ErrUnknownRecord", r, err)
		}
	}
}
