package tse

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"geoint/internal/collection"
)

// Fixture facts (testdata/tse/README.md).
const (
	fixtureRows     = 37 // records: 12 consulta_cand + 25 receitas (UF and BR files only)
	fixtureSkipped  = 4  // 2 malformed candidacies + 2 malformed receipts
	fixtureRecords  = 18 // 9 candidacies + 9 donor records
	fixtureRels     = 24 // 9 candidate_for + 15 donated_to
	fixtureParties  = 6  // PT, PSDB, MDB, PC DO B, PSOL, PSD
	cMariana22      = "cand:2022:10001600001"
	cJoao22         = "cand:2022:10001600002"
	cCarlos22       = "cand:2022:10001600003"
	cAna22          = "cand:2022:10001600004"
	cHelena22       = "cand:2022:10001600010"
	cMariana24      = "cand:2024:10002200001"
	cPaulo24        = "cand:2024:10002200002"
	cMariana26      = "cand:2026:10002600001"
	marianaNorm     = "MARIANA FICTICIA DOS SANTOS"
	robertoNorm     = "ROBERTO DOADOR FICTICIO"
	luciaNorm       = "LUCIA DOADORA EXEMPLO"
	refFormatRegexp = `^(consulta_cand|receitas_candidatos)_(2022|2024|2026)_[A-Z]{2}\.csv:[1-9][0-9]*$`
)

func TestCandidateLinkedToParty(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	rec := e.record(t, cMariana22)
	where := `rel_type = 'candidate_for' AND src_record_id = $1 AND dst_entity_id = $2 AND kind = 'fact'`
	pt := e.party(t, "PT")
	for key, want := range map[string]string{
		"year": "2022", "office": "DEPUTADO FEDERAL", "office_code": "6", "uf": "AC", "city": "",
		"result": "ELEITO POR QP", "ballot_name": "MARIANA DO POVO", "role": "2022:6:AC:",
	} {
		if got := e.attr(t, key, where, rec, pt); got != want {
			t.Errorf("candidate_for attrs.%s = %q, want %q", key, got, want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE `+where+` AND valid_from = '2022-10-02'
		AND src_entity_id = (SELECT entity_id FROM person_record WHERE id = $1) AND record_ref = 'consulta_cand_2022_AC.csv:2'`, rec, pt); n != 1 {
		t.Error("candidate_for has no election date, entity or record_ref")
	}
	// The party entity: natural key = sigla, name from the file (Latin-1 decoded).
	if n := e.count(t, `SELECT count(*) FROM entity WHERE kind = 'party' AND natural_key = 'MDB'
		AND name = 'MOVIMENTO DEMOCRÁTICO BRASILEIRO' AND name_norm = 'MOVIMENTO DEMOCRATICO BRASILEIRO'`); n != 1 {
		t.Error("MDB party entity missing or misnamed")
	}
	got := e.strings(t, `SELECT natural_key FROM entity WHERE kind = 'party' ORDER BY 1`)
	if joined(got) != "MDB,PC DO B,PSD,PSDB,PSOL,PT" {
		t.Errorf("parties = %v", got)
	}
	// Two rounds: one record, one candidate_for with the final result and the 1st-round date.
	carlos := e.record(t, cCarlos22)
	if r := e.attr(t, "result", `rel_type = 'candidate_for' AND src_record_id = $1`, carlos); r != "ELEITO" {
		t.Errorf("governor result = %q, want ELEITO (2nd round row)", r)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE src_record_id = $1 AND valid_from = '2022-10-02'`, carlos); n != 1 {
		t.Error("governor candidate_for does not start at the 1st round")
	}
	// Municipal candidacy: city code and name, role with the city code.
	m24 := e.record(t, cMariana24)
	if c := e.attr(t, "city", `rel_type = 'candidate_for' AND src_record_id = $1`, m24); c != "01392" {
		t.Errorf("city = %q", c)
	}
	if c := e.attr(t, "city_name", `rel_type = 'candidate_for' AND src_record_id = $1`, m24); c != "RIO BRANCO" {
		t.Errorf("city_name = %q", c)
	}
	if r := e.attr(t, "role", `rel_type = 'candidate_for' AND src_record_id = $1`, m24); r != "2024:13:AC:01392" {
		t.Errorf("role = %q", r)
	}
	// Sentinel result (#NULO) is empty; the sigla "PC do B" is upper-cased.
	if r := e.attr(t, "result", `rel_type = 'candidate_for' AND dst_entity_id = $1`, e.party(t, "PSOL")); r != "" {
		t.Errorf("#NULO result = %q", r)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'candidate_for' AND src_record_id = $1 AND dst_entity_id = $2`,
		e.record(t, cAna22), e.party(t, "PC DO B")); n != 1 {
		t.Error("PC do B candidacy not linked to party PC DO B")
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'candidate_for'`); n != 9 {
		t.Errorf("candidate_for = %d, want 9", n)
	}
}

func TestPersonDonorCreatesRecordAndDonation(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	var name, frag, ref string
	if err := e.pool.QueryRow(ctx, `SELECT name, cpf_fragment, record_ref FROM person_record
		WHERE context_key = 'donor:2022:10001600001'`).Scan(&name, &frag, &ref); err != nil {
		t.Fatal(err)
	}
	// NM_DOADOR_RFB wins over NM_DOADOR; only the CPF fragment is kept.
	if name != "ROBERTO DOADOR FICTÍCIO" || frag != "222333" || ref != "receitas_candidatos_2022_AC.csv:2" {
		t.Errorf("donor record = %q %q %q", name, frag, ref)
	}
	donor, cand := e.record(t, "donor:2022:10001600001"), e.record(t, cMariana22)
	where := `rel_type = 'donated_to' AND src_record_id = $1 AND dst_record_id = $2 AND attrs ->> 'receipt_id' = '50001'`
	for key, want := range map[string]string{
		"year": "2022", "amount": "1500.00", "date": "2022-09-15", "origin": "Recursos de pessoas físicas",
		"origin_code": "10010200", "receipt_id": "50001", "role": "50001", "items": "1",
	} {
		if got := e.attr(t, key, where, donor, cand); got != want {
			t.Errorf("donated_to attrs.%s = %q, want %q", key, got, want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE `+where+` AND valid_from = '2022-09-15' AND kind = 'fact'
		AND record_ref = 'receitas_candidatos_2022_AC.csv:2'
		AND src_entity_id = (SELECT entity_id FROM person_record WHERE id = $1)
		AND dst_entity_id = (SELECT entity_id FROM person_record WHERE id = $2)`, donor, cand); n != 1 {
		t.Error("donated_to has no valid_from, record_ref or entities")
	}
	// The amount keeps its exact decimal digits.
	if a := e.attr(t, "amount", `rel_type = 'donated_to' AND attrs ->> 'receipt_id' = '50002'`); a != "500.50" {
		t.Errorf("amount = %q, want 500.50", a)
	}
	// The national (BR) file is read: the same donor gave to the presidential candidate.
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE r.rel_type = 'donated_to' AND p.context_key = 'donor:2022:10001600010' AND r.dst_record_id = $1
		AND r.record_ref = 'receitas_candidatos_2022_BR.csv:2'`, e.record(t, cHelena22)); n != 1 {
		t.Error("donation from the BR file missing")
	}
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key LIKE 'donor:%'`); n != 9 {
		t.Errorf("donor records = %d, want 9", n)
	}
}

// Receipt 50018 (line 22): JOSÉ CONCEIÇÃO BRANDÃO, a person donor whose CPF
// fragment (123456) and name are those of the Receita partner of the same
// name (testdata/receita), so identity resolution can join the two sources.
func TestPersonDonorMatchingAReceitaPartner(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	got := e.strings(t, `SELECT p.name_norm || '|' || p.cpf_fragment || '|' || (r.attrs ->> 'amount') || '|' || r.record_ref
		FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE r.rel_type = 'donated_to' AND r.attrs ->> 'receipt_id' = '50018' AND r.dst_record_id = $1`, e.record(t, cCarlos22))
	if joined(got) != "JOSE CONCEICAO BRANDAO|123456|800.00|receitas_candidatos_2022_AC.csv:22" {
		t.Errorf("JOSÉ's donation = %v", got)
	}
}

func TestCompanyDonorLinksToCompanyEntity(t *testing.T) {
	e := newEnv(t)
	// The company is already known (as the Receita loads it), with its registry name.
	known := e.id(t, `INSERT INTO entity (kind, natural_key, name, name_norm)
		VALUES ('company', '98990001', 'ALFA PARTICIPAÇÕES S.A.', 'ALFA PARTICIPACOES S A') RETURNING id`)
	_, stats := e.loadFixture(t)
	cand := e.record(t, cCarlos22)
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'donated_to' AND src_entity_id = $1
		AND src_record_id IS NULL AND dst_record_id = $2 AND attrs ->> 'amount' = '10000.00' AND valid_from = '2022-09-01'
		AND attrs ->> 'origin' = 'Recursos de pessoas jurídicas'`, known, cand); n != 1 {
		t.Error("company donation not linked to the known company entity")
	}
	// An unknown donor company is created with the donor name from the file.
	if n := e.count(t, `SELECT count(*) FROM entity e JOIN relationship r ON r.src_entity_id = e.id
		WHERE e.kind = 'company' AND e.natural_key = '98990077' AND e.name = 'EMPRESA DOADORA INVENTADA LTDA'
		AND r.rel_type = 'donated_to' AND r.dst_record_id = $1`, cand); n != 1 {
		t.Error("new donor company entity missing")
	}
	if stats["entities_company_inserted"] != 1 {
		t.Errorf("entities_company_inserted = %d, want 1", stats["entities_company_inserted"])
	}
	if n := e.count(t, `SELECT count(*) FROM entity WHERE kind = 'company'`); n != 2 {
		t.Errorf("company entities = %d, want 2 (no entity for party, candidate or crowdfunding CNPJs)", n)
	}
}

func TestMaskedOrMissingCPFYieldsEmptyFragment(t *testing.T) {
	e := newEnv(t)
	col, _ := e.loadFixture(t)
	e.resolve(t, col.ID)
	// 2024 publishes no candidate CPF ("-4"); 2022 withheld one ("-4"); a donor without CPF.
	for _, key := range []string{cMariana24, cPaulo24, cJoao22, "donor:2022:10001600002"} {
		frags := e.strings(t, `SELECT cpf_fragment FROM person_record WHERE context_key = $1 ORDER BY name_norm`, key)
		if len(frags) == 0 {
			t.Errorf("%s: no record", key)
		}
	}
	for _, key := range []string{cMariana24, cPaulo24, cJoao22} {
		if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key = $1 AND cpf_fragment = ''`, key); n != 1 {
			t.Errorf("%s: record with empty fragment = %d, want 1", key, n)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key = 'donor:2022:10001600002'
		AND name = 'MARCOS SEM DOCUMENTO' AND cpf_fragment = ''`); n != 1 {
		t.Error("donor without CPF: record with empty fragment missing")
	}
	// Nothing joined: the 2024 record of a name seen with a fragment stays alone.
	m24 := e.entityOf(t, cMariana24)
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE entity_id = $1`, m24); n != 1 {
		t.Errorf("2024 record shares its entity with %d records, want 1", n)
	}
}

func TestSameCandidateAcrossYearsMergesOnlyWithFragment(t *testing.T) {
	e := newEnv(t)
	col, _ := e.loadFixture(t)
	if e.entityOf(t, cMariana22) == e.entityOf(t, cMariana26) {
		t.Fatal("records merged before Resolve")
	}
	e.resolve(t, col.ID)
	m22, m26, m24 := e.entityOf(t, cMariana22), e.entityOf(t, cMariana26), e.entityOf(t, cMariana24)
	if m22 != m26 {
		t.Error("2022 and 2026 candidacies (same name, same CPF fragment) were not merged")
	}
	if m24 == m22 {
		t.Error("2024 candidacy without CPF was merged by name alone")
	}
	// The same person as a 2024 donor (donor CPFs are published) joins too.
	if d := e.entityOf(t, "donor:2024:10002200002"); d != m22 {
		t.Error("2024 donor record with the candidate's fragment not merged")
	}
	// Relationships follow the merged entity.
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE r.src_entity_id <> p.entity_id`); n != 0 {
		t.Errorf("%d relationships disagree with their source record's entity", n)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'candidate_for' AND src_entity_id = $1`, m22); n != 2 {
		t.Errorf("candidate_for of the merged entity = %d, want 2 (2022 and 2026)", n)
	}
	// Donors merge across candidates and years only by name + fragment.
	if n := e.count(t, `SELECT count(DISTINCT entity_id) FROM person_record WHERE name_norm = $1`, robertoNorm); n != 1 {
		t.Errorf("Roberto's donor records are in %d entities, want 1", n)
	}
	if n := e.count(t, `SELECT count(DISTINCT entity_id) FROM person_record WHERE name_norm = $1`, luciaNorm); n != 1 {
		t.Errorf("Lúcia's donor records are in %d entities, want 1", n)
	}
}

func TestRepeatedDonationsAreSeparateRows(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	donor, cand := e.record(t, "donor:2022:10001600001"), e.record(t, cMariana22)
	got := e.strings(t, `SELECT (attrs ->> 'receipt_id') || '|' || (attrs ->> 'amount') || '|' || valid_from || '|' || record_ref
		FROM relationship WHERE rel_type = 'donated_to' AND src_record_id = $1 AND dst_record_id = $2 ORDER BY 1`, donor, cand)
	want := []string{
		"50001|1500.00|2022-09-15|receitas_candidatos_2022_AC.csv:2",
		"50002|500.50|2022-09-20|receitas_candidatos_2022_AC.csv:3",
	}
	if !slices.Equal(got, want) {
		t.Errorf("donations = %q, want %q", got, want)
	}
	// One donor gave twice: one record.
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key = 'donor:2022:10001600001'`); n != 1 {
		t.Errorf("donor records = %d, want 1", n)
	}
	// A receipt published as several items (same SQ_RECEITA) is one donation with the exact sum.
	if a := e.attr(t, "amount", `rel_type = 'donated_to' AND attrs ->> 'receipt_id' = '50009'`); a != "350.00" {
		t.Errorf("multi-item receipt amount = %q, want 350.00", a)
	}
	if n := e.attr(t, "items", `rel_type = 'donated_to' AND attrs ->> 'receipt_id' = '50009'`); n != "2" {
		t.Errorf("multi-item receipt items = %s, want 2", n)
	}
	if r := e.strings(t, `SELECT record_ref FROM relationship WHERE attrs ->> 'receipt_id' = '50009'`); joined(r) != "receitas_candidatos_2022_AC.csv:11" {
		t.Errorf("multi-item receipt record_ref = %v (want its first line)", r)
	}
}

func TestPartyAndCandidateDonors(t *testing.T) {
	e := newEnv(t)
	_, stats := e.loadFixture(t)
	mari := e.record(t, cMariana22)
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'donated_to' AND src_entity_id = $1
		AND src_record_id IS NULL AND dst_record_id = $2 AND attrs ->> 'amount' = '200000.00'`, e.party(t, "PT"), mari); n != 1 {
		t.Error("party donation not linked from the party entity")
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'donated_to' AND src_record_id = $1
		AND dst_record_id = $2 AND attrs ->> 'origin' = 'Recursos de outros candidatos'`, e.record(t, cAna22), mari); n != 1 {
		t.Error("donation from another candidate not linked from that candidacy's record")
	}
	want := map[string]int64{
		"own_resources": 1, "crowdfunding": 1, "other_origin": 1, "party_unknown": 1,
		"candidate_unknown": 1, "unidentified_donor": 0, "party_number_mapped": 1,
		// Receipt 50014 names a donor candidacy absent from the file; no receipt
		// lacks the donor SQ and none is a self-donation.
		"donor_candidate_not_in_file": 1, "donor_candidate_unpublished": 0, "donor_candidate_self": 0,
		"receipt_mixed_donor": 1, "receipt_conflict": 0,
	}
	for k, v := range want {
		if stats[k] != v {
			t.Errorf("stats %s = %d, want %d", k, stats[k], v)
		}
	}
	// Own resources never become a self-loop; crowdfunding platforms are not donors.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE attrs ->> 'receipt_id' IN ('50006', '50008', '50011', '50013', '50014')`); n != 0 {
		t.Errorf("%d donations loaded that must be skipped", n)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'donated_to'`); n != 15 {
		t.Errorf("donated_to = %d, want 15", n)
	}
	// A party donor without sigla but with NR_PARTIDO_DOADOR 45: PSDB, the only sigla with 45 in 2022.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE rel_type = 'donated_to' AND src_entity_id = $1
		AND dst_record_id = $2 AND attrs ->> 'receipt_id' = '50017' AND attrs ->> 'amount' = '5000.00'`,
		e.party(t, "PSDB"), e.record(t, cJoao22)); n != 1 {
		t.Error("party donor mapped by number not linked from PSDB")
	}
}

func TestAmbiguousPartyNumberStaysUnknown(t *testing.T) {
	e := newEnv(t)
	// Ana's party (PC do B) published with number 45 too: 45 is no longer unambiguous in 2022.
	zips := fixtureZips(t, func(entry string, b []byte) []byte {
		if entry == "consulta_cand_2022_AC.csv" {
			return bytes.Replace(b, []byte(`"65";"PC do B"`), []byte(`"45";"PC do B"`), 1)
		}
		return b
	})
	_, stats := e.load(t, time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC), zips)
	if stats["party_unknown"] != 2 || stats["party_number_mapped"] != 0 {
		t.Errorf("party_unknown = %d (want 2), party_number_mapped = %d (want 0)", stats["party_unknown"], stats["party_number_mapped"])
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE attrs ->> 'receipt_id' = '50017'`); n != 0 {
		t.Error("receipt with an ambiguous party number loaded")
	}
}

func TestReceiptWithMixedDonorsTakesTheFirstLine(t *testing.T) {
	e := newEnv(t)
	_, stats := e.loadFixture(t)
	if stats["receipt_mixed_donor"] != 1 {
		t.Errorf("receipt_mixed_donor = %d, want 1", stats["receipt_mixed_donor"])
	}
	got := e.strings(t, `SELECT p.context_key || '|' || p.name_norm || '|' || (r.attrs ->> 'amount') || '|' || (r.attrs ->> 'items') || '|' || r.record_ref
		FROM relationship r JOIN person_record p ON p.id = r.src_record_id WHERE r.attrs ->> 'receipt_id' = '50016'`)
	if joined(got) != "donor:2022:10001600003|"+robertoNorm+"|150.00|2|receitas_candidatos_2022_AC.csv:20" {
		t.Errorf("mixed receipt = %v", got)
	}
	// Line 21's donor (Lúcia) gets no record for this candidacy; Carlos's
	// other donor record is JOSÉ's (receipt 50018, line 22).
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key = 'donor:2022:10001600003' AND name_norm = $1`, luciaNorm); n != 0 {
		t.Errorf("records of the mixed receipt's second donor = %d, want 0", n)
	}
}

func TestMalformedRowsAreSkippedAndCounted(t *testing.T) {
	e := newEnv(t)
	_, stats := e.loadFixture(t)
	if stats["rows_read"] != fixtureRows || stats["rows_skipped"] != fixtureSkipped {
		t.Errorf("rows_read = %d (want %d), rows_skipped = %d (want %d)", stats["rows_read"], fixtureRows, stats["rows_skipped"], fixtureSkipped)
	}
	if stats["candidatos_rows"] != 12 || stats["candidatos_skipped"] != 2 || stats["receitas_rows"] != 25 || stats["receitas_skipped"] != 2 {
		t.Errorf("per-kind stats: %v", stats)
	}
	// The malformed rows were not loaded (column shift, stray quote, bad amount, extra column)...
	for _, sq := range []string{"cand:2022:10001600005", "cand:2022:10001600007"} {
		if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key = $1`, sq); n != 0 {
			t.Errorf("malformed candidacy %s loaded", sq)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE attrs ->> 'receipt_id' IN ('50010', '50015')`); n != 0 {
		t.Error("malformed receipt loaded")
	}
	// ...and their neighbours were.
	for _, key := range []string{cAna22, "cand:2022:10001600006"} {
		if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key = $1`, key); n != 1 {
			t.Errorf("neighbour %s of a malformed row not loaded", key)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE attrs ->> 'receipt_id' IN ('50012', '50013', '50014')`); n != 1 {
		t.Error("neighbours of the malformed receipts not handled")
	}
	// The _BRASIL aggregate and the other entries are never read: nothing twice.
	if n := e.count(t, `SELECT count(*) FROM person_record`); n != fixtureRecords {
		t.Errorf("person records = %d, want %d", n, fixtureRecords)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship`); n != fixtureRels {
		t.Errorf("relationships = %d, want %d", n, fixtureRels)
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

func TestLoadTwiceIsIdempotent(t *testing.T) {
	e := newEnv(t)
	col1, first := e.loadFixture(t)
	before := tableCounts(t, e)
	if before["relationship"] != fixtureRels || before["person_record"] != fixtureRecords {
		t.Fatalf("first load counts %v", before)
	}
	if first["relationships_inserted"] != fixtureRels || first["person_records_inserted"] != fixtureRecords ||
		first["entities_party_inserted"] != fixtureParties {
		t.Errorf("first load stats: %v", first)
	}
	e.resolve(t, col1.ID)
	resolved := claimDump(t, e)
	// A republished edition (new collection, other reference date) with the same rows.
	col2, stats := e.load(t, refB, fixtureZips(t, nil))
	compareDumps(t, "second load", resolved, claimDump(t, e))
	after := tableCounts(t, e)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %d rows after the first load, %d after the second", k, v, after[k])
		}
	}
	if stats["relationships_inserted"] != 0 || stats["relationships_seen"] != fixtureRels || stats["person_records_inserted"] != 0 ||
		stats["entities_party_inserted"] != 0 || stats["entities_company_inserted"] != 0 {
		t.Errorf("second load stats: %v", stats)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE last_seen_collection_id <> $1 OR first_collection_id <> $2`, col2.ID, col1.ID); n != 0 {
		t.Errorf("%d relationships not first seen by the first and last seen by the second collection", n)
	}
	// Re-seen claims follow the merged entities; nothing is ever closed for TSE.
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id = r.src_record_id WHERE r.src_entity_id <> p.entity_id`); n != 0 {
		t.Errorf("%d relationships disagree with their record's entity", n)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NOT NULL`); n != 0 {
		t.Errorf("%d relationships closed", n)
	}
	// Loading the same collection's files again (a retried collection) changes nothing either.
	e.load(t, refB, fixtureZips(t, nil))
	compareDumps(t, "third load", resolved, claimDump(t, e))
}

// claimDump is every row of entity, person_record and relationship as JSON
// (relationships without last_seen_collection_id), ordered by id.
func claimDump(t *testing.T, e env) map[string][]string {
	t.Helper()
	return map[string][]string{
		"entity":        e.strings(t, `SELECT to_jsonb(x)::text FROM entity x ORDER BY id`),
		"person_record": e.strings(t, `SELECT to_jsonb(x)::text FROM person_record x ORDER BY id`),
		"relationship":  e.strings(t, `SELECT (to_jsonb(x) - 'last_seen_collection_id')::text FROM relationship x ORDER BY id`),
	}
}

func compareDumps(t *testing.T, what string, want, got map[string][]string) {
	t.Helper()
	for tbl, rows := range want {
		if !slices.Equal(rows, got[tbl]) {
			t.Errorf("%s changed %s: %d rows before, %d after", what, tbl, len(rows), len(got[tbl]))
			for i := range min(len(rows), len(got[tbl])) {
				if rows[i] != got[tbl][i] {
					t.Errorf("first difference:\n%s\n%s", rows[i], got[tbl][i])
					break
				}
			}
		}
	}
}

func TestEveryClaimHasProvenance(t *testing.T) {
	e := newEnv(t)
	col, _ := e.loadFixture(t)
	refFormat := regexp.MustCompile(refFormatRegexp)
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
			AND f.sha256 ~ '^[0-9a-f]{64}$' AND f.raw_key LIKE 'raw/tse/2026-11-10/%.zip')`); n != 0 {
			t.Errorf("%d %s rows without a recorded raw file", n, tbl)
		}
	}
	files := e.strings(t, `SELECT name FROM collection_file WHERE collection_id = $1 ORDER BY name`, col.ID)
	want := []string{
		"consulta_cand_2022_AC.csv", "consulta_cand_2022_BR.csv", "consulta_cand_2024_AC.csv", "consulta_cand_2026_AC.csv",
		"receitas_candidatos_2022_AC.csv", "receitas_candidatos_2022_BR.csv", "receitas_candidatos_2024_AC.csv", "receitas_candidatos_2026_AC.csv",
	}
	if !slices.Equal(files, want) {
		t.Errorf("recorded files %v, want %v", files, want)
	}
	// The line points at the row: the 2nd-round row of the governor (line 5) carries the final result.
	if r := e.strings(t, `SELECT record_ref FROM relationship WHERE rel_type = 'candidate_for' AND src_record_id = $1`, e.record(t, cCarlos22)); joined(r) != "consulta_cand_2022_AC.csv:5" {
		t.Errorf("governor candidate_for record_ref = %v", r)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE kind <> 'fact' OR confidence IS NOT NULL`); n != 0 {
		t.Errorf("%d relationships are not plain facts", n)
	}
}

// fixtureSecrets are the full CPFs, voter-registration numbers, e-mail and
// birth dates of the fixture: none may be stored or logged anywhere.
var fixtureSecrets = []string{"98712345601", "45678901234", "32165498700", "11199988877", "74185296300", "11122233344", "32112345600",
	"55566677788", "900000000101", "900000000303", "900000000404", "900000000606", "900000001010",
	"mariana@exemplo.invalid", "01/02/1971", "1971-02-01", "03/04/1965", "1965-04-03"}

// querier is a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// schemaText is the text of every row of every table of the test schema
// (claims, collections, collection_file, staging...), and the row count of
// each table, read through q (inside the loader's transaction for staging
// rows not committed yet).
func schemaText(t *testing.T, q querier) (string, map[string]int) {
	t.Helper()
	read := func(sql string) []string {
		rows, err := q.Query(ctx, sql)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var b strings.Builder
	counts := map[string]int{}
	for _, tbl := range read(`SELECT table_name::text FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY 1`) {
		rows := read(`SELECT t::text FROM ` + pgx.Identifier{tbl}.Sanitize() + ` t`)
		counts[tbl] = len(rows)
		for _, row := range rows {
			b.WriteString(row)
			b.WriteByte('\n')
		}
	}
	return b.String(), counts
}

// lockedBuffer is a bytes.Buffer safe for the loader's concurrent logging.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestNoFullCPFOrPersonalFieldIsStored(t *testing.T) {
	e := newEnv(t)
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	// Staging is scanned at every hook point, inside the loader's
	// transactions, while each staging table is populated.
	var scans []string
	populated := map[string]bool{}
	stagingHook = func(ctx context.Context, tx pgx.Tx, point string) {
		text, counts := schemaText(t, tx)
		scans = append(scans, text)
		for tbl, n := range counts {
			if n > 0 {
				populated[tbl] = true
			}
		}
	}
	t.Cleanup(func() { stagingHook = nil })

	e.loadFixture(t)
	for _, tbl := range stagingTables {
		if !populated[tbl] {
			t.Errorf("%s was never scanned while populated", tbl)
		}
	}
	if !strings.Contains(logs.String(), "tse: load done") {
		t.Fatal("logs were not captured")
	}
	final, _ := schemaText(t, e.pool)
	texts := map[string]string{"final scan": final, "logs": logs.String()}
	for i, s := range scans {
		texts["staging scan "+strconv.Itoa(i)] = s
	}
	for name, text := range texts {
		for _, s := range fixtureSecrets {
			if strings.Contains(text, s) {
				t.Errorf("%s: %q found", name, s)
			}
		}
	}
	for _, tbl := range stagingTables {
		if n := e.count(t, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("%s holds %d rows after the load", tbl, n)
		}
	}
}

func TestMissingRequiredColumnFailsTheFile(t *testing.T) {
	e := newEnv(t)
	zips := fixtureZips(t, func(entry string, b []byte) []byte {
		if entry == "receitas_candidatos_2024_AC.csv" {
			return bytes.Replace(b, []byte(`"VR_RECEITA"`), []byte(`"VR_RECEITA_X"`), 1)
		}
		return b
	})
	// Edited zips are stored under their own reference dates (raw store keys are write-once).
	_, _, err := e.loadErr(t, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), zips, collection.Scope{Full: true})
	if err == nil || !strings.Contains(err.Error(), "VR_RECEITA") || !strings.Contains(err.Error(), "receitas_candidatos_2024_AC.csv") {
		t.Fatalf("err = %v, want a missing VR_RECEITA column in receitas_candidatos_2024_AC.csv", err)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship`); n != 0 {
		t.Errorf("%d relationships written by a failed load", n)
	}
	for _, tbl := range stagingTables {
		if n := e.count(t, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("%s holds %d rows after a failed load", tbl, n)
		}
	}
	// Extra columns are ignored.
	e2 := newEnv(t)
	zips = fixtureZips(t, func(entry string, b []byte) []byte {
		return bytes.ReplaceAll(b, []byte("\r\n"), []byte(";\"EXTRA\"\r\n"))
	})
	if _, stats := e2.load(t, time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC), zips); stats["rows_read"] != fixtureRows {
		t.Errorf("with an extra column: rows_read = %d", stats["rows_read"])
	}
}

func TestOnlyFullScopeIsAccepted(t *testing.T) {
	e := newEnv(t)
	_, _, err := e.loadErr(t, refA, fixtureZips(t, nil), collection.Scope{CNPJs: []string{"98990001"}, Depth: 1})
	if !errors.Is(err, ErrScope) {
		t.Fatalf("err = %v, want ErrScope", err)
	}
}

func TestYearWithoutBothFilesIsRefused(t *testing.T) {
	e := newEnv(t)
	zips := fixtureZips(t, nil)
	delete(zips, "prestacao_de_contas_eleitorais_candidatos_2024.zip")
	_, _, err := e.loadErr(t, refA, zips, collection.Scope{Full: true})
	if err == nil || !strings.Contains(err.Error(), "2024") {
		t.Fatalf("err = %v, want a missing 2024 receipts file", err)
	}
}

func TestOversizedEntryIsRefused(t *testing.T) {
	e := newEnv(t)
	e.c.maxEntryBytes = 2000 // the receitas AC entries are larger
	_, _, err := e.loadErr(t, refA, fixtureZips(t, nil), collection.Scope{Full: true})
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want errTooLarge", err)
	}
	e2 := newEnv(t)
	e2.c.maxTotalBytes = 10000 // the loaded entries of the 2022 receipts zip together are larger
	_, _, err = e2.loadErr(t, refA, fixtureZips(t, nil), collection.Scope{Full: true})
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want errTooLarge for the total", err)
	}
}

func TestRepublishedCandidacyKeepsItsRecord(t *testing.T) {
	e := newEnv(t)
	col1, _ := e.loadFixture(t)
	before := claimDump(t, e)
	// The next publication gives the 2024 candidate a CPF (it had "-4") and
	// renames a 2022 candidate. Latin-1 bytes.
	zips := fixtureZips(t, func(entry string, b []byte) []byte {
		switch entry {
		case "consulta_cand_2024_AC.csv":
			return bytes.Replace(b, []byte(`"MARIANA DO POVO";"#NULO";"-4"`), []byte(`"MARIANA DO POVO";"#NULO";"98712345601"`), 1)
		case "consulta_cand_2022_AC.csv":
			return bytes.Replace(b, []byte("\"JO\xc3O PEDRO INVENTADO\""), []byte("\"JO\xc3O PEDRO INVENTADO FILHO\""), 1)
		}
		return b
	})
	col2, stats := e.load(t, time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC), zips)
	if stats["person_records_inserted"] != 0 || stats["relationships_inserted"] != 0 || stats["relationships_seen"] != fixtureRels {
		t.Errorf("second load stats: %v", stats)
	}
	// One record each, as first published (name and fragment kept).
	for key, want := range map[string]string{cMariana24: marianaNorm + "|", cJoao22: "JOAO PEDRO INVENTADO|"} {
		if got := e.strings(t, `SELECT name_norm || '|' || cpf_fragment FROM person_record WHERE context_key = $1`, key); joined(got) != want {
			t.Errorf("%s records = %v, want %s", key, got, want)
		}
		if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id = r.src_record_id
			WHERE p.context_key = $1 AND r.rel_type = 'candidate_for' AND r.last_seen_collection_id = $2`, key, col2.ID); n != 1 {
			t.Errorf("%s: candidate_for re-seen = %d, want 1", key, n)
		}
	}
	// The donation to the republished candidacy is not duplicated.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE attrs ->> 'receipt_id' = '60001'`); n != 1 {
		t.Errorf("donations of receipt 60001 = %d, want 1", n)
	}
	compareDumps(t, "republished candidacies", before, claimDump(t, e))
	if col1.ID == col2.ID {
		t.Fatal("same collection")
	}
}

func TestConflictingReceiptIsNotDuplicated(t *testing.T) {
	e := newEnv(t)
	col1, _ := e.loadFixture(t)
	mari := e.record(t, cMariana22)
	total := func() string {
		return joined(e.strings(t, `SELECT sum((attrs ->> 'amount')::numeric)::text FROM relationship
			WHERE rel_type = 'donated_to' AND closed_collection_id IS NULL AND dst_record_id = $1`, mari))
	}
	sum1 := total()
	// The next publication gives receipt 50001 (line 2) another donor name.
	zips := fixtureZips(t, func(entry string, b []byte) []byte {
		if entry == "receitas_candidatos_2022_AC.csv" {
			return bytes.Replace(b, []byte("\"ROBERTO DOADOR FICTICIO\";\"ROBERTO DOADOR FICT\xcdCIO\""),
				[]byte("\"ROBERTO DOADOR FICTICIO\";\"ROBERTO DOADOR DIFERENTE\""), 1)
		}
		return b
	})
	_, stats := e.load(t, time.Date(2030, 4, 1, 0, 0, 0, 0, time.UTC), zips)
	if stats["receipt_conflict"] != 1 {
		t.Errorf("receipt_conflict = %d, want 1", stats["receipt_conflict"])
	}
	got := e.strings(t, `SELECT p.name || '|' || r.last_seen_collection_id FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE r.rel_type = 'donated_to' AND r.closed_collection_id IS NULL AND r.dst_record_id = $1 AND r.attrs ->> 'role' = '50001'`, mari)
	if want := "ROBERTO DOADOR FICTÍCIO|" + strconv.FormatInt(col1.ID, 10); joined(got) != want {
		t.Errorf("open rows of receipt 50001 = %v, want [%s] (untouched)", got, want)
	}
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE name_norm = 'ROBERTO DOADOR DIFERENTE'`); n != 0 {
		t.Error("a donor record was created for the conflicting receipt")
	}
	if sum2 := total(); sum2 != sum1 {
		t.Errorf("candidate total %s -> %s", sum1, sum2)
	}
	if stats["person_records_inserted"] != 0 || stats["relationships_inserted"] != 0 {
		t.Errorf("second load stats: %v", stats)
	}
}
