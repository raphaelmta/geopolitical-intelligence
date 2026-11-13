package identity_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/identity"
	"geoint/internal/testutil"
)

var ctx = context.Background()

const (
	receita int16 = 1
	tse     int16 = 2
)

type fixture struct {
	pool *pgxpool.Pool
	s    *claims.Store
	r    *identity.Resolver
}

func newFixture(t *testing.T, threshold int) fixture {
	t.Helper()
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	return fixture{pool: pool, s: claims.NewStore(pool), r: &identity.Resolver{Pool: pool, CommonNameThreshold: threshold}}
}

func (f fixture) collect(t *testing.T, source int16) int64 {
	t.Helper()
	c, err := collection.Begin(ctx, f.pool, source, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// person creates a person record in the context company `ctxKey` and a
// partner_of relationship from it to that company. It returns the record id.
func (f fixture) person(t *testing.T, col int64, source int16, ctxKey, name, frag string) int64 {
	t.Helper()
	prov := claims.Provenance{CollectionID: col, RecordRef: fmt.Sprintf("test:%s:%s:%s", ctxKey, name, frag)}
	rec, ent, err := f.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{
		SourceID: source, ContextKey: ctxKey, Name: name, CPFFragment: frag, Prov: prov})
	if err != nil {
		t.Fatal(err)
	}
	company, err := f.s.UpsertEntity(ctx, claims.KindCompany, "c"+ctxKey, "EMPRESA "+ctxKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{Type: claims.PartnerOf,
		SrcEntityID: ent, DstEntityID: company, SrcRecordID: &rec, Kind: claims.Fact, Prov: prov}); err != nil {
		t.Fatal(err)
	}
	return rec
}

func (f fixture) resolve(t *testing.T, col int64) identity.Result {
	t.Helper()
	res, err := f.r.Resolve(ctx, col)
	if err != nil {
		t.Fatalf("resolve %d: %v", col, err)
	}
	return res
}

func (f fixture) entityOf(t *testing.T, rec int64) int64 {
	t.Helper()
	var e int64
	if err := f.pool.QueryRow(ctx, `SELECT entity_id FROM person_record WHERE id=$1`, rec).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func (f fixture) confidence(t *testing.T, rec int64) float64 {
	t.Helper()
	var c float32
	if err := f.pool.QueryRow(ctx, `SELECT identity_confidence FROM person_record WHERE id=$1`, rec).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return float64(c)
}

func (f fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f fixture) proposalID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(ctx, `SELECT id FROM identity_proposal`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-7 }

// commonName adds records so that name has three distinct fragments in total
// (with "123456"), above a threshold of 2.
func (f fixture) commonName(t *testing.T, col int64, name string) {
	t.Helper()
	f.person(t, col, receita, "90000001", name, "111111")
	f.person(t, col, receita, "90000002", name, "222222")
}

// seedConcurrent builds two collections of different sources whose names overlap.
func seedConcurrent(t *testing.T, f fixture) (int64, int64) {
	t.Helper()
	colR := f.collect(t, receita)
	colT := f.collect(t, tse)
	for i := range 6 {
		name := fmt.Sprintf("PESSOA NUMERO %d", i%3)
		f.person(t, colR, receita, fmt.Sprintf("%08d", i), name, fmt.Sprintf("%06d", 100000+i%2))
		f.person(t, colT, tse, fmt.Sprintf("cand-%d", i), name, fmt.Sprintf("%06d", 100000+i%4))
	}
	f.commonName(t, colR, "JOSE DA SILVA")
	f.person(t, colR, receita, "00000101", "JOSE DA SILVA", "123456")
	f.person(t, colT, tse, "cand-101", "JOSE DA SILVA", "123456")
	return colR, colT
}

// snapshot renders the identity state of a schema in a comparable form.
func snapshot(t *testing.T, f fixture) string {
	t.Helper()
	var s string
	err := f.pool.QueryRow(ctx, `SELECT
		(SELECT string_agg(format('%s:%s:%s', id, entity_id, identity_confidence), ' ' ORDER BY id) FROM person_record) || ' / ' ||
		coalesce((SELECT string_agg(format('%s:%s:%s:%s', group_key, status, record_ids, connections), ' ' ORDER BY group_key)
			FROM identity_proposal), '') || ' / ' ||
		(SELECT string_agg(format('%s:%s:%s', id, src_entity_id, dst_entity_id), ' ' ORDER BY id) FROM relationship)`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// changes is r without its fragment distribution, which a re-run reports
// again (it describes the names touched, not what changed).
func changes(r identity.Result) identity.Result {
	r.NamesByFragments = [5]int64{}
	return r
}
