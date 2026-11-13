package receita

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"geoint/internal/collection"
)

// Fixture facts (testdata/receita/README.md).
const (
	fixtureCompanies = 30  // 29 distinct bases in Empresas + 1 orphan company partner
	fixtureRows      = 104 // records: 31 Empresas + 30 Estabelecimentos + 43 Socios
	p1Norm           = "JOSE CONCEICAO BRANDAO"
)

func TestLoadCreatesCompaniesPartnersAndProvenance(t *testing.T) {
	e := newEnv(t)
	col, stats := e.loadFixture(t)

	if n := e.count(t, `SELECT count(*) FROM entity WHERE kind='company'`); n != fixtureCompanies {
		t.Errorf("company entities = %d, want %d", n, fixtureCompanies)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship`); n == 0 {
		t.Fatal("no relationships")
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE first_collection_id <> $1 OR last_seen_collection_id <> $1`, col.ID); n != 0 {
		t.Errorf("%d relationships without this collection as first/last seen", n)
	}
	refFormat := regexp.MustCompile(`^Socios\.csv:[1-9][0-9]*$`)
	for _, ref := range e.strings(t, `SELECT record_ref FROM relationship`) {
		if !refFormat.MatchString(ref) {
			t.Errorf("relationship record_ref %q, want Socios.csv:<line>", ref)
		}
	}
	for _, ref := range e.strings(t, `SELECT record_ref FROM company_version`) {
		if !regexp.MustCompile(`^Empresas\.csv:[1-9][0-9]*$`).MatchString(ref) {
			t.Errorf("company_version record_ref %q", ref)
		}
	}
	// Socios.csv line 1 is the first partner row of ALFA PARTICIPAÇÕES.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE record_ref='Socios.csv:1' AND rel_type='partner_of'`); n != 1 {
		t.Errorf("partner_of from Socios.csv:1 = %d, want 1", n)
	}
	if stats["relationships_inserted"] != int64(e.count(t, `SELECT count(*) FROM relationship`)) {
		t.Errorf("stats relationships_inserted = %d", stats["relationships_inserted"])
	}
	if stats["entities_company_inserted"] != fixtureCompanies {
		t.Errorf("stats entities_company_inserted = %d", stats["entities_company_inserted"])
	}
}

func TestPJPartnerLinksCompanyToCompany(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type='partner_of'
		AND src_entity_id=$1 AND dst_entity_id=$2 AND src_record_id IS NULL AND dst_record_id IS NULL
		AND attrs->>'role'='22' AND valid_from='2011-01-01'`, e.company(t, "98990001"), e.company(t, "98990002"))
	if n != 1 {
		t.Errorf("ALFA PARTICIPAÇÕES partner_of ALFA INVESTIMENTOS = %d, want 1", n)
	}
	// Third level of the chain.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type='partner_of' AND src_entity_id=$1 AND dst_entity_id=$2`,
		e.company(t, "98990002"), e.company(t, "98990003")); n != 1 {
		t.Errorf("ALFA INVESTIMENTOS partner_of ALFA COMÉRCIO = %d, want 1", n)
	}
}

func TestPersonPartnerCreatesOneRecordPerCompany(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	ctxs := e.strings(t, `SELECT context_key FROM person_record WHERE name_norm=$1 AND cpf_fragment='123456' ORDER BY 1`, p1Norm)
	if strings.Join(ctxs, ",") != "98990001,98990003,98990005" {
		t.Fatalf("records of the person in three companies: %v", ctxs)
	}
	if n := e.count(t, `SELECT count(DISTINCT entity_id) FROM person_record WHERE name_norm=$1`, p1Norm); n != 3 {
		t.Errorf("entities = %d, want 3 (one per record until identity resolution)", n)
	}
	// Latin-1 decoding of an accented name.
	names := e.strings(t, `SELECT DISTINCT name FROM person_record WHERE name_norm=$1`, p1Norm)
	if len(names) != 1 || names[0] != "JOSÉ CONCEIÇÃO BRANDÃO" {
		t.Errorf("names = %q", names)
	}
	if n := e.count(t, `SELECT count(*) FROM entity e JOIN person_record p ON p.entity_id=e.id
		WHERE p.name_norm=$1 AND e.kind='person' AND e.name='JOSÉ CONCEIÇÃO BRANDÃO' AND e.natural_key IS NULL AND p.identity_confidence=1`, p1Norm); n != 3 {
		t.Errorf("person entities with the decoded name = %d, want 3", n)
	}
	// Every relationship citing a record points at the record's entity.
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id=r.src_record_id WHERE r.src_entity_id<>p.entity_id`); n != 0 {
		t.Errorf("%d relationships disagree with their record's entity", n)
	}
}

func TestMalformedRowsAreSkippedAndCounted(t *testing.T) {
	e := newEnv(t)
	_, stats := e.loadFixture(t)
	if stats["rows_skipped"] != 4 {
		t.Errorf("rows_skipped = %d, want 4", stats["rows_skipped"])
	}
	if stats["rows_read"] != fixtureRows {
		t.Errorf("rows_read = %d, want %d", stats["rows_read"], fixtureRows)
	}
	if stats["empresas_skipped"] != 1 || stats["socios_skipped"] != 3 || stats["estabelecimentos_skipped"] != 0 {
		t.Errorf("per-file skipped: %v", stats)
	}
	// The rows next to the malformed ones were loaded.
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key='98990004'`); n != 1 {
		t.Errorf("records of the single-partner company = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM entity WHERE natural_key='98990099'`); n != 0 {
		t.Error("the malformed Empresas row was loaded")
	}
	// NUL byte removed from a name.
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE name='SIMONE TAVARES' AND name_norm='SIMONE TAVARES'`); n != 1 {
		t.Error("the name with a NUL byte was not loaded cleanly")
	}
}

func tableCounts(t *testing.T, e env) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"entity", "person_record", "relationship", "company_version"} {
		out[tbl] = e.count(t, `SELECT count(*) FROM `+tbl)
	}
	return out
}

func TestLoadTwiceSameCollectionDateIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	first := tableCounts(t, e)
	col2, stats := e.loadFixture(t)
	second := tableCounts(t, e)
	for k, v := range first {
		if second[k] != v {
			t.Errorf("%s: %d rows after the first load, %d after the second", k, v, second[k])
		}
	}
	// Seen again: every open row now has the second collection as last seen.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NULL AND last_seen_collection_id <> $1`, col2.ID); n != 0 {
		t.Errorf("%d relationships not marked seen by the second collection", n)
	}
	if n := e.count(t, `SELECT count(*) FROM company_version WHERE closed_collection_id IS NULL AND last_seen_collection_id <> $1`, col2.ID); n != 0 {
		t.Errorf("%d company versions not marked seen by the second collection", n)
	}
	if n := e.count(t, `SELECT count(*) FROM company_version WHERE closed_collection_id IS NOT NULL`); n != 0 {
		t.Errorf("%d versions closed by an identical edition", n)
	}
	if stats["relationships_inserted"] != 0 || stats["relationships_seen"] != int64(first["relationship"]) ||
		stats["person_records_inserted"] != 0 || stats["company_versions_seen"] != int64(first["company_version"]) {
		t.Errorf("second load stats: %v", stats)
	}
}

func TestZeroDateBecomesNullValidFrom(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	for _, base := range []string{"98990013", "98990014"} { // dates "00000000" and ""
		n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type='partner_of' AND dst_entity_id=$1 AND valid_from IS NULL`, e.company(t, base))
		if n != 1 {
			t.Errorf("%s: partner_of with NULL valid_from = %d, want 1", base, n)
		}
	}
}

func TestSliceScopeLoadsOnlyRequestedCNPJsAndNeighbors(t *testing.T) {
	e := newEnv(t)
	// ALFA INVESTIMENTOS, its company partner and the company it is a partner of.
	e.load(t, refOct, fixtureZips(t, nil), collection.Scope{CNPJs: []string{"98990002000100"}, Depth: 1})
	got := e.strings(t, `SELECT natural_key FROM entity WHERE kind='company' ORDER BY 1`)
	if strings.Join(got, ",") != "98990001,98990002,98990003" {
		t.Errorf("companies = %v, want the chain 98990001,98990002,98990003", got)
	}
	ctxs := e.strings(t, `SELECT DISTINCT context_key FROM person_record ORDER BY 1`)
	if strings.Join(ctxs, ",") != "98990001,98990002,98990003" {
		t.Errorf("person record contexts = %v", ctxs)
	}
	// All partners of the selected companies, foreign ones included.
	if n := e.count(t, `SELECT count(*) FROM entity WHERE kind='foreign'`); n != 1 {
		t.Errorf("foreign entities = %d, want 1 (partner of 98990001)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM company_version`); n != 3 {
		t.Errorf("company versions = %d, want 3", n)
	}
	// Depth 0: only the company and its person/foreign partners.
	e2 := newEnv(t)
	e2.load(t, refOct, fixtureZips(t, nil), collection.Scope{CNPJs: []string{"98990002"}, Depth: 0})
	if got := e2.strings(t, `SELECT natural_key FROM entity WHERE kind='company'`); strings.Join(got, ",") != "98990002" {
		t.Errorf("depth 0 companies = %v", got)
	}
}

func TestEmptyNamePersonsLoadAndStaySeparatePerCompany(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	rows := e.strings(t, `SELECT context_key || '/' || cpf_fragment FROM person_record WHERE name_norm='' ORDER BY 1`)
	if strings.Join(rows, ",") != "98990006/111222,98990007/,98990007/111222" {
		t.Errorf("empty-name records = %v", rows)
	}
	if n := e.count(t, `SELECT count(DISTINCT entity_id) FROM person_record WHERE name_norm=''`); n != 3 {
		t.Errorf("empty-name entities = %d, want 3", n)
	}
}

func TestOrphanPartnerCreatesCompanyEntity(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	orphan := e.company(t, "98999999")
	if n := e.count(t, `SELECT count(*) FROM entity WHERE id=$1 AND name='ÔMEGA HOLDING DO BRASIL LTDA' AND name_norm='OMEGA HOLDING DO BRASIL LTDA'`, orphan); n != 1 {
		t.Error("orphan company entity has no name from the partner row")
	}
	if n := e.count(t, `SELECT count(*) FROM company_version WHERE entity_id=$1`, orphan); n != 0 {
		t.Errorf("orphan has %d company versions, want 0", n)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type='partner_of' AND src_entity_id=$1 AND dst_entity_id=$2`, orphan, e.company(t, "98990008")); n != 1 {
		t.Error("orphan partner_of missing")
	}
}

func TestForeignPartnersKeys(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	keys := e.strings(t, `SELECT natural_key FROM entity WHERE kind='foreign' ORDER BY 1`)
	want := []string{"receita:98990001:NORTHWIND HOLDINGS LIMITED", "receita:98990009:#37", "receita:98990009:#38"}
	if !slices.Equal(keys, want) {
		t.Errorf("foreign keys = %q, want %q", keys, want)
	}
	// Its attorney is a person record in the company's context, legal_rep_of the foreign partner.
	n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id=r.src_record_id
		JOIN entity f ON f.id=r.dst_entity_id
		WHERE r.rel_type='legal_rep_of' AND p.context_key='98990001' AND p.name='CARLOS ALBERTO FICTÍCIO'
		AND p.cpf_fragment='654321' AND f.kind='foreign' AND r.dst_record_id IS NULL AND r.attrs->>'role'='17'`)
	if n != 1 {
		t.Errorf("attorney legal_rep_of foreign partner = %d, want 1", n)
	}
}

func TestAdministratorAndLegalRepresentative(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	// Qualification 49 (Sócio-Administrador) gives administrator_of; 22 (Sócio) does not.
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id=r.src_record_id
		WHERE r.rel_type='administrator_of' AND p.name_norm=$1`, p1Norm); n != 2 {
		t.Errorf("administrator_of of the person in three companies = %d, want 2 (qualification 49 twice, 22 once)", n)
	}
	// The minor's mother is a partner too: one record, partner_of and legal_rep_of the minor.
	mother := e.id(t, `SELECT id FROM person_record WHERE context_key='98990012' AND name_norm='PATRICIA MENDES' AND cpf_fragment='678901'`)
	minor := e.id(t, `SELECT id FROM person_record WHERE context_key='98990012' AND name_norm='LUCAS HENRIQUE MENDES'`)
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type='legal_rep_of' AND src_record_id=$1 AND dst_record_id=$2
		AND attrs->>'role'='14' AND valid_from IS NULL`, mother, minor); n != 1 {
		t.Error("legal_rep_of mother -> minor missing")
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type='partner_of' AND src_record_id=$1`, mother); n != 1 {
		t.Error("mother's own partner_of missing")
	}
}

func TestCompanyVersionData(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	var name, nature, size, uf, city, cnae, status, capital string
	err := e.pool.QueryRow(ctx, `SELECT name, legal_nature, size, uf, city, cnae, status, capital::text FROM company_version WHERE entity_id=$1`,
		e.company(t, "00000000")).Scan(&name, &nature, &size, &uf, &city, &cnae, &status, &capital)
	if err != nil {
		t.Fatal(err)
	}
	if name != "BANCO DO BRASIL SA" || nature != "2038" || size != "05" || uf != "DF" || city != "9701" ||
		cnae != "6422100" || status != "02" || capital != "120000000000.00" {
		t.Errorf("version = %q %q %q %q %q %q %q %q", name, nature, size, uf, city, cnae, status, capital)
	}
	// Branches (filiais) are not loaded: ALFA PARTICIPAÇÕES keeps its head-office city.
	if n := e.count(t, `SELECT count(*) FROM company_version WHERE entity_id=$1 AND uf='RJ'`, e.company(t, "98990001")); n != 1 {
		t.Error("head office data not used")
	}
	// No head-office row: the version still exists, without address data.
	if n := e.count(t, `SELECT count(*) FROM company_version WHERE entity_id=$1 AND uf='' AND status=''`, e.company(t, "98990019")); n != 1 {
		t.Error("company without head office has no version")
	}
	// Doubled quotes and the duplicate cnpj_basico.
	if n := e.count(t, `SELECT count(*) FROM company_version WHERE name='TETA COMÉRCIO "BOA VISTA" LTDA'`); n != 1 {
		t.Error("duplicate/doubled-quote company not loaded exactly once")
	}
}

func TestAlphanumericCNPJ(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	eta := e.company(t, "98AB0001")
	if n := e.count(t, `SELECT count(*) FROM company_version WHERE entity_id=$1 AND name='ETA TECNOLOGIA LTDA'`, eta); n != 1 {
		t.Error("alphanumeric company has no version")
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type='partner_of' AND src_entity_id=$1 AND dst_entity_id=$2`, eta, e.company(t, "98990020")); n != 1 {
		t.Error("alphanumeric company partner_of missing")
	}
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key='98AB0001'`); n != 1 {
		t.Error("partner of the alphanumeric company missing")
	}
}

func TestChangedCompanyClosesOldVersionAndOpensNew(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	col2, stats := e.load(t, refNov, fixtureZips(t, variant), collection.Scope{Full: true})
	bakery := e.company(t, "98990004")
	if n := e.count(t, `SELECT count(*) FROM company_version WHERE entity_id=$1 AND name='PADARIA SOL NASCENTE LTDA' AND closed_collection_id=$2`, bakery, col2.ID); n != 1 {
		t.Error("old version not closed by the second collection")
	}
	if n := e.count(t, `SELECT count(*) FROM company_version WHERE entity_id=$1 AND name='PADARIA SOL NASCENTE COMERCIO LTDA'
		AND closed_collection_id IS NULL AND first_collection_id=$2 AND last_seen_collection_id=$2`, bakery, col2.ID); n != 1 {
		t.Error("new open version missing")
	}
	if n := e.count(t, `SELECT count(*) FROM entity WHERE id=$1 AND name='PADARIA SOL NASCENTE LTDA'`, bakery); n != 1 {
		t.Error("the entity must keep its first name")
	}
	if stats["company_versions_closed"] != 1 || stats["company_versions_inserted"] != 1 {
		t.Errorf("stats: %v", stats)
	}
}

func TestRelationshipUsesTheRecordsCurrentEntity(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	// Identity resolution merged two records of the person into another entity.
	merged := e.id(t, `INSERT INTO entity (kind, name, name_norm) VALUES ('person', 'JOSÉ CONCEIÇÃO BRANDÃO', $1) RETURNING id`, p1Norm)
	if _, err := e.pool.Exec(ctx, `UPDATE person_record SET entity_id=$1 WHERE name_norm=$2 AND context_key IN ('98990001', '98990003')`, merged, p1Norm); err != nil {
		t.Fatal(err)
	}
	e.load(t, refNov, fixtureZips(t, variant), collection.Scope{Full: true})

	// 98990001: the row changed qualification, so new claims were inserted.
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id=r.src_record_id
		WHERE p.context_key='98990001' AND p.name_norm=$1 AND r.attrs->>'role'='05' AND r.src_entity_id=$2`, p1Norm, merged); n != 2 {
		t.Errorf("new partner_of/administrator_of with the current entity = %d, want 2", n)
	}
	// 98990003: unchanged row, seen again and kept on the current entity.
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id=r.src_record_id
		WHERE p.context_key='98990003' AND p.name_norm=$1 AND r.src_entity_id=$2`, p1Norm, merged); n != 1 {
		t.Errorf("re-seen partner_of on the current entity = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE name_norm=$1`, p1Norm); n != 3 {
		t.Errorf("person records = %d, want 3 (no new record for a known key)", n)
	}
}

func TestLoadRecordsRawFiles(t *testing.T) {
	e := newEnv(t)
	col, _ := e.loadFixture(t)
	type file struct {
		name, key, sha string
		size           int64
	}
	read := func(colID int64) []file {
		t.Helper()
		rows, err := e.pool.Query(ctx, `SELECT name, raw_key, sha256, size_bytes FROM collection_file
			WHERE collection_id = $1 ORDER BY name`, colID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []file
		for rows.Next() {
			var f file
			if err := rows.Scan(&f.name, &f.key, &f.sha, &f.size); err != nil {
				t.Fatal(err)
			}
			out = append(out, f)
		}
		return out
	}
	files := read(col.ID)
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.name
		st, ok, err := e.raw.Stat(ctx, f.key)
		if err != nil || !ok {
			t.Fatalf("%s: stored object %s: ok=%v err=%v", f.name, f.key, ok, err)
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(f.sha) || f.sha != st.SHA256 || f.size != st.Size {
			t.Errorf("%s: recorded %s/%d, stored object has %s/%d", f.name, f.sha, f.size, st.SHA256, st.Size)
		}
	}
	if !slices.Equal(names, []string{"Empresas.csv", "Estabelecimentos.csv", "Socios.csv"}) {
		t.Fatalf("recorded files %v", names)
	}
	// Every relationship's record_ref names a recorded file of its collection.
	if n := e.count(t, `SELECT count(*) FROM relationship r WHERE NOT EXISTS (SELECT 1 FROM collection_file f
		WHERE f.collection_id = r.first_collection_id AND f.name = split_part(r.record_ref, ':', 1))`); n != 0 {
		t.Errorf("%d relationships without a recorded raw file", n)
	}
	col2, _ := e.loadFixture(t)
	if got := read(col2.ID); len(got) != 3 || got[2].sha != files[2].sha {
		t.Fatalf("second collection files %+v", got)
	}
}
