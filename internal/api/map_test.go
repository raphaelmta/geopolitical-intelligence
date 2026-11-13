package api_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"geoint/internal/api"
	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/graph/graphtest"
)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func TestSearchNormalizesInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	b, rc := graphtest.FreshReceita(t, e.pool)
	id := b.Company(rc, "12345678", "AÇÚCAR MODELO LTDA", "2062")
	b.Company(rc, "87654321", "OUTRA EMPRESA FICTICIA LTDA", "2062")
	e.rebuildSearch()
	for _, q := range []string{"açúcar modelo", "ACUCAR MODELO", "12.345.678/0001-90", "12345678000190", "12345678"} {
		got := want[struct{ Items []entityRef }](t, e.do("GET", "/api/v1/search?q="+url.QueryEscape(q), tokAnalyst, ""), http.StatusOK)
		if len(got.Items) == 0 || got.Items[0].ID != id || got.Items[0].NaturalKey != "12345678" || got.Items[0].Kind != "company" {
			t.Errorf("q=%q: items %+v, want entity %d first", q, got.Items, id)
		}
	}
	rec := e.do("GET", "/api/v1/search?q=zzzzqqq", tokAnalyst, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("no results: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSearchTooShortIs400(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	for _, q := range []string{"", "ab", "de da"} {
		wantError(t, e.do("GET", "/api/v1/search?q="+url.QueryEscape(q), tokAnalyst, ""), http.StatusBadRequest, "query_too_short")
	}
}

func TestSearchAuditHasNoQuery(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	e.rebuildSearch()
	got := want[struct{ Items []entityRef }](t, e.do("GET", "/api/v1/search?q=empresa+ficticia&kind=company", tokAnalyst, ""), http.StatusOK)
	var detail map[string]any
	var user string
	if err := e.pool.QueryRow(bg, `SELECT user_sub, detail FROM audit_log WHERE action = 'search'`).Scan(&user, &detail); err != nil {
		t.Fatal(err)
	}
	if user != "sub-analyst" || len(detail) != 2 || detail["kind"] != "company" || detail["count"] != float64(len(got.Items)) {
		t.Fatalf("audit %s %v, want only kind and count", user, detail)
	}
}

type entityPage struct {
	Entity   entityRef `json:"entity"`
	Versions []struct {
		ID                     int64   `json:"id"`
		Name                   string  `json:"name"`
		LegalNature            string  `json:"legal_nature"`
		Open                   bool    `json:"open"`
		FirstSeenReferenceDate *string `json:"first_seen_reference_date"`
		LastSeenReferenceDate  *string `json:"last_seen_reference_date"`
		ClosedReferenceDate    *string `json:"closed_reference_date"`
	} `json:"versions"`
	Records []struct {
		ID                     int64   `json:"id"`
		SourceKey              string  `json:"source_key"`
		ContextKey             string  `json:"context_key"`
		IdentityConfidence     float64 `json:"identity_confidence"`
		CPFMasked              string  `json:"cpf_masked"`
		RecordRef              string  `json:"record_ref"`
		FirstCollectionID      int64   `json:"first_collection_id"`
		FirstSeenReferenceDate *string `json:"first_seen_reference_date"`
	} `json:"records"`
	PossibleSame []struct {
		EntityID       int64  `json:"entity_id"`
		Name           string `json:"name"`
		SameName       bool   `json:"same_name"`
		Records        int    `json:"records"`
		SharedFragment bool   `json:"shared_fragment"`
	} `json:"possible_same"`
}

func TestEntityPage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	got := want[entityPage](t, e.do("GET", "/api/v1/entities/"+itoa(e.ids.H), tokAnalyst, ""), http.StatusOK)
	if got.Entity.ID != e.ids.H || got.Entity.Name != "HOLDING FICTICIA ALFA SA" || len(got.Versions) != 1 ||
		!got.Versions[0].Open || got.Versions[0].LegalNature != "2054" {
		t.Fatalf("company page %+v", got)
	}
	rec := e.do("GET", "/api/v1/entities/"+itoa(e.ids.P2), tokAnalyst, "")
	got = want[entityPage](t, rec, http.StatusOK)
	if len(got.Records) != 1 || got.Records[0].SourceKey != "receita" || got.Records[0].ContextKey != "90000004" ||
		got.Records[0].IdentityConfidence != 0.99 || got.Records[0].CPFMasked != "***222222**" {
		t.Fatalf("person page %+v", got)
	}
	if r := got.Records[0]; r.RecordRef != "test:90000004:PESSOA FICTICIA DOIS" || r.FirstCollectionID != e.ids.ReceitaCol ||
		r.FirstSeenReferenceDate == nil || *r.FirstSeenReferenceDate != time.Now().UTC().Format(time.DateOnly) {
		t.Fatalf("record provenance %+v", r)
	}
	if strings.Contains(rec.Body.String(), "cpf_fragment") || got.PossibleSame == nil {
		t.Fatalf("person page body %s", rec.Body.String())
	}
}

func TestEntityPageVersionDatesAndPossibleSameBasis(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	b := graphtest.NewBuilder(t, e.pool)
	today := time.Now().UTC().Format(time.DateOnly)
	// A later Receita edition closes H's version.
	later := b.Done(1, *graphtest.Date(2030, 2, 1))
	if _, err := e.pool.Exec(bg, `UPDATE company_version SET last_seen_collection_id = $2 WHERE entity_id = $1`,
		e.ids.H, e.ids.ReceitaCol); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(bg, `UPDATE company_version SET closed_collection_id = $2 WHERE entity_id = $1`,
		e.ids.H, later); err != nil {
		t.Fatal(err)
	}
	got := want[entityPage](t, e.do("GET", "/api/v1/entities/"+itoa(e.ids.H), tokAnalyst, ""), http.StatusOK)
	v := got.Versions[0]
	if v.Open || v.FirstSeenReferenceDate == nil || *v.FirstSeenReferenceDate != today ||
		v.LastSeenReferenceDate == nil || *v.LastSeenReferenceDate != today ||
		v.ClosedReferenceDate == nil || *v.ClosedReferenceDate != "2030-02-01" {
		t.Fatalf("closed version %+v", v)
	}
	// An open version has no closing date.
	if v := want[entityPage](t, e.do("GET", "/api/v1/entities/"+itoa(e.ids.A), tokAnalyst, ""), http.StatusOK).Versions[0]; v.ClosedReferenceDate != nil || !v.Open {
		t.Fatalf("open version %+v", v)
	}

	// A person and two other records with the same name: one with the same
	// CPF fragment (stored as the loaders store it, 6 digits), one with another.
	_, base := b.Person(e.ids.ReceitaCol, "90000076", "PESSOA BASE FICTICIA", "222222")
	_, same := b.Person(e.ids.ReceitaCol, "90000077", "PESSOA BASE FICTICIA", "222222")
	_, other := b.Person(e.ids.ReceitaCol, "90000078", "PESSOA BASE FICTICIA", "999999")
	rec := e.do("GET", "/api/v1/entities/"+itoa(base), tokAnalyst, "")
	page := want[entityPage](t, rec, http.StatusOK)
	basis := map[int64]bool{}
	for _, c := range page.PossibleSame {
		if !c.SameName || c.Records != 1 || c.Name != "PESSOA BASE FICTICIA" {
			t.Errorf("candidate %+v", c)
		}
		basis[c.EntityID] = c.SharedFragment
	}
	if len(basis) != 2 || !basis[same] || basis[other] {
		t.Fatalf("possible_same %+v, want %d sharing the fragment and %d not", page.PossibleSame, same, other)
	}
	if strings.Contains(rec.Body.String(), "999999") || strings.Contains(rec.Body.String(), `"222222"`) {
		t.Fatalf("a fragment leaked: %s", rec.Body.String())
	}
}

func TestEntityReadIsAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	want[entityPage](t, e.do("GET", "/api/v1/entities/"+itoa(e.ids.A), tokAnalyst, ""), http.StatusOK)
	if n := e.auditCount("entity.read", &e.ids.A); n != 1 {
		t.Fatalf("%d entity.read rows, want 1", n)
	}
	want[map[string]any](t, e.do("GET", "/api/v1/entities/"+itoa(e.ids.A)+"/relationships", tokAnalyst, ""), http.StatusOK)
	if n := e.auditCount("entity.relationships.read", &e.ids.A); n != 1 {
		t.Fatalf("%d entity.relationships.read rows, want 1", n)
	}
	want[map[string]any](t, e.do("GET", fmt.Sprintf("/api/v1/relationships/%d/provenance", e.ids.RelHA), tokAnalyst, ""), http.StatusOK)
	if n := e.auditCount("relationship.provenance.read", nil); n != 1 {
		t.Fatalf("%d relationship.provenance.read rows, want 1", n)
	}
}

type relPage struct {
	Items      []edge `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func TestEntityRelationshipsPaginates(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	b, rc := graphtest.FreshReceita(t, e.pool)
	hub := b.Company(rc, "80000000", "CENTRO FICTICIO LTDA", "2062")
	for i := range 450 {
		p := b.Entity(claims.KindCompany, fmt.Sprintf("8%07d", i+1), fmt.Sprintf("SOCIA FICTICIA %03d", i))
		b.Rel(rc, claims.PartnerOf, graphtest.Ent(p), graphtest.Ent(hub), nil, nil)
	}
	seen := map[int64]bool{}
	var sizes []int
	cursor := ""
	for range 5 {
		path := "/api/v1/entities/" + itoa(hub) + "/relationships?limit=1000"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		page := want[relPage](t, e.do("GET", path, tokAnalyst, ""), http.StatusOK)
		sizes = append(sizes, len(page.Items))
		for _, it := range page.Items {
			seen[it.RelationshipID] = true
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if fmt.Sprint(sizes) != "[200 200 50]" || len(seen) != 450 {
		t.Fatalf("page sizes %v, %d distinct edges", sizes, len(seen))
	}
	wantError(t, e.do("GET", "/api/v1/entities/"+itoa(hub)+"/relationships?cursor=bad!", tokAnalyst, ""), http.StatusBadRequest, "bad_request")
}

func TestRelationshipsStateTypeAndFactVsInference(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	b := graphtest.NewBuilder(t, e.pool)
	inf := b.Inference(e.ids.ReceitaCol, claims.Controls, graphtest.Ent(e.ids.H), graphtest.Ent(e.ids.A), 0.7)
	base := "/api/v1/entities/" + itoa(e.ids.A) + "/relationships"

	rec := e.do("GET", base, tokAnalyst, "")
	open := want[relPage](t, rec, http.StatusOK)
	if len(open.Items) != 6 || open.NextCursor != "" {
		t.Fatalf("open edges of A: %d (next %q)", len(open.Items), open.NextCursor)
	}
	if !strings.Contains(rec.Body.String(), `"confidence":null`) {
		t.Fatal("facts must carry confidence null")
	}
	for _, it := range open.Items {
		switch it.RelationshipID {
		case inf:
			if it.Kind != "inference" || it.Confidence == nil || *it.Confidence != 0.7 {
				t.Errorf("inference edge %+v", it)
			}
		case e.ids.RelHA:
			if it.Kind != "fact" || it.Confidence != nil || it.ValidFrom == nil || *it.ValidFrom != "2020-01-01" ||
				it.SourceKey != "receita" || it.CollectionID != e.ids.ReceitaCol || it.Stale || it.Attrs["role"] != "22" {
				t.Errorf("H→A edge %+v", it)
			}
		}
	}

	for _, it := range open.Items {
		if it.Closed == nil || *it.Closed || it.IdentityBasis == nil || *it.IdentityBasis != "none" {
			t.Errorf("open edge %d: closed %v identity_basis %v, want false and none (single-record persons)",
				it.RelationshipID, it.Closed, it.IdentityBasis)
		}
	}
	closed := want[relPage](t, e.do("GET", base+"?state=closed", tokAnalyst, ""), http.StatusOK)
	if len(closed.Items) != 1 || closed.Items[0].Closed == nil || !*closed.Items[0].Closed {
		t.Fatalf("closed edge without closed: true: %+v", closed.Items)
	}
	if len(closed.Items) != 1 || closed.Items[0].RelationshipID != e.ids.RelP3A ||
		closed.Items[0].ValidToAfter == nil || *closed.Items[0].ValidToAfter != "2025-06-01" ||
		*closed.Items[0].ValidToBefore != "2025-07-01" {
		t.Fatalf("closed edges of A: %+v", closed.Items)
	}
	all := want[relPage](t, e.do("GET", base+"?state=all", tokAnalyst, ""), http.StatusOK)
	if len(all.Items) != 7 {
		t.Fatalf("all edges of A: %d, want 7", len(all.Items))
	}
	typed := want[relPage](t, e.do("GET", base+"?type=controls&type=administrator_of", tokAnalyst, ""), http.StatusOK)
	typed2 := want[relPage](t, e.do("GET", base+"?type=controls,administrator_of", tokAnalyst, ""), http.StatusOK)
	if len(typed.Items) != 1 || typed.Items[0].RelationshipID != inf || len(typed2.Items) != 1 {
		t.Fatalf("typed: %+v / %+v", typed.Items, typed2.Items)
	}
	// On 2015-01-01 the P3 → A partnership (2010 to mid-2025) was valid; H → A (from 2020) was not.
	at := want[relPage](t, e.do("GET", base+"?at=2015-01-01", tokAnalyst, ""), http.StatusOK)
	var ids []int64
	for _, it := range at.Items {
		ids = append(ids, it.RelationshipID)
	}
	if !contains(ids, e.ids.RelP3A) || contains(ids, e.ids.RelHA) {
		t.Fatalf("edges at 2015-01-01: %v", ids)
	}
}

func contains(s []int64, v int64) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestNamesWithCPFRunsAreMasked(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	b, rc := graphtest.FreshReceita(t, e.pool)
	sole := b.Company(rc, "70000001", "JOANA FICTICIA 12345678901", "2135")
	r, p := b.Person(rc, "70000001", "JOANA FICTICIA 12345678901", "***456789**")
	b.Rel(rc, claims.PartnerOf, graphtest.Rec(r, p), graphtest.Ent(sole), nil, nil)
	for _, path := range []string{
		"/api/v1/entities/" + itoa(sole),
		"/api/v1/entities/" + itoa(p),
		"/api/v1/entities/" + itoa(sole) + "/relationships",
	} {
		body := want[map[string]any](t, e.do("GET", path, tokAnalyst, ""), http.StatusOK)
		raw := fmt.Sprint(body)
		if strings.Contains(raw, "12345678901") || !strings.Contains(raw, "JOANA FICTICIA ***456789**") {
			t.Errorf("%s: %s", path, raw)
		}
	}
}

type provenance struct {
	RelationshipID int64 `json:"relationship_id"`
	Source         struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"source"`
	Collection struct {
		ID            int64   `json:"id"`
		ReferenceDate string  `json:"reference_date"`
		ObservedDate  string  `json:"observed_date"`
		CollectedAt   *string `json:"collected_at"`
		Stale         bool    `json:"stale"`
	} `json:"collection"`
	LastSeenCollection *struct {
		ID            int64   `json:"id"`
		ReferenceDate string  `json:"reference_date"`
		ObservedDate  string  `json:"observed_date"`
		CollectedAt   *string `json:"collected_at"`
	} `json:"last_seen_collection"`
	RecordRef  string  `json:"record_ref"`
	Kind       string  `json:"kind"`
	RawKey     *string `json:"raw_key"`
	RawSHA256  *string `json:"raw_sha256"`
	RawSize    *int64  `json:"raw_size"`
	SourceURL  *string `json:"source_url"`
	SourceNote *string `json:"source_note"`
}

func TestProvenanceReturnsSourceAndCollection(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	// The fixture's record_ref is "test:partner_of:<H>:<A>": the file part is
	// everything before the last ':'.
	sha := strings.Repeat("0f", 32)
	fileURL := "https://arquivos.receitafederal.gov.br/public.php/webdav/Dados/Cadastros/CNPJ/2026-10/Socios0.zip"
	if err := collection.RecordSourceFile(bg, e.pool, e.ids.ReceitaCol, collection.File{
		Name: fmt.Sprintf("test:partner_of:%d", e.ids.H), RawKey: "raw/receita/2026-10-01/Socios0.zip",
		SHA256: sha, Size: 1234, SourceURL: fileURL}); err != nil {
		t.Fatal(err)
	}
	// A later collection saw the claim again.
	b := graphtest.NewBuilder(t, e.pool)
	later := b.Done(1, time.Now().UTC().Truncate(24*time.Hour))
	if _, err := e.pool.Exec(bg, `UPDATE relationship SET last_seen_collection_id = $2 WHERE id = $1`, e.ids.RelHA, later); err != nil {
		t.Fatal(err)
	}
	got := want[provenance](t, e.do("GET", fmt.Sprintf("/api/v1/relationships/%d/provenance", e.ids.RelHA), tokAnalyst, ""), http.StatusOK)
	var ref string
	var refDate time.Time
	if err := e.pool.QueryRow(bg, `SELECT r.record_ref, c.reference_date FROM relationship r
		JOIN collection c ON c.id = r.first_collection_id WHERE r.id = $1`, e.ids.RelHA).Scan(&ref, &refDate); err != nil {
		t.Fatal(err)
	}
	if got.RelationshipID != e.ids.RelHA || got.RecordRef != ref || got.Collection.ReferenceDate != refDate.Format("2006-01-02") ||
		got.Collection.ID != e.ids.ReceitaCol || got.Source.Key != "receita" || got.Source.Name == "" ||
		got.Collection.CollectedAt == nil || got.Collection.Stale || got.Kind != "fact" {
		t.Fatalf("provenance %+v", got)
	}
	if got.RawKey == nil || *got.RawKey != "raw/receita/2026-10-01/Socios0.zip" || got.RawSHA256 == nil ||
		*got.RawSHA256 != sha || got.RawSize == nil || *got.RawSize != 1234 {
		t.Fatalf("raw file %v %v %v", got.RawKey, got.RawSHA256, got.RawSize)
	}
	if got.SourceURL == nil || *got.SourceURL != fileURL || got.SourceNote == nil || !strings.Contains(*got.SourceNote, "Receita") {
		t.Fatalf("source url %v note %v", got.SourceURL, got.SourceNote)
	}
	today := time.Now().UTC().Format(time.DateOnly)
	if got.Collection.ObservedDate != today {
		t.Fatalf("first collection observed %q, want %q", got.Collection.ObservedDate, today)
	}
	if l := got.LastSeenCollection; l == nil || l.ID != later || l.ReferenceDate != today || l.ObservedDate != today || l.CollectedAt == nil {
		t.Fatalf("last seen collection %+v, want %d", got.LastSeenCollection, later)
	}
	// TSE claims come from a 2020 collection: stale.
	got = want[provenance](t, e.do("GET", fmt.Sprintf("/api/v1/relationships/%d/provenance", e.ids.RelP2POL), tokAnalyst, ""), http.StatusOK)
	if got.Source.Key != "tse" || !got.Collection.Stale || got.Collection.ReferenceDate != "2020-01-01" {
		t.Fatalf("TSE provenance %+v", got)
	}
	// No file recorded for that collection: the raw fields are absent, not invented.
	rec := e.do("GET", fmt.Sprintf("/api/v1/relationships/%d/provenance", e.ids.RelP2POL), tokAnalyst, "")
	if strings.Contains(rec.Body.String(), "raw_") || !strings.Contains(rec.Body.String(), `"source_url":null`) ||
		strings.Contains(rec.Body.String(), "source_note") {
		t.Fatalf("raw fields without a recorded file: %s", rec.Body.String())
	}
}

// Provenance staleness follows graph: an edition source counts from the
// observed (publication) date, so an edition of 60 days ago published
// yesterday is current; a TSE collection of the same age is stale.
func TestProvenanceStalenessCountsFromThePublicationDate(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	b := graphtest.NewBuilder(t, e.pool)
	now := time.Now().UTC().Truncate(24 * time.Hour)
	begin := func(src int16, ref, observed time.Time) int64 {
		t.Helper()
		c, err := collection.BeginObserved(bg, e.pool, src, ref, observed, collection.Scope{Full: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := collection.Finish(bg, e.pool, c.ID, nil); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	rc := begin(1, now.AddDate(0, 0, -60), now.AddDate(0, 0, -1))
	tc := begin(2, now.AddDate(0, 0, -60), now)
	x := b.Company(rc, "72000001", "EMPRESA FICTICIA EDICAO X LTDA", "2062")
	y := b.Company(rc, "72000002", "EMPRESA FICTICIA EDICAO Y LTDA", "2062")
	relR := b.Rel(rc, claims.PartnerOf, graphtest.Ent(x), graphtest.Ent(y), nil, nil)
	pol := b.Entity(claims.KindPerson, "candidate:7200", "POLITICO FICTICIO EDICAO")
	party := b.Entity(claims.KindParty, "party:7200", "PARTIDO FICTICIO EDICAO")
	relT := b.Rel(tc, claims.CandidateFor, graphtest.Ent(pol), graphtest.Ent(party), nil, nil)
	got := want[provenance](t, e.do("GET", fmt.Sprintf("/api/v1/relationships/%d/provenance", relR), tokAnalyst, ""), http.StatusOK)
	if got.Collection.Stale || got.Collection.ObservedDate != now.AddDate(0, 0, -1).Format(time.DateOnly) {
		t.Fatalf("receita edition published yesterday: %+v, want fresh", got.Collection)
	}
	got = want[provenance](t, e.do("GET", fmt.Sprintf("/api/v1/relationships/%d/provenance", relT), tokAnalyst, ""), http.StatusOK)
	if !got.Collection.Stale {
		t.Fatalf("tse data of 60 days ago: %+v, want stale", got.Collection)
	}
}

func TestProvenanceMarksStaleCollection(t *testing.T) {
	t.Parallel()
	later := time.Now().AddDate(0, 0, 60)
	e := newEnv(t, true, func(d *api.Deps) { d.Now = func() time.Time { return later } })
	got := want[provenance](t, e.do("GET", fmt.Sprintf("/api/v1/relationships/%d/provenance", e.ids.RelHA), tokAnalyst, ""), http.StatusOK)
	if !got.Collection.Stale {
		t.Fatalf("60 days after the Receita collection: %+v, want stale", got)
	}
}

func TestEdgeAttrsAllowlist(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	b, rc := graphtest.FreshReceita(t, e.pool)
	x := b.Company(rc, "71000001", "EMPRESA FICTICIA X LTDA", "2062")
	y := b.Company(rc, "71000002", "EMPRESA FICTICIA Y LTDA", "2062")
	b.Rel(rc, claims.ShareholderOf, graphtest.Ent(x), graphtest.Ent(y), map[string]any{
		"role": "22", "percent": "51.5", "receipt_id": "12345678901", "ballot_name": "JOANA 12345678901",
		"name": "JOANA FICTICIA", "nested": map[string]any{"role": "x"}, "percent_on": nil,
	}, nil)
	page := want[relPage](t, e.do("GET", "/api/v1/entities/"+itoa(x)+"/relationships", tokAnalyst, ""), http.StatusOK)
	if len(page.Items) != 1 {
		t.Fatalf("items %+v", page.Items)
	}
	a := page.Items[0].Attrs
	if len(a) != 4 || a["role"] != "22" || a["percent"] != "51.5" || a["receipt_id"] != "12345678901" ||
		a["ballot_name"] != "JOANA ***456789**" {
		t.Fatalf("attrs %v, want role, percent, the receipt id intact and a masked ballot_name", a)
	}
}
