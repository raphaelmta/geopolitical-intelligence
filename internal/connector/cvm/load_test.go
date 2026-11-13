package cvm

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/connector/receita"
	"geoint/internal/graph"
	"geoint/internal/rawstore"
)

func TestShareholderPercentIsStored(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	holding := e.company(t, baseHolding)
	want := map[string]string{
		"role": "shareholder", "percent": "51.37", "percent_on": "51.37", "percent_pn": "0",
		"reference_date": "2026-12-31", "form_version": "2",
	}
	for k, v := range want {
		if got := e.openAttr(t, "shareholder_of", holding, baseAlfa, k); got != v {
			t.Errorf("Alfa Holding -> Alfa attrs.%s = %q, want %q", k, got, v)
		}
	}
	// The percent is a JSON string, never a float.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'shareholder_of' AND jsonb_typeof(attrs -> 'percent') <> 'string'`); n != 0 {
		t.Errorf("%d shareholder_of rows with a non-string percent", n)
	}
	cases := []struct {
		src  int64
		dst  string
		want string
	}{
		{e.record(t, baseAlfa, "ANA FICTÍCIA CONTROLADORA"), baseAlfa, "10"},
		{e.foreign(t, keyFictitious), baseAlfa, "8"},
		{e.company(t, baseDelta), baseBeta, "30"},
		{e.foreign(t, keyInventada), baseBeta, "3.5"},
		{e.company(t, baseOmega), baseGama, "75"},
	}
	for _, c := range cases {
		got := e.strings(t, `SELECT attrs ->> 'percent' FROM relationship r JOIN entity d ON d.id = r.dst_entity_id
			WHERE rel_type = 'shareholder_of' AND (r.src_record_id = $1 OR (r.src_record_id IS NULL AND r.src_entity_id = $1)) AND d.natural_key = $2`, c.src, c.dst)
		if joined(got) != c.want {
			t.Errorf("percent of %d in %s = %v, want %s", c.src, c.dst, got, c.want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'shareholder_of' AND valid_from IS NOT NULL`); n != 0 {
		t.Errorf("%d shareholder_of rows with valid_from", n)
	}
}

func TestDeclaredControllerCreatesControlsEdge(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	alfa := e.company(t, baseAlfa)
	got := e.strings(t, `SELECT coalesce(s.natural_key, p.name) || ' ' || (r.attrs ->> 'role') || ' ' || (r.attrs ->> 'reference_date') || ' v' || (r.attrs ->> 'form_version')
		FROM relationship r JOIN entity s ON s.id = r.src_entity_id LEFT JOIN person_record p ON p.id = r.src_record_id
		WHERE r.rel_type = 'controls' AND r.dst_entity_id = $1 ORDER BY 1`, alfa)
	want := "98991101 controller 2026-12-31 v2,ANA FICTÍCIA CONTROLADORA controller 2026-12-31 v2"
	if joined(got) != want {
		t.Errorf("controls into Alfa = %v, want %s", got, want)
	}
	// The individual controller is cited through her person record.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'controls' AND src_record_id = $1`,
		e.record(t, baseAlfa, "ANA FICTÍCIA CONTROLADORA")); n != 1 {
		t.Errorf("controls from Ana's record = %d, want 1", n)
	}
	// Beta has no declared controller; Gama's 75 % holder is not flagged either.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'controls'`); n != fixtureControls {
		t.Errorf("controls = %d, want %d", n, fixtureControls)
	}
}

// receitaFixture loads testdata/receita with the real Receita connector.
func receitaFixture(t *testing.T, e env) {
	t.Helper()
	col, err := collection.Begin(ctx, e.pool, 1, refA, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	var files []rawstore.File
	for _, name := range []string{"Empresas", "Estabelecimentos", "Socios"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "receita", name+".csv"))
		if err != nil {
			t.Fatal(err)
		}
		f, err := e.raw.Put(ctx, e.prefix+"receita/"+name+"0.zip", bytes.NewReader(zipOf(t, map[string][]byte{name + ".csv": b})))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	stats, err := receita.New("http://127.0.0.1:1/unused/").Load(ctx, col, files, e.raw, e.pool)
	if err != nil {
		t.Fatalf("receita Load: %v", err)
	}
	if err := collection.Finish(ctx, e.pool, col.ID, stats); err != nil {
		t.Fatal(err)
	}
}

func TestCompanyMatchesReceitaEntityByCNPJ(t *testing.T) {
	e := newEnv(t)
	receitaFixture(t, e)
	alfa := e.company(t, baseAlfa)
	before := e.count(t, `SELECT count(*) FROM entity WHERE kind = 'company'`)
	e.loadFixture(t)
	if got := e.company(t, baseAlfa); got != alfa {
		t.Errorf("Alfa entity = %d after the CVM load, want the Receita's %d", got, alfa)
	}
	// The Receita name stays (first-seen rule); no company version is written by the CVM.
	if n := e.strings(t, `SELECT name FROM entity WHERE id = $1`, alfa); joined(n) != "ALFA PARTICIPAÇÕES S.A." {
		t.Errorf("Alfa name = %v", n)
	}
	if n := e.count(t, `SELECT count(*) FROM company_version v JOIN collection c ON c.id = v.first_collection_id WHERE c.source_id = $1`, sourceID); n != 0 {
		t.Errorf("%d company versions written by the CVM", n)
	}
	// The CVM claims point at the Receita entity: one company more per new CNPJ base only.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type IN ('shareholder_of', 'controls') AND dst_entity_id = $1`, alfa); n != 6 {
		t.Errorf("claims into Alfa = %d, want 6", n)
	}
	after := e.count(t, `SELECT count(*) FROM entity WHERE kind = 'company'`)
	// New: Beta, Gama, Alfa Holding, Delta, Omega, Alfa Controle, Omega
	// Fundador (Alfa already existed).
	if after-before != 7 {
		t.Errorf("company entities created = %d, want 7", after-before)
	}
}

func TestDocumentedControlUsesCVMPercent(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	g := graph.New(e.pool)

	// Gama: no declared controller; Omega holds 75 % of the total capital
	// (a direct line), and only Omega's edge into Gama carries control there.
	res, err := g.ControlGroup(ctx, e.company(t, baseGama), graph.ControlOptions{Mode: graph.Documented}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ctrl []string
	for _, ed := range res.Edges {
		if ed.Dst.NaturalKey == baseGama {
			ctrl = append(ctrl, string(ed.Type)+" "+ed.Src.NaturalKey)
		}
	}
	if joined(ctrl) != "shareholder_of "+baseOmega {
		t.Errorf("Gama control edges = %v, want only Omega's shareholder_of", ctrl)
	}

	// Alfa: the chain climbs through the indirect lines: Alfa <- Alfa
	// Holding (51.37 %) <- Alfa Controle (70 %) <- Fernanda (100 %); Ana is
	// a declared controller of Alfa. Eduardo (30 %) and the 8 % foreign
	// holder carry no control.
	res, err = g.ControlGroup(ctx, e.company(t, baseAlfa), graph.ControlOptions{Mode: graph.Documented}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctrl = nil
	for _, c := range res.Controllers {
		ctrl = append(ctrl, c.Name)
	}
	slices.Sort(ctrl)
	if joined(ctrl) != "ANA FICTÍCIA CONTROLADORA,FERNANDA FICTÍCIA NETA" {
		t.Errorf("Alfa controllers = %v", ctrl)
	}
	var shareholderEdges []string
	for _, ed := range res.Edges {
		if ed.Type == claims.ShareholderOf {
			shareholderEdges = append(shareholderEdges, ed.Src.Name+">"+ed.Dst.Name)
		}
	}
	slices.Sort(shareholderEdges)
	if want := "ALFA CONTROLE LTDA>ALFA HOLDING S.A.,ALFA HOLDING S.A.>ALFA PARTICIPAÇÕES S.A.,FERNANDA FICTÍCIA NETA>ALFA CONTROLE LTDA"; joined(shareholderEdges) != want {
		t.Errorf("Alfa shareholder_of control edges %v, want %s", shareholderEdges, want)
	}

	// Gama <- Omega (75 %) <- Omega Fundador (90 %, Beta's description, not
	// declared controller): the percent alone climbs both levels.
	res, err = g.ControlGroup(ctx, e.company(t, baseGama), graph.ControlOptions{Mode: graph.Documented}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Controllers) != 1 || res.Controllers[0].NaturalKey != baseFounder {
		t.Errorf("Gama controllers = %+v, want %s", res.Controllers, baseFounder)
	}

	// Delta: its group is all zeros (no percent stored): only the declared
	// controller Helio climbs, the foreign holder without percent does not.
	res, err = g.ControlGroup(ctx, e.company(t, baseDelta), graph.ControlOptions{Mode: graph.Documented}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Controllers) != 1 || res.Controllers[0].Name != "HELIO ZERO FICTÍCIO" {
		t.Errorf("Delta controllers = %+v, want Helio only", res.Controllers)
	}

	// Beta: nobody above 50 % and nobody declared.
	res, err = g.ControlGroup(ctx, e.company(t, baseBeta), graph.ControlOptions{Mode: graph.Documented}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Controllers) != 0 {
		t.Errorf("Beta controllers = %+v, want none", res.Controllers)
	}
}

func TestMalformedRowsAreSkippedAndCounted(t *testing.T) {
	e := newEnv(t)
	_, stats := e.loadFixture(t)
	want := map[string]int64{
		"rows_read": fixtureRows, "rows_skipped": fixtureMalformed,
		"malformed_columns": 1, "malformed_percent": 2, "malformed_cnpj": 2, "latest_form_lines_skipped": 3,
		"superseded_rows": 5, "aggregate_outros": 7, "aggregate_tesouraria": 3,
		"duplicate_shareholder": 2, "duplicate_conflict": 1,
		"indirect_parent_not_company": 1, "indirect_parent_missing": 0, "indirect_superseded_by_own_form": 2,
		"indirect_superseded": 2, "indirect_conflict": 1, "holdings_described": 4, "indirect_shareholders_loaded": 6,
		"forms_loaded": 3, "companies_from_previous_year": 1, "year_not_published": 0,
		"shareholders_loaded":    fixtureShareholderOf,
		"relationships_inserted": fixtureRels, "relationships_seen": 0, "relationships_replaced": 0,
	}
	for k, v := range want {
		if stats[k] != v {
			t.Errorf("stats[%s] = %d, want %d", k, stats[k], v)
		}
	}
	// No malformed row was shifted into a claim: their names never appear.
	for _, name := range []string{"LINHA CURTA LTDA", "MALFORMADO PERCENTUAL LTDA", "MALFORMADO EXCESSO LTDA", "MALFORMADO DOCUMENTO LTDA", "COMPANHIA SEM CNPJ LTDA"} {
		if n := e.count(t, `SELECT count(*) FROM entity WHERE name = $1`, name); n != 0 {
			t.Errorf("malformed row %s became an entity", name)
		}
	}
	// The rest of Beta's form is still loaded.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE dst_entity_id = $1`, e.company(t, baseBeta)); n != 5 {
		t.Errorf("claims into Beta = %d, want 5", n)
	}
}

// claimDump lists every relationship and person record with what makes it
// distinct, in a stable order.
func claimDump(t *testing.T, e env) []string {
	t.Helper()
	rels := e.strings(t, `SELECT r.rel_type || '|' || coalesce(s.natural_key, '') || '|' || coalesce(p.context_key || ':' || p.name, '') || '|' ||
			d.natural_key || '|' || r.attrs::text || '|' || r.record_ref || '|' || coalesce(r.closed_collection_id::text, 'open')
		FROM relationship r JOIN entity s ON s.id = r.src_entity_id JOIN entity d ON d.id = r.dst_entity_id
		LEFT JOIN person_record p ON p.id = r.src_record_id ORDER BY 1`)
	recs := e.strings(t, `SELECT context_key || '|' || name || '|' || cpf_fragment || '|' || record_ref FROM person_record ORDER BY 1`)
	return append(rels, recs...)
}

func tableCounts(t *testing.T, e env) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"entity", "person_record", "relationship", "company_version"} {
		out[tbl] = e.count(t, `SELECT count(*) FROM `+tbl)
	}
	return out
}

func TestLoadTwiceIsIdempotent(t *testing.T) {
	e := newEnv(t)
	col1, _ := e.loadFixture(t)
	before := tableCounts(t, e)
	if before["relationship"] != fixtureRels || before["person_record"] != fixtureRecords {
		t.Fatalf("first load counts %v", before)
	}
	dump := claimDump(t, e)
	col2, stats := e.load(t, refB, fixtureZips(t, nil))
	if got := claimDump(t, e); !slices.Equal(got, dump) {
		t.Errorf("claims changed on the second load:\n%v\nwant\n%v", got, dump)
	}
	after := tableCounts(t, e)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %d rows after the first load, %d after the second", k, v, after[k])
		}
	}
	if stats["relationships_inserted"] != 0 || stats["relationships_seen"] != fixtureRels || stats["relationships_replaced"] != 0 {
		t.Errorf("second load stats: %v", stats)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE last_seen_collection_id <> $1 OR first_collection_id <> $2`, col2.ID, col1.ID); n != 0 {
		t.Errorf("%d relationships not first seen by the first and last seen by the second collection", n)
	}
	// Loading the same files again (a retried collection) changes nothing either.
	e.load(t, refB, fixtureZips(t, nil))
	if got := claimDump(t, e); !slices.Equal(got, dump) {
		t.Errorf("claims changed on the third load")
	}
}

func TestEveryClaimHasProvenance(t *testing.T) {
	e := newEnv(t)
	col, _ := e.loadFixture(t)
	refFormat := regexp.MustCompile(`^fre_cia_aberta_posicao_acionaria_20(25|26)\.csv:[0-9]+$`)
	for _, tbl := range []string{"relationship", "person_record"} {
		refs := e.strings(t, `SELECT record_ref FROM `+tbl+` WHERE first_collection_id = $1`, col.ID)
		if len(refs) == 0 {
			t.Fatalf("no %s rows", tbl)
		}
		for _, ref := range refs {
			if !refFormat.MatchString(ref) {
				t.Errorf("%s record_ref %q", tbl, ref)
			}
		}
		if n := e.count(t, `SELECT count(*) FROM `+tbl+` WHERE first_collection_id IS DISTINCT FROM $1`, col.ID); n != 0 {
			t.Errorf("%d %s rows without this collection", n, tbl)
		}
		// Every record_ref names a raw file recorded for the collection, with its checksum.
		if n := e.count(t, `SELECT count(*) FROM `+tbl+` x WHERE NOT EXISTS (SELECT 1 FROM collection_file f
			WHERE f.collection_id = x.first_collection_id AND f.name = split_part(x.record_ref, ':', 1)
			AND f.sha256 ~ '^[0-9a-f]{64}$' AND f.raw_key LIKE $1 AND f.size_bytes > 0)`, e.prefix+"2026-11-10/fre_cia_aberta_20%.zip"); n != 0 {
			t.Errorf("%d %s rows without a recorded raw file", n, tbl)
		}
	}
	files := e.strings(t, `SELECT name FROM collection_file WHERE collection_id = $1 ORDER BY name`, col.ID)
	if want := []string{entryName(2025), entryName(2026)}; !slices.Equal(files, want) {
		t.Errorf("recorded files %v, want %v", files, want)
	}
	// The line points at the row.
	holding := e.company(t, baseHolding)
	if r := e.strings(t, `SELECT record_ref FROM relationship WHERE src_entity_id = $1 ORDER BY rel_type`, holding); joined(r) != entryName(2026)+":2,"+entryName(2026)+":2" {
		t.Errorf("Alfa Holding record_refs = %v", r)
	}
	if r := e.strings(t, `SELECT record_ref FROM relationship WHERE src_entity_id = $1 AND dst_entity_id = $2`, e.company(t, baseOmega), e.company(t, baseGama)); joined(r) != entryName(2025)+":4" {
		t.Errorf("Omega record_ref = %v", r)
	}
	// The duplicate Delta row (line 15) is not the one cited.
	if r := e.strings(t, `SELECT record_ref FROM relationship WHERE src_entity_id = $1`, e.company(t, baseDelta)); joined(r) != entryName(2026)+":11" {
		t.Errorf("Delta record_ref = %v", r)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE kind <> 'fact' OR confidence IS NOT NULL`); n != 0 {
		t.Errorf("%d relationships are not plain facts", n)
	}
}

func TestOnlyTheLatestFormOfEachCompanyIsLoaded(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	// Alfa: the 2026 form (51.37 %), never the 2025 one (50.1 %).
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE attrs ->> 'percent' = '50.1'`); n != 0 {
		t.Error("the superseded 2025 form of Alfa was loaded")
	}
	// Beta: version 2, not version 1 of the same reference date (Delta 40 %).
	if got := e.openAttr(t, "shareholder_of", e.company(t, baseDelta), baseBeta, "percent"); got != "30" {
		t.Errorf("Delta in Beta = %s, want 30 (version 2)", got)
	}
	// Gama: only a 2025 form, still covered; its later reference date wins (75 %, not 70 %).
	if got := e.openAttr(t, "shareholder_of", e.company(t, baseOmega), baseGama, "reference_date"); got != "2025-12-31" {
		t.Errorf("Omega in Gama reference_date = %s", got)
	}
	if got := e.openAttr(t, "shareholder_of", e.company(t, baseOmega), baseGama, "percent"); got != "75" {
		t.Errorf("Omega in Gama percent = %s", got)
	}
	// Ana holds Alfa and Gama: two records, one per listed company.
	got := e.strings(t, `SELECT context_key || ' ' || cpf_fragment FROM person_record WHERE name = 'ANA FICTÍCIA CONTROLADORA' ORDER BY 1`)
	if joined(got) != "cvm:98990001 654321,cvm:98991002 654321" {
		t.Errorf("Ana's records = %v", got)
	}
}

func TestShareholderKinds(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	got := e.strings(t, `SELECT kind || ' ' || natural_key FROM entity WHERE kind = 'foreign' ORDER BY natural_key`)
	if want := "foreign " + keyFictitious + ",foreign " + keyInventada + ",foreign " + keyGlobalZero; joined(got) != want {
		t.Errorf("foreign entities = %v, want %s", got, want)
	}
	recs := e.strings(t, `SELECT context_key || '|' || name || '|' || cpf_fragment FROM person_record WHERE source_id = $1 ORDER BY 1`, sourceID)
	want := "cvm:98990001|ANA FICTÍCIA CONTROLADORA|654321,cvm:98991001|BRUNO EXEMPLO TESTE|654987,cvm:98991001|CARLA SEM DOCUMENTO|," +
		"cvm:98991002|ANA FICTÍCIA CONTROLADORA|654321,cvm:98991101|EDUARDO INVENTADO INDIRETO|123123," +
		"cvm:98991102|HELIO ZERO FICTÍCIO|456456,cvm:98991108|FERNANDA FICTÍCIA NETA|852963"
	if joined(recs) != want {
		t.Errorf("person records = %v, want %s", recs, want)
	}
	// Aggregate lines and holders under an individual are not shareholders.
	for _, name := range []string{"Outros", "Ações Tesouraria", "Outros (registered shareholders)", "FILHA DE PESSOA LTDA"} {
		if n := e.count(t, `SELECT count(*) FROM entity WHERE name = $1`, name) + e.count(t, `SELECT count(*) FROM person_record WHERE name = $1`, name); n != 0 {
			t.Errorf("%s was loaded", name)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE dst_entity_id = $1`, e.company(t, baseHolding)); n != 3 {
		t.Errorf("%d claims into Alfa Holding, want 3 (indirect lines)", n)
	}
	// Listed companies carry the CVM name.
	if got := e.strings(t, `SELECT name FROM entity WHERE kind = 'company' AND natural_key = $1`, baseBeta); joined(got) != "BETA ENERGIA S.A." {
		t.Errorf("Beta name = %v", got)
	}
}

func TestChangedPercentClosesOldRowAndOpensNew(t *testing.T) {
	e := newEnv(t)
	col1, _ := e.loadFixture(t)
	holding := e.company(t, baseHolding)
	oldID := e.id(t, `SELECT id FROM relationship WHERE rel_type = 'shareholder_of' AND src_entity_id = $1`, holding)
	controlsID := e.id(t, `SELECT id FROM relationship WHERE rel_type = 'controls' AND src_entity_id = $1`, holding)

	// A new version of Alfa's form: the holding now has 55 %.
	edit := func(year int, b []byte) []byte {
		if year != 2026 {
			return b
		}
		b = replaceLine(b, "98.990.001/0001-11;2026-12-31;2;900002", ";2;900002;", ";3;900003;")
		return replaceLine(b, ";7000001;ALFA HOLDING", ";51.370000;0;0.000000;5137000;51.370000;", ";55.000000;0;0.000000;5500000;55.000000;")
	}
	col2, stats, closed := e.loadAndClose(t, refB, fixtureZips(t, edit))
	if closed != 0 {
		t.Errorf("CloseUnseen closed %d rows, want 0", closed)
	}
	if stats["relationships_replaced"] != 1 || stats["relationships_inserted"] != 1 || stats["relationships_seen"] != fixtureRels-1 {
		t.Errorf("stats %v", stats)
	}
	var (
		closedBy      *int64
		after, before *time.Time
		lastSeen      int64
	)
	if err := e.pool.QueryRow(ctx, `SELECT closed_collection_id, valid_to_after, valid_to_before, last_seen_collection_id FROM relationship WHERE id = $1`, oldID).
		Scan(&closedBy, &after, &before, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if closedBy == nil || *closedBy != col2.ID {
		t.Errorf("old row closed by %v, want collection %d", closedBy, col2.ID)
	}
	// The same rule as CloseUnseen: the change was observed on col2's
	// observed date (refB here), so the exclusive bound is the day after.
	if after == nil || !after.Equal(refA) || before == nil || !before.Equal(refB.AddDate(0, 0, 1)) {
		t.Errorf("old row ended between %v and %v, want %s and %s", after, before, refA.Format(time.DateOnly), refB.AddDate(0, 0, 1).Format(time.DateOnly))
	}
	if lastSeen != col1.ID {
		t.Errorf("old row last seen by %d, want %d", lastSeen, col1.ID)
	}
	// The old row keeps its attrs; the new open row has the new percent and form.
	if got := e.strings(t, `SELECT attrs ->> 'percent' FROM relationship WHERE id = $1`, oldID); joined(got) != "51.37" {
		t.Errorf("old row percent = %v", got)
	}
	if got := e.openAttr(t, "shareholder_of", holding, baseAlfa, "percent"); got != "55" {
		t.Errorf("open percent = %s, want 55", got)
	}
	if got := e.openAttr(t, "shareholder_of", holding, baseAlfa, "form_version"); got != "3" {
		t.Errorf("open form_version = %s, want 3", got)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'shareholder_of' AND src_entity_id = $1 AND first_collection_id = $2 AND closed_collection_id IS NULL`, holding, col2.ID); n != 1 {
		t.Errorf("new open rows = %d, want 1", n)
	}
	// The unchanged controls claim is only seen again.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE id = $1 AND closed_collection_id IS NULL AND last_seen_collection_id = $2`, controlsID, col2.ID); n != 1 {
		t.Error("the unchanged controls claim was not just seen again")
	}
	// Loading the same edition again changes nothing more.
	_, stats, closed = e.loadAndClose(t, refC, fixtureZips(t, edit))
	if closed != 0 || stats["relationships_replaced"] != 0 || stats["relationships_inserted"] != 0 {
		t.Errorf("third load: closed %d, stats %v", closed, stats)
	}
}

func TestLostControllerStatusIsClosedByCloseUnseen(t *testing.T) {
	e := newEnv(t)
	if _, _, closed := e.loadAndClose(t, refA, fixtureZips(t, nil)); closed != 0 {
		t.Fatalf("first collection closed %d", closed)
	}
	ana := e.record(t, baseAlfa, "ANA FICTÍCIA CONTROLADORA")
	edit := func(year int, b []byte) []byte {
		if year != 2026 {
			return b
		}
		return replaceLine(b, ";7000002;ANA", ";SP;N;;;;;2023-01-02;S;S", ";SP;N;;;;;2023-01-02;N;S")
	}
	col2, _, closed := e.loadAndClose(t, refB, fixtureZips(t, edit))
	if closed != 1 {
		t.Errorf("CloseUnseen closed %d rows, want 1 (Ana's controls)", closed)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'controls' AND src_record_id = $1 AND closed_collection_id = $2`, ana, col2.ID); n != 1 {
		t.Error("Ana's controls claim was not closed")
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'shareholder_of' AND src_record_id = $1 AND closed_collection_id IS NULL`, ana); n != 1 {
		t.Error("Ana's unchanged shareholder_of claim must stay open")
	}
}

func TestReloadWithCloseUnseenClosesNothing(t *testing.T) {
	e := newEnv(t)
	for i, ref := range []time.Time{refA, refB, refC} {
		_, _, closed := e.loadAndClose(t, ref, fixtureZips(t, nil))
		if closed != 0 {
			t.Errorf("collection %d closed %d rows", i+1, closed)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NOT NULL`); n != 0 {
		t.Errorf("%d closed relationships", n)
	}
}

func TestNoFullCPFOrPersonalFieldIsStored(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	// The full CPFs of the fixture, with and without punctuation.
	cpfs := []string{"98765432101", "987.654.321-01", "32165498700", "321.654.987-00", "12312312300", "123.123.123-00",
		"74185296300", "741.852.963-00", "45645645601", "456.456.456-01"}
	dump := strings.Join(e.strings(t, `
		SELECT coalesce(name, '') || ' ' || coalesce(natural_key, '') FROM entity
		UNION ALL SELECT name || ' ' || context_key || ' ' || cpf_fragment || ' ' || record_ref FROM person_record
		UNION ALL SELECT attrs::text || ' ' || record_ref FROM relationship
		UNION ALL SELECT coalesce(stats::text, '') || ' ' || coalesce(error, '') FROM collection`), "\n")
	for _, c := range cpfs {
		if strings.Contains(dump, c) {
			t.Errorf("full CPF %s stored", c)
		}
	}
	// No other personal field: UF, nationality, shareholder agreement, dates of the position.
	for _, s := range []string{"SP", "RJ", "Brasil", "2023-01-02", "acordo", "nacionalidade", "uf"} {
		if n := e.count(t, `SELECT count(*) FROM relationship WHERE attrs::text ILIKE '%' || $1 || '%'`, s); n != 0 {
			t.Errorf("%q found in relationship attrs", s)
		}
	}
	keys := e.strings(t, `SELECT DISTINCT jsonb_object_keys(attrs) FROM relationship ORDER BY 1`)
	if want := "described_by,form_version,percent,percent_on,percent_pn,reference_date,role"; joined(keys) != want {
		t.Errorf("attrs keys = %v, want %s", keys, want)
	}
}

func TestMissingRequiredColumnFailsTheFile(t *testing.T) {
	e := newEnv(t)
	edit := func(year int, b []byte) []byte {
		if year != 2025 {
			return b
		}
		return bytes.Replace(b, []byte("Percentual_Total_Acoes_Circulacao"), []byte("Percentual_Total"), 1)
	}
	_, _, err := e.loadErr(t, refA, fixtureZips(t, edit), collection.Scope{Full: true})
	if err == nil || !strings.Contains(err.Error(), "Percentual_Total_Acoes_Circulacao") || !strings.Contains(err.Error(), entryName(2025)) {
		t.Errorf("err = %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship`); n != 0 {
		t.Errorf("%d relationships written by a failed load", n)
	}
}

func TestOnlyFullScopeIsAccepted(t *testing.T) {
	e := newEnv(t)
	for _, scope := range []collection.Scope{{}, {Full: true, CNPJs: []string{baseAlfa}}} {
		_, _, err := e.loadErr(t, refA, fixtureZips(t, nil), scope)
		if !errors.Is(err, ErrScope) {
			t.Errorf("scope %+v: err = %v, want ErrScope", scope, err)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM entity`) + e.count(t, `SELECT count(*) FROM collection_file`); n != 0 {
		t.Errorf("%d rows written before the scope was refused", n)
	}
}

func TestZipWithoutShareholdingEntryFails(t *testing.T) {
	e := newEnv(t)
	zips := fixtureZips(t, nil)
	zips["fre_cia_aberta_2025.zip"] = zipOf(t, map[string][]byte{"fre_cia_aberta_2025.csv": []byte("x\r\n")})
	_, _, err := e.loadErr(t, refA, zips, collection.Scope{Full: true})
	if err == nil || !strings.Contains(err.Error(), entryName(2025)) {
		t.Errorf("err = %v", err)
	}
}

func TestOversizedEntryIsRefused(t *testing.T) {
	e := newEnv(t)
	e.c.maxEntryBytes = 1000
	_, _, err := e.loadErr(t, refA, fixtureZips(t, nil), collection.Scope{Full: true})
	if err == nil || !strings.Contains(err.Error(), "over the limit") {
		t.Errorf("err = %v", err)
	}
}

func TestChecksumMismatchIsRefused(t *testing.T) {
	e := newEnv(t)
	files := e.store(t, refA, fixtureZips(t, nil))
	files[0].SHA256 = strings.Repeat("0", 64)
	col, err := collection.Begin(ctx, e.pool, sourceID, refA, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Load(ctx, col, files, e.raw, e.pool); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("err = %v", err)
	}
}
