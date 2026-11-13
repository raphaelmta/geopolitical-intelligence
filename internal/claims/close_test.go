package claims_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/identity"
	"geoint/internal/rawstore"
	"geoint/internal/sources"
)

var (
	refOct = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	refNov = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	refDec = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	full   = collection.Scope{Full: true}
)

func source(t *testing.T, key string) sources.Source {
	t.Helper()
	s, ok := sources.ByKey(key)
	if !ok {
		t.Fatalf("no source %q", key)
	}
	return s
}

// co is one company as a collection lists it. All names and numbers are invented.
type co struct {
	cnpj     string
	name     string   // registry name; empty: no company version in this collection
	partners []string // person partners (role 49)
	// owners are company partners (cnpj bases, role 22); rep, when set,
	// represents each of them (legal_rep_of, rep record in this company).
	owners []string
	rep    string
}

// baseline is ten companies with one person partner each.
func baseline() []co { return baselineN(10) }

// baselineN is n companies with one person partner each.
func baselineN(n int) []co {
	out := make([]co, n)
	for i := range out {
		out[i] = co{
			cnpj:     fmt.Sprintf("110000%02d", i+1),
			name:     fmt.Sprintf("EMPRESA FICTICIA %02d LTDA", i+1),
			partners: []string{fmt.Sprintf("SOCIO FICTICIO %02d", i+1)},
		}
	}
	return out
}

// without returns cos minus the partner named p.
func without(cos []co, p string) []co {
	out := make([]co, len(cos))
	for i, c := range cos {
		c.partners = nil
		for _, q := range cos[i].partners {
			if q != p {
				c.partners = append(c.partners, q)
			}
		}
		out[i] = c
	}
	return out
}

// begin opens a collection observed on its reference date (as a collection
// of a non-edition source started that day), so the closing bound of these
// tests is the reference date + 1 day (see CloseUnseen).
func (f fixture) begin(t *testing.T, src sources.Source, ref time.Time, scope collection.Scope) collection.Collection {
	t.Helper()
	return f.beginObserved(t, src, ref, ref, scope)
}

func (f fixture) beginObserved(t *testing.T, src sources.Source, ref, observed time.Time, scope collection.Scope) collection.Collection {
	t.Helper()
	col, err := collection.BeginObserved(ctx, f.pool, src.ID, ref, observed, scope)
	if err != nil {
		t.Fatal(err)
	}
	return col
}

// write records the claims of cos in col, as a loader would.
func (f fixture) write(t *testing.T, col collection.Collection, cos []co) {
	t.Helper()
	prov := func(ref string) claims.Provenance {
		return claims.Provenance{CollectionID: col.ID, RecordRef: "test:" + ref}
	}
	for _, c := range cos {
		company := f.company(t, c.cnpj)
		if c.name != "" {
			if err := f.s.PutCompanyVersion(ctx, claims.CompanyVersionInput{EntityID: company, Name: c.name, Prov: prov(c.cnpj)}); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range c.partners {
			rec, _, err := f.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{
				SourceID: col.SourceID, ContextKey: c.cnpj, Name: p, CPFFragment: "***123456**", Prov: prov(c.cnpj + ":" + p)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{
				Type: claims.PartnerOf, SrcRecordID: &rec, DstEntityID: company, Kind: claims.Fact,
				Attrs: map[string]any{"role": "49"}, Prov: prov(c.cnpj + ":" + p)}); err != nil {
				t.Fatal(err)
			}
		}
		for _, o := range c.owners {
			owner := f.company(t, o)
			if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{
				Type: claims.PartnerOf, SrcEntityID: owner, DstEntityID: company, Kind: claims.Fact,
				Attrs: map[string]any{"role": "22"}, Prov: prov(c.cnpj + ":" + o)}); err != nil {
				t.Fatal(err)
			}
			if c.rep == "" {
				continue
			}
			rec, _, err := f.s.UpsertPersonRecord(ctx, claims.PersonRecordInput{
				SourceID: col.SourceID, ContextKey: c.cnpj, Name: c.rep, CPFFragment: "***654321**", Prov: prov(c.cnpj + ":rep")})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{
				Type: claims.LegalRepOf, SrcRecordID: &rec, DstEntityID: owner, Kind: claims.Fact,
				Attrs: map[string]any{"role": "17"}, Prov: prov(c.cnpj + ":rep")}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// collect runs one collection like connector.Run: write the claims, close
// the unseen ones, then finish (or fail) the collection.
func (f fixture) collectWith(t *testing.T, src sources.Source, ref time.Time, scope collection.Scope, cos []co) (collection.Collection, int64, error) {
	t.Helper()
	col := f.begin(t, src, ref, scope)
	f.write(t, col, cos)
	closed, err := f.s.CloseUnseen(ctx, src, col, 0.9)
	if err != nil {
		if ferr := collection.Fail(ctx, f.pool, col.ID, err); ferr != nil {
			t.Fatal(ferr)
		}
		return col, closed, err
	}
	if err := collection.Finish(ctx, f.pool, col.ID, nil); err != nil {
		t.Fatal(err)
	}
	return col, closed, nil
}

func (f fixture) mustCollect(t *testing.T, ref time.Time, scope collection.Scope, cos []co) (collection.Collection, int64) {
	t.Helper()
	col, closed, err := f.collectWith(t, source(t, "receita"), ref, scope, cos)
	if err != nil {
		t.Fatalf("CloseUnseen: %v", err)
	}
	return col, closed
}

// relState is the closing state of one relationship row.
type relState struct {
	id                  int64
	closed              *int64
	validAfter, validBy *time.Time
}

// partnerRows returns the relationship rows whose source end is the person
// record named name, oldest first.
func (f fixture) partnerRows(t *testing.T, name string) []relState {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT r.id, r.closed_collection_id, r.valid_to_after, r.valid_to_before
		FROM relationship r JOIN person_record p ON p.id = r.src_record_id WHERE p.name = $1 ORDER BY r.id`, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []relState
	for rows.Next() {
		var s relState
		if err := rows.Scan(&s.id, &s.closed, &s.validAfter, &s.validBy); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f fixture) closedRows(t *testing.T) int {
	t.Helper()
	return f.count(t, `SELECT (SELECT count(*) FROM relationship WHERE closed_collection_id IS NOT NULL)
		+ (SELECT count(*) FROM company_version WHERE closed_collection_id IS NOT NULL)`)
}

func sameDay(p *time.Time, want time.Time) bool { return p != nil && p.Equal(want) }

// dayAfter is the exclusive closing bound of a collection observed on d.
func dayAfter(d time.Time) time.Time { return d.AddDate(0, 0, 1) }

func TestPartnerMissingInNextCollectionIsClosedWithInterval(t *testing.T) {
	f := newFixture(t)
	first := baseline()
	first[0].partners = append(first[0].partners, "XAVIER FICTICIO")
	f.mustCollect(t, refOct, full, first)
	c2, closed := f.mustCollect(t, refNov, full, baseline())

	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
	rows := f.partnerRows(t, "XAVIER FICTICIO")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (the row is kept)", len(rows))
	}
	r := rows[0]
	// Observed on refNov (begin): absent in the state of that day, so the
	// first day it certainly no longer holds is the day after.
	if r.closed == nil || *r.closed != c2.ID || !sameDay(r.validAfter, refOct) || !sameDay(r.validBy, dayAfter(refNov)) {
		t.Fatalf("closed=%v after=%v before=%v, want %d/%s/%s", r.closed, r.validAfter, r.validBy, c2.ID, refOct, dayAfter(refNov))
	}
	if n := f.closedRows(t); n != 1 {
		t.Fatalf("closed rows = %d, want only the missing partner", n)
	}
}

func TestPartnerStillPresentStaysOpen(t *testing.T) {
	f := newFixture(t)
	f.mustCollect(t, refOct, full, baseline())
	c2, closed := f.mustCollect(t, refNov, full, baseline())
	if closed != 0 || f.closedRows(t) != 0 {
		t.Fatalf("closed = %d, closed rows = %d, want none", closed, f.closedRows(t))
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE last_seen_collection_id = $1`, c2.ID); n != 10 {
		t.Fatalf("relationships seen in c2 = %d, want 10", n)
	}
	r := f.partnerRows(t, "SOCIO FICTICIO 01")[0]
	if r.validAfter != nil || r.validBy != nil {
		t.Fatalf("open row has an end interval: %v %v", r.validAfter, r.validBy)
	}
}

func TestShrunkCollectionClosesNothing(t *testing.T) {
	f := newFixture(t)
	f.mustCollect(t, refOct, full, baseline())
	_, closed, err := f.collectWith(t, source(t, "receita"), refNov, full, baseline()[:5])
	if !errors.Is(err, claims.ErrShrunkCollection) {
		t.Fatalf("err = %v, want ErrShrunkCollection", err)
	}
	if closed != 0 || f.closedRows(t) != 0 {
		t.Fatalf("closed = %d, closed rows = %d, want none", closed, f.closedRows(t))
	}
}

func TestShrunkCompanyVersionsAloneClosesNothing(t *testing.T) {
	f := newFixture(t)
	first := baseline()
	first[0].partners = append(first[0].partners, "XAVIER FICTICIO")
	f.mustCollect(t, refOct, full, first)
	// Every partner but one is seen again, but half the companies lost their registry row.
	second := baseline()
	for i := range second[:5] {
		second[i].name = ""
	}
	_, _, err := f.collectWith(t, source(t, "receita"), refNov, full, second)
	if !errors.Is(err, claims.ErrShrunkCollection) {
		t.Fatalf("err = %v, want ErrShrunkCollection", err)
	}
	if n := f.closedRows(t); n != 0 {
		t.Fatalf("closed rows = %d, want none (the missing partner stays open too)", n)
	}
}

// sliceWorld is October's full collection for the slice tests: twenty
// companies; 11000001 has the partners XAVIER and YARA too and is owned by
// 11000021, a company outside any slice of 11000001, whose legal
// representative is listed in 11000001.
func sliceWorld() []co {
	cos := baselineN(20)
	cos[0].partners = append(cos[0].partners, "XAVIER FICTICIO", "YARA FICTICIA")
	cos[0].owners, cos[0].rep = []string{"11000021"}, "REPRESENTANTE FICTICIO"
	return cos
}

// sliceRun is the slice of 11000001 at depth 0, as the Receita loader writes
// it: the corporate owner is outside the slice, so its partner row (and the
// representative of that row) is dropped; XAVIER left the company.
func sliceRun() []co {
	a := sliceWorld()[0]
	a.partners = []string{"SOCIO FICTICIO 01", "YARA FICTICIA"}
	a.owners, a.rep = nil, ""
	return []co{a}
}

func TestSliceCollectionDoesNotCloseOutsideSlice(t *testing.T) {
	f := newFixture(t)
	f.mustCollect(t, refOct, full, sliceWorld())
	_, closed := f.mustCollect(t, refNov, collection.Scope{CNPJs: []string{"11.000.001/0001-91"}}, sliceRun())
	if closed != 0 {
		t.Fatalf("closed = %d, want 0: a slice never closes", closed)
	}
	if n := f.closedRows(t); n != 0 {
		t.Fatalf("closed rows = %d, want none", n)
	}
}

func TestFullCollectionAfterSliceClosesWithLastSeenDate(t *testing.T) {
	f := newFixture(t)
	f.mustCollect(t, refOct, full, sliceWorld())
	f.mustCollect(t, refNov, collection.Scope{CNPJs: []string{"11000001"}}, sliceRun())
	// December: everything again except XAVIER (last seen in October) and
	// YARA (last seen by the November slice).
	c3, closed := f.mustCollect(t, refDec, full, without(without(sliceWorld(), "XAVIER FICTICIO"), "YARA FICTICIA"))
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}
	for name, after := range map[string]time.Time{"XAVIER FICTICIO": refOct, "YARA FICTICIA": refNov} {
		r := f.partnerRows(t, name)[0]
		if r.closed == nil || *r.closed != c3.ID || !sameDay(r.validAfter, after) || !sameDay(r.validBy, dayAfter(refDec)) {
			t.Errorf("%s: closed=%v after=%v before=%v, want %d/%s/%s", name, r.closed, r.validAfter, r.validBy, c3.ID, after, dayAfter(refDec))
		}
	}
	if r := f.partnerRows(t, "REPRESENTANTE FICTICIO"); len(r) != 1 || r[0].closed != nil {
		t.Fatalf("representative re-seen in December was closed: %+v", r)
	}
}

func TestInferencesAreNeverClosed(t *testing.T) {
	f := newFixture(t)
	src := source(t, "receita")
	c1 := f.begin(t, src, refOct, full)
	f.write(t, c1, baseline())
	conf := 0.7
	if _, err := f.s.AssertRelationship(ctx, claims.RelationshipInput{
		Type: claims.Controls, SrcEntityID: f.company(t, "11000001"), DstEntityID: f.company(t, "11000002"),
		Kind: claims.Inference, Confidence: &conf, Prov: claims.Provenance{CollectionID: c1.ID, RecordRef: "test:inference"}}); err != nil {
		t.Fatal(err)
	}
	if err := collection.Finish(ctx, f.pool, c1.ID, nil); err != nil {
		t.Fatal(err)
	}
	_, closed := f.mustCollect(t, refNov, full, baseline())
	if closed != 0 || f.closedRows(t) != 0 {
		t.Fatalf("closed = %d, closed rows = %d: the inference was closed", closed, f.closedRows(t))
	}
}

func TestShrinkGuardBoundary(t *testing.T) {
	for _, tc := range []struct {
		dropped int
		shrunk  bool
	}{{10, false}, {11, true}} {
		t.Run(fmt.Sprintf("%d of 100 relationships unseen", tc.dropped), func(t *testing.T) {
			f := newFixture(t)
			f.mustCollect(t, refOct, full, baselineN(100))
			second := baselineN(100)
			for i := range second[:tc.dropped] {
				second[i].partners = nil
			}
			_, closed, err := f.collectWith(t, source(t, "receita"), refNov, full, second)
			switch {
			case tc.shrunk && !errors.Is(err, claims.ErrShrunkCollection):
				t.Fatalf("err = %v, want ErrShrunkCollection", err)
			case !tc.shrunk && (err != nil || closed != int64(tc.dropped)):
				t.Fatalf("closed = %d, err = %v, want %d, nil", closed, err, tc.dropped)
			}
		})
	}
}

func TestFirstCollectionClosesNothing(t *testing.T) {
	f := newFixture(t)
	src := source(t, "receita")
	// A failed collection is not a previous one, even if it wrote claims.
	failed := f.begin(t, src, refSep(), full)
	f.write(t, failed, baseline())
	if err := collection.Fail(ctx, f.pool, failed.ID, errors.New("broke")); err != nil {
		t.Fatal(err)
	}
	col := f.begin(t, src, refOct, full)
	f.write(t, col, baseline()[:1])
	closed, err := f.s.CloseUnseen(ctx, src, col, 0.9)
	if err != nil || closed != 0 {
		t.Fatalf("closed = %d, err = %v, want 0, nil", closed, err)
	}
	if n := f.closedRows(t); n != 0 {
		t.Fatalf("closed rows = %d, want none", n)
	}
}

func refSep() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) }

func TestReappearingPartnerGetsNewOpenRow(t *testing.T) {
	f := newFixture(t)
	with := baseline()
	with[0].partners = append(with[0].partners, "XAVIER FICTICIO")
	f.mustCollect(t, refOct, full, with)
	c2, _ := f.mustCollect(t, refNov, full, baseline())
	c3, closed := f.mustCollect(t, refDec, full, with)
	if closed != 0 {
		t.Fatalf("closed in c3 = %d, want 0", closed)
	}
	rows := f.partnerRows(t, "XAVIER FICTICIO")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].closed == nil || *rows[0].closed != c2.ID {
		t.Fatalf("old row: closed = %v, want %d", rows[0].closed, c2.ID)
	}
	if rows[1].closed != nil {
		t.Fatalf("new row is closed: %v", *rows[1].closed)
	}
	if n := f.count(t, `SELECT count(*) FROM relationship WHERE id = $1 AND first_collection_id = $2`, rows[1].id, c3.ID); n != 1 {
		t.Fatal("new row was not opened by c3")
	}
}

func TestClosedRowIsNeverTouchedAgain(t *testing.T) {
	f := newFixture(t)
	with := baseline()
	with[0].partners = append(with[0].partners, "XAVIER FICTICIO")
	f.mustCollect(t, refOct, full, with)
	c2, _ := f.mustCollect(t, refNov, full, baseline())
	_, closed := f.mustCollect(t, refDec, full, baseline())
	if closed != 0 {
		t.Fatalf("closed in c3 = %d, want 0", closed)
	}
	r := f.partnerRows(t, "XAVIER FICTICIO")[0]
	if r.closed == nil || *r.closed != c2.ID || !sameDay(r.validAfter, refOct) || !sameDay(r.validBy, dayAfter(refNov)) {
		t.Fatalf("closed=%v after=%v before=%v, want the first closer %d", r.closed, r.validAfter, r.validBy, c2.ID)
	}
}

func TestCloseUnseenIgnoresOtherSources(t *testing.T) {
	f := newFixture(t)
	other := make([]co, 20)
	for i := range other {
		other[i] = co{cnpj: fmt.Sprintf("220000%02d", i+1), name: "OUTRA FONTE LTDA", partners: []string{fmt.Sprintf("SOCIO CVM %02d", i+1)}}
	}
	cvm := source(t, "cvm")
	if _, _, err := f.collectWith(t, cvm, refOct, full, other); err != nil {
		t.Fatal(err)
	}
	f.mustCollect(t, refOct, full, baseline())
	_, closed := f.mustCollect(t, refNov, full, baseline())
	if closed != 0 || f.closedRows(t) != 0 {
		t.Fatalf("closed = %d, closed rows = %d: another source's claims were closed", closed, f.closedRows(t))
	}
}

func TestOlderReferenceDateClosesNothing(t *testing.T) {
	f := newFixture(t)
	with := baseline()
	with[0].partners = append(with[0].partners, "XAVIER FICTICIO")
	f.mustCollect(t, refNov, full, with)
	_, closed, err := f.collectWith(t, source(t, "receita"), refOct, full, baseline())
	if err == nil || errors.Is(err, claims.ErrShrunkCollection) {
		t.Fatalf("err = %v, want a reference date error", err)
	}
	if closed != 0 || f.closedRows(t) != 0 {
		t.Fatalf("closed = %d, closed rows = %d, want none", closed, f.closedRows(t))
	}
}

func TestCloseUnseenRejectsInvalidArguments(t *testing.T) {
	f := newFixture(t)
	src := source(t, "receita")
	f.mustCollect(t, refOct, full, baseline())
	col := f.begin(t, src, refNov, full)
	for _, r := range []float64{0, -0.5, 1.5} {
		if _, err := f.s.CloseUnseen(ctx, src, col, r); err == nil {
			t.Errorf("minRatio %v accepted", r)
		}
	}
	if _, err := f.s.CloseUnseen(ctx, source(t, "cvm"), col, 0.9); err == nil {
		t.Error("collection of another source accepted")
	}
	if n := f.closedRows(t); n != 0 {
		t.Fatalf("closed rows = %d, want none", n)
	}
}

// snapshotConnector writes cos as its claims; the files are not used.
type snapshotConnector struct {
	f   fixture
	t   *testing.T
	cos []co
}

func (c *snapshotConnector) Key() string { return "cvm" }

func (c *snapshotConnector) Fetch(context.Context, time.Time, rawstore.Store) ([]rawstore.File, error) {
	return nil, nil
}

func (c *snapshotConnector) Load(_ context.Context, col collection.Collection, _ []rawstore.File, _ rawstore.Store, _ *pgxpool.Pool) (connector.Stats, error) {
	c.f.write(c.t, col, c.cos)
	return connector.Stats{"rows_read": int64(len(c.cos)), "rows_skipped": 0}, nil
}

func TestShrunkCollectionFailsTheRun(t *testing.T) {
	f := newFixture(t)
	hooks := connector.Hooks{CloseUnseen: func(ctx context.Context, src sources.Source, col collection.Collection) (int64, error) {
		return f.s.CloseUnseen(ctx, src, col, 0.9)
	}}
	run := func(ref time.Time, cos []co) error {
		req := connector.Request{Source: "cvm", ReferenceDate: ref, Scope: full}
		return connector.Run(ctx, &snapshotConnector{f: f, t: t, cos: cos}, req, f.pool, nil, hooks)
	}
	if err := run(refOct, baseline()); err != nil {
		t.Fatal(err)
	}
	err := run(refNov, baseline()[:5])
	var runErr *connector.RunError
	if !errors.As(err, &runErr) || !errors.Is(err, claims.ErrShrunkCollection) {
		t.Fatalf("err = %v, want a RunError wrapping ErrShrunkCollection", err)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM collection WHERE id = $1`, runErr.CollectionID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != collection.StatusFailed {
		t.Fatalf("status = %q, want failed", status)
	}
	if n := f.closedRows(t); n != 0 {
		t.Fatalf("closed rows = %d, want none", n)
	}
}

// The Receita case of the final review (I2): edition 2026-10 (reference date
// 2026-10-01) was published on 2026-10-13 and edition 2026-11 on 2026-11-10.
// A partner listed in 2026-10 and absent from 2026-11 left on some day up to
// 2026-11-10: the row must stay valid on 2026-11-04 (it may have left later
// that week), so the exclusive upper bound is 2026-11-11, never the month
// start 2026-11-01. The lower bound stays the reference date of the last
// edition that listed it.
func TestEditionClosingUsesTheObservedDate(t *testing.T) {
	f := newFixture(t)
	src := source(t, "receita")
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }
	first := baseline()
	first[0].partners = append(first[0].partners, "XAVIER FICTICIO")
	c1 := f.beginObserved(t, src, refOct, day(10, 13), full)
	f.write(t, c1, first)
	if _, err := f.s.CloseUnseen(ctx, src, c1, 0.9); err != nil {
		t.Fatal(err)
	}
	if err := collection.Finish(ctx, f.pool, c1.ID, nil); err != nil {
		t.Fatal(err)
	}
	c2 := f.beginObserved(t, src, refNov, day(11, 10), full)
	f.write(t, c2, baseline())
	if closed, err := f.s.CloseUnseen(ctx, src, c2, 0.9); err != nil || closed != 1 {
		t.Fatalf("closed = %d, err = %v, want 1", closed, err)
	}
	r := f.partnerRows(t, "XAVIER FICTICIO")[0]
	if !sameDay(r.validAfter, refOct) || !sameDay(r.validBy, day(11, 11)) {
		t.Fatalf("after=%v before=%v, want %s/%s", r.validAfter, r.validBy, refOct, day(11, 11))
	}
	// Point in time (claims package doc: valid on D when D < valid_to_before).
	validOn := func(d time.Time) bool {
		return f.count(t, `SELECT count(*) FROM relationship WHERE id = $1
			AND (valid_from IS NULL OR valid_from <= $2::date) AND (valid_to_before IS NULL OR $2::date < valid_to_before)`, r.id, d) == 1
	}
	if !validOn(day(11, 4)) || !validOn(day(11, 10)) {
		t.Fatal("the closed link is not valid on 2026-11-04 / 2026-11-10, days it may still have held")
	}
	if validOn(day(11, 11)) {
		t.Fatal("the closed link is valid on 2026-11-11, after the edition that no longer lists it")
	}
}

// CloseUnseen trusts the stored collection row, not the caller's struct.
func TestCloseUnseenReadsTheStoredScope(t *testing.T) {
	f := newFixture(t)
	src := source(t, "receita")
	with := baseline()
	with[0].partners = append(with[0].partners, "XAVIER FICTICIO")
	f.mustCollect(t, refOct, full, with)

	// A slice whose caller claims it is full closes nothing.
	slice := f.begin(t, src, refNov, collection.Scope{CNPJs: []string{"11000002"}})
	f.write(t, slice, baseline()[1:2])
	lying := slice
	lying.Scope = full
	if closed, err := f.s.CloseUnseen(ctx, src, lying, 0.9); err != nil || closed != 0 {
		t.Fatalf("slice presented as full: closed = %d, err = %v, want 0, nil", closed, err)
	}
	if err := collection.Finish(ctx, f.pool, slice.ID, nil); err != nil {
		t.Fatal(err)
	}
	// A full collection whose caller lost its scope still closes.
	c3 := f.begin(t, src, refDec, full)
	f.write(t, c3, baseline())
	blank := c3
	blank.Scope = collection.Scope{}
	if closed, err := f.s.CloseUnseen(ctx, src, blank, 0.9); err != nil || closed != 1 {
		t.Fatalf("full presented as a slice: closed = %d, err = %v, want 1, nil", closed, err)
	}
}

// CloseUnseen takes the identity lock first: while an analyst decision (or
// a resolver batch) holds it, the close waits instead of racing it.
func TestCloseUnseenWaitsForTheIdentityLock(t *testing.T) {
	f := newFixture(t)
	src := source(t, "receita")
	with := baseline()
	with[0].partners = append(with[0].partners, "XAVIER FICTICIO")
	f.mustCollect(t, refOct, full, with)
	col := f.begin(t, src, refNov, full)
	f.write(t, col, baseline())

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed below
	if err := identity.LockTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.s.CloseUnseen(ctx, src, col, 0.9)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("CloseUnseen finished while the identity lock was held (err %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CloseUnseen did not finish after the identity lock was released")
	}
	if n := f.closedRows(t); n != 1 {
		t.Fatalf("closed rows = %d, want 1", n)
	}
}
