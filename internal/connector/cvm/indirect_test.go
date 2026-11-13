package cvm

import (
	"bytes"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"geoint/internal/collection"
)

// rel returns "<percent or -> <described_by>" of the single open
// relationship of that type from src (entity id, or person record id when
// record is true) into the company with base dst.
func (e env) rel(t *testing.T, typ string, src int64, record bool, dst string) string {
	t.Helper()
	cond := `r.src_record_id IS NULL AND r.src_entity_id = $2`
	if record {
		cond = `r.src_record_id = $2`
	}
	got := e.strings(t, `SELECT coalesce(r.attrs ->> 'percent', '-') || ' ' || coalesce(r.attrs ->> 'described_by', '-')
		FROM relationship r JOIN entity d ON d.id = r.dst_entity_id
		WHERE r.rel_type = $1 AND `+cond+` AND d.kind = 'company' AND d.natural_key = $3 AND r.closed_collection_id IS NULL`, typ, src, dst)
	if len(got) != 1 {
		t.Fatalf("%d open %s from %d into %s, want 1", len(got), typ, src, dst)
	}
	return got[0]
}

func TestIndirectHoldersAreLoaded(t *testing.T) {
	e := newEnv(t)
	e.loadFixture(t)
	cases := []struct {
		name, typ string
		src       int64
		record    bool
		dst, want string
	}{
		// Alfa's form describes Alfa Holding (group sums to 100).
		{"Alfa Controle in Alfa Holding", "shareholder_of", e.company(t, baseControl), false, baseHolding, "70 98990001"},
		{"Alfa Controle controls Alfa Holding", "controls", e.company(t, baseControl), false, baseHolding, "- 98990001"},
		{"Eduardo in Alfa Holding", "shareholder_of", e.record(t, baseHolding, "EDUARDO INVENTADO INDIRETO"), true, baseHolding, "30 98990001"},
		// A nested level: the holders of Alfa Controle.
		{"Fernanda in Alfa Controle", "shareholder_of", e.record(t, baseControl, "FERNANDA FICTÍCIA NETA"), true, baseControl, "100 98990001"},
		{"Fernanda controls Alfa Controle", "controls", e.record(t, baseControl, "FERNANDA FICTÍCIA NETA"), true, baseControl, "- 98990001"},
		// Beta's form describes Delta with an all-zero group: no percent at all.
		{"Helio in Delta", "shareholder_of", e.record(t, baseDelta, "HELIO ZERO FICTÍCIO"), true, baseDelta, "- 98991001"},
		{"Helio controls Delta", "controls", e.record(t, baseDelta, "HELIO ZERO FICTÍCIO"), true, baseDelta, "- 98991001"},
		{"Global Zero in Delta", "shareholder_of", e.foreign(t, keyGlobalZero), false, baseDelta, "- 98991001"},
		// Omega is described by Beta (2026) and Gama (2025): Beta's newer form wins.
		{"Omega Fundador in Omega", "shareholder_of", e.company(t, baseFounder), false, baseOmega, "90 98991001"},
		// Gama is listed: its own form, not Alfa's description (Omega 99 %).
		{"Omega in Gama", "shareholder_of", e.company(t, baseOmega), false, baseGama, "75 -"},
	}
	for _, c := range cases {
		if got := e.rel(t, c.typ, c.src, c.record, c.dst); got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE attrs ->> 'percent' IN ('80', '99')`); n != 0 {
		t.Error("a superseded description was loaded")
	}
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN entity d ON d.id = r.dst_entity_id WHERE d.natural_key = $1`, baseGama); n != 2 {
		t.Errorf("claims into Gama = %d, want 2 (its own direct lines only)", n)
	}
	// The all-zero group stores no percent of any kind.
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN entity d ON d.id = r.dst_entity_id
		WHERE d.natural_key = $1 AND (r.attrs ? 'percent' OR r.attrs ? 'percent_on' OR r.attrs ? 'percent_pn')`, baseDelta); n != 0 {
		t.Errorf("%d claims into Delta with a percent", n)
	}
	// Holders under an individual (Carla) are not loaded.
	if n := e.count(t, `SELECT count(*) FROM entity WHERE natural_key = '98991110'`); n != 0 {
		t.Error("a holder under an individual parent was loaded")
	}
}

// reverseLines returns a CSV with its data lines in reverse order.
func reverseLines(b []byte) []byte {
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\r\n")), []byte("\r\n"))
	data := lines[1:]
	slices.Reverse(data)
	return append(bytes.Join(append([][]byte{lines[0]}, data...), []byte("\r\n")), "\r\n"...)
}

// withoutRefs drops the record_ref part of claimDump lines (line numbers
// change when the file order changes).
func withoutRefs(dump []string) []string {
	out := make([]string, len(dump))
	for i, l := range dump {
		parts := strings.Split(l, "|")
		for j, p := range parts {
			if strings.HasPrefix(p, "fre_cia_aberta_posicao_acionaria_") {
				parts[j] = "<ref>"
			}
		}
		out[i] = strings.Join(parts, "|")
	}
	return out
}

func TestIndirectChoiceDoesNotDependOnFileOrder(t *testing.T) {
	reversed := func(_ int, b []byte) []byte { return reverseLines(b) }

	e := newEnv(t)
	e.loadAndClose(t, refA, fixtureZips(t, nil))
	dump := claimDump(t, e)
	_, stats, closed := e.loadAndClose(t, refB, fixtureZips(t, reversed))
	if closed != 0 || stats["relationships_replaced"] != 0 || stats["relationships_inserted"] != 0 || stats["relationships_seen"] != fixtureRels {
		t.Errorf("reversed reload: closed %d, stats %v", closed, stats)
	}
	if got := claimDump(t, e); !slices.Equal(got, dump) {
		t.Errorf("claims changed on the reversed reload:\n%v\nwant\n%v", got, dump)
	}

	// A fresh database loading the reversed files first gets the same claims.
	f := newEnv(t)
	f.loadAndClose(t, refA, fixtureZips(t, reversed))
	if got, want := withoutRefs(claimDump(t, f)), withoutRefs(dump); !slices.Equal(got, want) {
		t.Errorf("reversed first load:\n%v\nwant\n%v", got, want)
	}
}

func TestChangedPercentOfAPersonRecordClaim(t *testing.T) {
	e := newEnv(t)
	e.loadAndClose(t, refA, fixtureZips(t, nil))
	ana := e.record(t, baseAlfa, "ANA FICTÍCIA CONTROLADORA")
	edit := func(year int, b []byte) []byte {
		if year != 2026 {
			return b
		}
		return replaceLine(b, ";7000002;ANA", ";1000000;10.000000;0;0.000000;1000000;10.000000;", ";1200000;12.000000;0;0.000000;1200000;12.000000;")
	}
	col2, stats, closed := e.loadAndClose(t, refB, fixtureZips(t, edit))
	if closed != 0 || stats["relationships_replaced"] != 1 || stats["relationships_inserted"] != 1 {
		t.Errorf("closed %d, stats %v", closed, stats)
	}
	got := e.strings(t, `SELECT (attrs ->> 'percent') || ' ' || coalesce(closed_collection_id::text, 'open') FROM relationship
		WHERE rel_type = 'shareholder_of' AND src_record_id = $1 ORDER BY id`, ana)
	if want := "10 " + itoa(col2.ID) + ",12 open"; joined(got) != want {
		t.Errorf("Ana's shareholder_of rows = %v, want %s", got, want)
	}
	// Still cited through the record, and her controls claim was only seen again.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE src_record_id = $1 AND closed_collection_id IS NULL`, ana); n != 2 {
		t.Errorf("open claims from Ana's record = %d, want 2", n)
	}
}

func TestDroppedShareholderIsClosedByCloseUnseen(t *testing.T) {
	e := newEnv(t)
	e.loadAndClose(t, refA, fixtureZips(t, nil))
	bruno := e.record(t, baseBeta, "BRUNO EXEMPLO TESTE")
	edit := func(year int, b []byte) []byte {
		if year != 2026 {
			return b
		}
		return replaceLine(b, ";7000112;BRUNO", "98.991.001/0001-05;", "XX;") // Bruno's line becomes malformed
	}
	col2, stats, closed := e.loadAndClose(t, refB, fixtureZips(t, edit))
	if closed != 1 {
		t.Errorf("CloseUnseen closed %d, want 1 (Bruno's shareholder_of)", closed)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE src_record_id = $1 AND rel_type = 'shareholder_of' AND closed_collection_id = $2`, bruno, col2.ID); n != 1 {
		t.Error("Bruno's shareholder_of was not closed")
	}
	if stats["malformed_cnpj"] != 3 {
		t.Errorf("malformed_cnpj = %d, want 3", stats["malformed_cnpj"])
	}
}

func TestMissingReferenceYearLoadsThePreviousYear(t *testing.T) {
	e := newEnv(t)
	zips := fixtureZips(t, nil)
	delete(zips, "fre_cia_aberta_2026.zip")
	_, stats := e.load(t, refA, zips)
	if stats["year_not_published"] != 2026 || stats["companies_from_previous_year"] != 2 || stats["forms_loaded"] != 2 {
		t.Errorf("stats %v", stats)
	}
	if got := e.openAttr(t, "shareholder_of", e.company(t, baseHolding), baseAlfa, "percent"); got != "50.1" {
		t.Errorf("Alfa Holding in Alfa = %s, want the 2025 form's 50.1", got)
	}
}

func TestMissingReferenceYearAfterItWasLoadedFails(t *testing.T) {
	e := newEnv(t)
	e.loadAndClose(t, refA, fixtureZips(t, nil))
	dump := claimDump(t, e)
	zips := fixtureZips(t, nil)
	delete(zips, "fre_cia_aberta_2026.zip")
	_, _, err := e.loadErr(t, refB, zips, collection.Scope{Full: true})
	if err == nil || !strings.Contains(err.Error(), "earlier collection loaded it") {
		t.Fatalf("err = %v", err)
	}
	if got := claimDump(t, e); !slices.Equal(got, dump) {
		t.Error("claims changed by the refused load")
	}
}

func TestMissingPreviousYearFails(t *testing.T) {
	e := newEnv(t)
	zips := fixtureZips(t, nil)
	delete(zips, "fre_cia_aberta_2025.zip")
	_, _, err := e.loadErr(t, refA, zips, collection.Scope{Full: true})
	if err == nil || !strings.Contains(err.Error(), "no FRE file for 2025") {
		t.Errorf("err = %v", err)
	}
}

func TestHeaderWithByteOrderMarkIsRead(t *testing.T) {
	b := fixtureFile(t, entryName(2025))
	plain, _, err := parseEntry(bytes.NewReader(b), entryName(2025), 2025)
	if err != nil {
		t.Fatal(err)
	}
	bom, _, err := parseEntry(bytes.NewReader(append([]byte{0xEF, 0xBB, 0xBF}, b...)), entryName(2025), 2025)
	if err != nil {
		t.Fatalf("with BOM: %v", err)
	}
	if len(bom) != len(plain) || bom[0].Base != plain[0].Base {
		t.Errorf("with BOM: %d rows, want %d", len(bom), len(plain))
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestHoldingUnderTwoParentLinesOfOneForm(t *testing.T) {
	form := formKey{RefDate: "2026-12-31", Version: 1, DocID: 1, Year: 2026}
	hold := func(id, parent int64, base, pct string) holderRow {
		return holderRow{Base: "98999001", Form: form, ID: id, ParentID: parent, Kind: holderCompany, HolderBase: base,
			Name: base, NameNorm: base, Percent: pct, PercentOK: true}
	}
	rows := []holderRow{
		hold(10, 0, "98999100", "60"),   // holding X, direct
		hold(11, 10, "98999200", "100"), // Y holds X
		hold(12, 11, "98999100", "1"),   // X again, under Y (lowest parent id: 10)
		hold(20, 12, "98999300", "100"), // X described a second time: differs
		hold(13, 10, "98999400", "0"),   // second holder of X under parent 10
	}
	stats := map[string]int64{}
	descs := describeHoldings(map[string][]holderRow{"98999001": rows}, stats)
	d, ok := descs["98999100"]
	if !ok || len(d.rows) != 2 || d.rows[0].ID != 11 {
		t.Fatalf("X described by %+v", d)
	}
	if stats["indirect_duplicate_description"] != 1 || stats["indirect_duplicate_conflict"] != 1 {
		t.Errorf("stats %v", stats)
	}
}

// Within one form, a complete group beats an all-zero ("not informed") one
// describing the same holding, whatever their parent ids.
func TestCompleteGroupBeatsAnAllZeroOneInOneForm(t *testing.T) {
	form := formKey{RefDate: "2026-12-31", Version: 1, DocID: 1, Year: 2026}
	hold := func(id, parent int64, base, pct string) holderRow {
		return holderRow{Base: "98999001", Form: form, ID: id, ParentID: parent, Kind: holderCompany, HolderBase: base,
			Name: base, NameNorm: base, Percent: pct, PercentOK: true, Line: id}
	}
	rows := []holderRow{
		hold(10, 0, "98999100", "60"), // holding X, direct
		hold(11, 10, "98999200", "0"), // X described with zeros under parent 10
		hold(12, 10, "98999300", "0"),
		hold(13, 0, "98999500", "40"),  // a second direct line ...
		hold(14, 13, "98999500", "10"), // ... under which X appears again
		hold(20, 14, "98999200", "70"), // X described completely under parent 14
		hold(21, 14, "98999300", "30"),
	}
	rows[3].HolderBase, rows[4].HolderBase = "98999600", "98999100"
	stats := map[string]int64{}
	descs := describeHoldings(map[string][]holderRow{"98999001": rows}, stats)
	d, ok := descs["98999100"]
	if !ok || len(d.rows) != 2 || d.rows[0].ID != 20 {
		t.Fatalf("X described by %+v, want the complete group under parent 14", d)
	}
	if stats["indirect_duplicate_description"] != 2 || stats["indirect_duplicate_conflict"] != 1 {
		t.Errorf("stats %v", stats)
	}
}

// A duplicate ID_Acionista inside one form: the lowest line wins, whatever
// the order the rows arrive in, and the later ones are counted and skipped.
func TestDuplicateShareholderIDKeepsTheLowestLine(t *testing.T) {
	form := formKey{RefDate: "2026-12-31", Version: 1, DocID: 1, Year: 2026}
	a := holderRow{Base: "98999001", Form: form, ID: 7, Kind: holderCompany, HolderBase: "98999100", Percent: "60", PercentOK: true, Line: 3}
	b := a
	b.HolderBase, b.Percent, b.Line = "98999200", "40", 9
	for _, order := range [][]holderRow{{a, b}, {b, a}} {
		stats := map[string]int64{}
		got := dropDuplicateIDs(order, stats)
		if len(got) != 1 || got[0].Line != 3 || got[0].HolderBase != "98999100" {
			t.Errorf("kept %+v, want line 3", got)
		}
		if stats["duplicate_shareholder_id"] != 1 || stats["rows_skipped"] != 1 {
			t.Errorf("stats %v", stats)
		}
	}
}

// The ±0.01 tolerance of an indirect group's sum, at its boundary.
func TestGroupCompleteBoundary(t *testing.T) {
	group := func(pcts ...string) []*holderRow {
		var out []*holderRow
		for _, p := range pcts {
			out = append(out, &holderRow{Percent: p, PercentOK: true})
		}
		return out
	}
	for pcts, want := range map[[2]string]bool{
		{"60", "39.99"}: true, {"60", "40.01"}: true,
		{"60", "39.98"}: false, {"60", "40.02"}: false,
		{"0", "0"}: false,
	} {
		if got := groupComplete(group(pcts[0], pcts[1])); got != want {
			t.Errorf("%v: complete = %v, want %v", pcts, got, want)
		}
	}
}

// A FAILED earlier collection that recorded the reference year's file does
// not make that year required: only a done one does.
func TestFailedEarlierCollectionDoesNotBlockAMissingReferenceYear(t *testing.T) {
	e := newEnv(t)
	failed, err := collection.Begin(ctx, e.pool, sourceID, refA, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.RecordSourceFile(ctx, e.pool, failed.ID, collection.File{Name: entryName(2026), RawKey: rawPrefix(refA) + remoteName(2026),
		SHA256: strings.Repeat("a", 64), Size: 1, SourceURL: "https://example.org/" + remoteName(2026)}); err != nil {
		t.Fatal(err)
	}
	if err := collection.Fail(ctx, e.pool, failed.ID, errors.New("broke")); err != nil {
		t.Fatal(err)
	}
	zips := fixtureZips(t, nil)
	delete(zips, "fre_cia_aberta_2026.zip")
	_, stats, err := e.loadErr(t, refB, zips, collection.Scope{Full: true})
	if err != nil {
		t.Fatalf("a failed earlier collection blocked the load: %v", err)
	}
	if stats["year_not_published"] != 2026 {
		t.Errorf("stats %v", stats)
	}
}
