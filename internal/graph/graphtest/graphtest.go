// Package graphtest builds the test graph of the graph package (and of the
// packages that serve it, like the API). It is test support only: every name,
// CPF fragment and CNPJ in it is invented.
package graphtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/testutil"
)

var ctx = context.Background()

// IDs are the entities (and some relationship ids) of the test graph.
type IDs struct {
	P1, P2, P3, P4, ADM int64 // persons (Receita partners)
	H, A, B, C, F, W, Z int64 // companies; F is an investment fund, W has 60 partners
	X                   int64 // foreign partner of A
	POL, PT1            int64 // politician and party
	Candidates          []int64
	RelP2C, RelP2POL    int64
	RelP3A, RelHA       int64
	RelADMC             int64
	ReceitaCol, TSECol  int64
}

// Builder writes claims for test graphs.
type Builder struct {
	t    *testing.T
	pool *pgxpool.Pool
	s    *claims.Store
}

// NewBuilder seeds the sources and returns a Builder on p.
func NewBuilder(t *testing.T, p *pgxpool.Pool) Builder {
	t.Helper()
	testutil.SeedSources(t, p)
	return Builder{t: t, pool: p, s: claims.NewStore(p)}
}

// Done begins and finishes a full-scope collection of the source at ref.
func (b Builder) Done(sourceID int16, ref time.Time) int64 {
	b.t.Helper()
	col, err := collection.Begin(ctx, b.pool, sourceID, ref, collection.Scope{Full: true})
	if err != nil {
		b.t.Fatal(err)
	}
	if err := collection.Finish(ctx, b.pool, col.ID, nil); err != nil {
		b.t.Fatal(err)
	}
	return col.ID
}

// Entity upserts an entity.
func (b Builder) Entity(kind claims.EntityKind, key, name string) int64 {
	b.t.Helper()
	id, err := b.s.UpsertEntity(ctx, kind, key, name)
	if err != nil {
		b.t.Fatal(err)
	}
	return id
}

// Company creates a company with an open registry version of that legal nature.
func (b Builder) Company(col int64, cnpj, name, legalNature string) int64 {
	b.t.Helper()
	id := b.Entity(claims.KindCompany, cnpj, name)
	if err := b.s.PutCompanyVersion(ctx, claims.CompanyVersionInput{EntityID: id, Name: name, LegalNature: legalNature,
		Prov: claims.Provenance{CollectionID: col, RecordRef: "test:" + cnpj}}); err != nil {
		b.t.Fatal(err)
	}
	return id
}

// Person creates a Receita person record (and its entity) listed in a company context.
func (b Builder) Person(col int64, contextKey, name, cpf string) (record, entity int64) {
	b.t.Helper()
	r, e, err := b.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{SourceID: 1, ContextKey: contextKey, Name: name,
		CPFFragment: cpf, Prov: claims.Provenance{CollectionID: col, RecordRef: "test:" + contextKey + ":" + name}})
	if err != nil {
		b.t.Fatal(err)
	}
	return r, e
}

// End is one end of a relationship: an entity, and the person record cited for it.
type End struct {
	entity int64
	record *int64
}

// Ent is an end without a person record.
func Ent(id int64) End { return End{entity: id} }

// Rec is an end through a person record.
func Rec(record, entity int64) End { return End{entity: entity, record: &record} }

// Rel asserts a fact relationship.
func (b Builder) Rel(col int64, typ claims.RelType, src, dst End, attrs map[string]any, validFrom *time.Time) int64 {
	b.t.Helper()
	return b.assert(col, typ, src, dst, attrs, validFrom, claims.Fact, nil)
}

// Inference asserts an inference relationship with a confidence.
func (b Builder) Inference(col int64, typ claims.RelType, src, dst End, confidence float64) int64 {
	b.t.Helper()
	return b.assert(col, typ, src, dst, nil, nil, claims.Inference, &confidence)
}

func (b Builder) assert(col int64, typ claims.RelType, src, dst End, attrs map[string]any, validFrom *time.Time,
	kind claims.ClaimKind, confidence *float64) int64 {
	b.t.Helper()
	id, err := b.s.AssertRelationship(ctx, claims.RelationshipInput{Type: typ, SrcEntityID: src.entity, DstEntityID: dst.entity,
		SrcRecordID: src.record, DstRecordID: dst.record, Attrs: attrs, Kind: kind, Confidence: confidence, ValidFrom: validFrom,
		Prov: claims.Provenance{CollectionID: col, RecordRef: fmt.Sprintf("test:%s:%d:%d", typ, src.entity, dst.entity)}})
	if err != nil {
		b.t.Fatal(err)
	}
	return id
}

// Date returns a pointer to that UTC date.
func Date(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &t
}

// Seed builds the test graph:
//
//	P1 is the only partner of the holding H; H is a partner of A and B (B
//	also has P4); A is a partner of C; P2 is a partner of C and donated to
//	the politician POL; POL is candidate_for the party PT1, which has 600
//	other candidates (a hub); the fund F, the foreign X and W (60 person
//	partners) are partners of A; P3 → A is closed; ADM is administrator of C
//	and partner of Z.
//
// Receita claims come from a fresh collection; TSE claims from one of 2020,
// so the TSE source is stale.
func Seed(t *testing.T, p *pgxpool.Pool) IDs {
	t.Helper()
	b := NewBuilder(t, p)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var g IDs
	g.ReceitaCol = b.Done(1, today)
	g.TSECol = b.Done(2, *Date(2020, 1, 1))
	rc, tc := g.ReceitaCol, g.TSECol

	g.H = b.Company(rc, "90000001", "HOLDING FICTICIA ALFA SA", "2054")
	g.A = b.Company(rc, "90000002", "EMPRESA FICTICIA A LTDA", "2062")
	g.B = b.Company(rc, "90000003", "EMPRESA FICTICIA B LTDA", "2062")
	g.C = b.Company(rc, "90000004", "EMPRESA FICTICIA C LTDA", "2062")
	g.F = b.Company(rc, "90000005", "FUNDO FICTICIO DE INVESTIMENTO", "2224")
	g.W = b.Company(rc, "90000006", "SOCIEDADE FICTICIA DE MEDICOS LTDA", "2240")
	g.Z = b.Company(rc, "90000007", "EMPRESA FICTICIA Z LTDA", "2062")
	g.X = b.Entity(claims.KindForeign, "foreign:ALPHA OVERSEAS FICTICIA", "ALPHA OVERSEAS FICTICIA")
	g.PT1 = b.Entity(claims.KindParty, "party:99", "PARTIDO FICTICIO")
	g.POL = b.Entity(claims.KindPerson, "candidate:1", "POLITICO FICTICIO")

	p1r, p1 := b.Person(rc, "90000001", "PESSOA FICTICIA UM", "***111111**")
	p2r, p2 := b.Person(rc, "90000004", "PESSOA FICTICIA DOIS", "***222222**")
	p3r, p3 := b.Person(rc, "90000002", "PESSOA FICTICIA TRES", "***333333**")
	p4r, p4 := b.Person(rc, "90000003", "PESSOA FICTICIA QUATRO", "***444444**")
	admr, adm := b.Person(rc, "90000004", "ADMINISTRADOR FICTICIO", "***555555**")
	g.P1, g.P2, g.P3, g.P4, g.ADM = p1, p2, p3, p4, adm
	if _, err := p.Exec(ctx, `UPDATE person_record SET identity_confidence = 0.99 WHERE id = $1`, p2r); err != nil {
		t.Fatal(err)
	}

	b.Rel(rc, claims.PartnerOf, Rec(p1r, p1), Ent(g.H), nil, nil)
	g.RelHA = b.Rel(rc, claims.PartnerOf, Ent(g.H), Ent(g.A), map[string]any{"role": "22"}, Date(2020, 1, 1))
	b.Rel(rc, claims.PartnerOf, Ent(g.H), Ent(g.B), nil, nil)
	b.Rel(rc, claims.PartnerOf, Rec(p4r, p4), Ent(g.B), nil, nil)
	b.Rel(rc, claims.PartnerOf, Ent(g.A), Ent(g.C), nil, nil)
	g.RelP2C = b.Rel(rc, claims.PartnerOf, Rec(p2r, p2), Ent(g.C), nil, nil)
	g.RelP2POL = b.Rel(tc, claims.DonatedTo, Rec(p2r, p2), Ent(g.POL), map[string]any{"amount": 1000}, nil)
	b.Rel(tc, claims.CandidateFor, Ent(g.POL), Ent(g.PT1), nil, nil)
	for i := range 600 {
		c := b.Entity(claims.KindPerson, fmt.Sprintf("candidate:%d", i+2), fmt.Sprintf("CANDIDATO FICTICIO %03d", i+2))
		g.Candidates = append(g.Candidates, c)
		b.Rel(tc, claims.CandidateFor, Ent(c), Ent(g.PT1), nil, nil)
	}
	b.Rel(rc, claims.PartnerOf, Ent(g.F), Ent(g.A), nil, nil)
	b.Rel(rc, claims.PartnerOf, Ent(g.X), Ent(g.A), nil, nil)
	b.Rel(rc, claims.PartnerOf, Ent(g.W), Ent(g.A), nil, nil)
	for i := range 60 {
		r, e := b.Person(rc, "90000006", fmt.Sprintf("MEDICO FICTICIO %02d", i), fmt.Sprintf("***9%05d**", i))
		b.Rel(rc, claims.PartnerOf, Rec(r, e), Ent(g.W), nil, nil)
	}
	g.RelP3A = b.Rel(rc, claims.PartnerOf, Rec(p3r, p3), Ent(g.A), nil, Date(2010, 1, 1))
	if _, err := p.Exec(ctx, `UPDATE relationship SET closed_collection_id = $2,
		valid_to_after = '2025-06-01', valid_to_before = '2025-07-01' WHERE id = $1`, g.RelP3A, rc); err != nil {
		t.Fatal(err)
	}
	g.RelADMC = b.Rel(rc, claims.AdministratorOf, Rec(admr, adm), Ent(g.C), map[string]any{"role": "05"}, nil)
	b.Rel(rc, claims.PartnerOf, Rec(admr, adm), Ent(g.Z), nil, nil)
	return g
}

// FreshReceita returns a Builder and a done Receita collection, for tests
// that build their own small graph.
func FreshReceita(t *testing.T, p *pgxpool.Pool) (Builder, int64) {
	t.Helper()
	b := NewBuilder(t, p)
	return b, b.Done(1, time.Now().UTC().Truncate(24*time.Hour))
}
