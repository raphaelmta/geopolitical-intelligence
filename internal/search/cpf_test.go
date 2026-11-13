package search_test

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"geoint/internal/claims"
	"geoint/internal/search"
)

// Every CPF here is synthetic: 123.456.789-09 and 111.222.333-96 have valid
// check digits; the others are computed by withDV from invented digits.

// withDV appends the two check digits to 9 digits (modulo 11), computed
// independently of the package.
func withDV(nine string) string {
	d := []int{}
	for _, c := range nine {
		d = append(d, int(c-'0'))
	}
	for n := 9; n <= 10; n++ {
		sum := 0
		for i := 0; i < n; i++ {
			sum += d[i] * (n + 1 - i)
		}
		r := 11 - sum%11
		if r >= 10 {
			r = 0
		}
		d = append(d, r)
	}
	var b strings.Builder
	for _, x := range d {
		b.WriteByte(byte('0' + x))
	}
	return b.String()
}

// punct writes 11 digits as ddd.ddd.ddd-dd.
func punct(d string) string { return d[:3] + "." + d[3:6] + "." + d[6:9] + "-" + d[9:] }

func TestValidCPF(t *testing.T) {
	for _, d := range []string{"12345678909", "11122233396", withDV("987654321"), withDV("000000001"), withDV("390533447")} {
		if !search.ValidCPF(d) {
			t.Errorf("%s should be valid", d)
		}
		for pos := 9; pos <= 10; pos++ {
			b := []byte(d)
			b[pos] = '0' + (b[pos]-'0'+1)%10
			if search.ValidCPF(string(b)) {
				t.Errorf("%s (check digit %d changed) should be invalid", b, pos)
			}
		}
	}
	for _, d := range []string{"00000000000", "11111111111", "99999999999", "1234567890", "123456789012", "1234567890a"} {
		if search.ValidCPF(d) {
			t.Errorf("%s should be invalid", d)
		}
	}
	if withDV("123456789") != "12345678909" || withDV("111222333") != "11122233396" {
		t.Fatal("withDV disagrees with the known valid CPFs")
	}
}

func TestExtractCPF(t *testing.T) {
	cases := []struct {
		q, frag, rest string
		ok            bool
		err           error
	}{
		{q: "12345678909", frag: "456789", ok: true},
		{q: "123.456.789-09", frag: "456789", ok: true},
		{q: "123 456 789 09", frag: "456789", ok: true},
		{q: " 123.456.789/09 ", frag: "456789", ok: true},
		{q: "1234 5678 909", frag: "456789", ok: true}, // only digits: 11 of them
		{q: "***456789**", frag: "456789", rest: " ", ok: true},
		{q: "***.456.789-**", frag: "456789", rest: " ", ok: true},
		{q: "maria silva 123.456.789-09", frag: "456789", rest: "maria silva  ", ok: true},
		{q: "123.456.789-09 maria silva", frag: "456789", rest: "  maria silva", ok: true},
		{q: "maria 12345678909 silva", frag: "456789", rest: "maria   silva", ok: true},
		{q: "maria ***456789** silva", frag: "456789", rest: "maria   silva", ok: true},
		// Glued to a word: not taken out (the word rules drop that token).
		{q: "maria silva123.456.789-09"},
		{q: "maria12345678909"},
		{q: "12345678909x maria"},
		{q: "MARIA***456789**"},
		{q: "maria12345678901"},
		{q: "maria 123.456.789-09 e 123.456.789-09", frag: "456789", rest: "maria   e  ", ok: true}, // same CPF twice
		{q: "123.456.789-09 ***456789**", frag: "456789", rest: "   ", ok: true},                    // full and masked agree
		{q: "***456789** maria ***.456.789-**", frag: "456789", rest: "  maria  ", ok: true},
		// Not a CPF: CNPJ lookups, short numbers, names.
		{q: "456789"},
		{q: "12345678"},
		{q: "12.345.678/0001-90"},
		{q: "12345678000190"},
		{q: "maria silva"},
		{q: "maria 123456789"},
		{q: "posto 15"},
		{q: "****456789**"},
		{q: "***4567890**"},
		{q: "***45678**"},
		// Invalid check digits, or all digits equal.
		{q: "12345678901", err: search.ErrInvalidCPF},
		{q: "123.456.789-01", err: search.ErrInvalidCPF},
		{q: "maria 123.456.789-01", err: search.ErrInvalidCPF},
		{q: "000.000.000-00", err: search.ErrInvalidCPF},
		{q: "111.111.111-11", err: search.ErrInvalidCPF},
		{q: "maria 11111111111", err: search.ErrInvalidCPF},
		// Two different CPFs.
		{q: "123.456.789-09 111.222.333-96", err: search.ErrMultipleCPFs},
		{q: "maria ***456789** 111.222.333-96", err: search.ErrMultipleCPFs},
		{q: "***456789** ***111222**", err: search.ErrMultipleCPFs},
		{q: "123.456.789-09 " + punct(withDV("000456789")), err: search.ErrMultipleCPFs}, // same fragment, two CPFs
		{q: "111.222.333-96 ***456789**", err: search.ErrMultipleCPFs},
	}
	for _, c := range cases {
		frag, rest, ok, err := search.ExtractCPF(c.q)
		if !errors.Is(err, c.err) || (c.err == nil && err != nil) {
			t.Errorf("ExtractCPF(%q) error = %v, want %v", c.q, err, c.err)
			continue
		}
		if ok != c.ok || frag != c.frag || (c.ok && rest != c.rest) {
			t.Errorf("ExtractCPF(%q) = %q, %q, %v; want %q, %q, %v", c.q, frag, rest, ok, c.frag, c.rest, c.ok)
		}
	}
}

// person inserts a person entity with one record per fragment given (none:
// one record without fragment) and returns its id.
func (f *fx) person(name string, fragments ...string) int64 {
	f.t.Helper()
	var id int64
	if err := f.pool.QueryRow(ctx, `INSERT INTO entity (kind, name) VALUES ('person', $1) RETURNING id`, name).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	if len(fragments) == 0 {
		fragments = []string{""}
	}
	for i, frag := range fragments {
		_, err := f.pool.Exec(ctx, `INSERT INTO person_record (source_id, context_key, name, name_norm, cpf_fragment, entity_id, first_collection_id, record_ref)
			VALUES ($1, $2, $3, '', $4, $5, $6, $7)`, f.srcID, fmt.Sprintf("ctx%d-%d", id, i), name, frag, id, f.col, fmt.Sprintf("test:p%d-%d", id, i))
		if err != nil {
			f.t.Fatal(err)
		}
	}
	return id
}

func (f *fx) find(s *search.Service, q string, kind claims.EntityKind, limit int) search.Result {
	f.t.Helper()
	r, err := s.Find(ctx, q, kind, limit)
	if err != nil {
		f.t.Fatalf("Find(%q, %q, %d): %v", q, kind, limit, err)
	}
	return r
}

func TestCPFFragmentSearch(t *testing.T) {
	f := setup(t)
	hub := f.entity(claims.KindCompany, "30000000", "CENTRO FICTICIO")
	low := f.person("ZULMIRA FICTICIA", "456789")            // degree 1
	high := f.person("BRUNO FICTICIO", "456789")             // degree 4
	tieA := f.person("ANA FICTICIA", "456789")               // degree 2, name first
	tieB := f.person("ANA FICTICIA", "456789")               // degree 2, same name, higher id
	two := f.person("CARLA FICTICIA", "456789", "456789")    // two records of the fragment: listed once
	mixed := f.person("DORA FICTICIA 12345678909", "222333") // another fragment (its name carries a CPF)
	f.person("ELIAS FICTICIO")                               // no fragment
	f.person("FABIO FICTICIO", "654321")
	// Companies are never found by a CPF, even one written in their name.
	company := f.entity(claims.KindCompany, "30000001", "GILDA FICTICIA 123.456.789-09")
	f.link(low, hub, 1)
	f.link(high, hub, 4)
	f.link(tieA, hub, 2)
	f.link(tieB, hub, 2)
	f.link(company, hub, 9)
	f.link(mixed, hub, 9)
	s := search.New(f.pool)
	f.rebuild(s)

	want := []int64{high, tieA, tieB, low, two}
	for _, q := range []string{"123.456.789-09", "12345678909", "123 456 789 09", "***456789**", "***.456.789-**", " 123456789-09 "} {
		r := f.find(s, q, "", 20)
		if !r.CPFFragment || !slices.Equal(ids(r.Items), want) {
			t.Errorf("Find(%q) = %v (cpf %v), want %v", q, ids(r.Items), r.CPFFragment, want)
		}
		if r.Items[0].Kind != claims.KindPerson || r.Items[0].Name != "BRUNO FICTICIO" {
			t.Errorf("Find(%q)[0] = %+v", q, r.Items[0])
		}
	}
	// The scope: persons only.
	if r := f.find(s, "123.456.789-09", claims.KindPerson, 20); !slices.Equal(ids(r.Items), want) || !r.CPFFragment {
		t.Errorf("kind person: %v", ids(r.Items))
	}
	for _, k := range []claims.EntityKind{claims.KindCompany, claims.KindParty, claims.KindPublicBody, claims.KindForeign} {
		if r := f.find(s, "123.456.789-09", k, 20); len(r.Items) != 0 || !r.CPFFragment {
			t.Errorf("kind %s: %+v, want nothing (flagged as a CPF search)", k, r)
		}
	}
	// Limit.
	if r := f.find(s, "***456789**", "", 2); !slices.Equal(ids(r.Items), want[:2]) {
		t.Errorf("limit 2: %v", ids(r.Items))
	}
	// Another fragment; the person whose name carries a CPF is shown masked.
	r := f.find(s, "111.222.333-96", "", 20)
	if !slices.Equal(ids(r.Items), []int64{mixed}) || r.Items[0].Name != "DORA FICTICIA ***456789**" {
		t.Errorf("111.222.333-96: %+v", r.Items)
	}
	// A fragment nobody has.
	if r := f.find(s, punct(withDV("999888777")), "", 20); len(r.Items) != 0 || !r.CPFFragment {
		t.Errorf("unknown fragment: %+v", r)
	}
	// Errors.
	for q, wantErr := range map[string]error{
		"123.456.789-01":                 search.ErrInvalidCPF,
		"ana 123.456.789-01":             search.ErrInvalidCPF,
		"000.000.000-00":                 search.ErrInvalidCPF,
		"123.456.789-09 111.222.333-96":  search.ErrMultipleCPFs,
		"ana ***456789** 111.222.333-96": search.ErrMultipleCPFs,
	} {
		if _, err := s.Find(ctx, q, "", 20); !errors.Is(err, wantErr) {
			t.Errorf("Find(%q) error %v, want %v", q, err, wantErr)
		}
		if _, err := s.Find(ctx, q, claims.KindCompany, 20); !errors.Is(err, wantErr) {
			t.Errorf("Find(%q, company) error %v, want %v", q, err, wantErr)
		}
	}
	// A plain name search is not flagged; a bare 6-digit number stays a CNPJ prefix.
	if r := f.find(s, "ana ficticia", "", 20); r.CPFFragment || !slices.Equal(ids(r.Items), []int64{tieA, tieB}) {
		t.Errorf("name search: %+v", r)
	}
	if r := f.find(s, "456789", "", 20); r.CPFFragment || len(r.Items) != 0 {
		t.Errorf("6 digits: %+v", r)
	}
}

func TestCPFFragmentWithWords(t *testing.T) {
	f := setup(t)
	maria := f.person("MARIA SILVA FICTICIA", "456789")
	mariana := f.person("MARIANA SOUZA", "456789")
	f.person("JOAO SILVA", "456789")
	f.person("MARIA SILVA FICTICIA", "111222") // same name, other fragment
	f.entity(claims.KindCompany, "30000002", "MARIA SILVA 123.456.789-09 ME")
	s := search.New(f.pool)
	f.rebuild(s)

	cases := map[string][]int64{
		"maria silva 123.456.789-09":    {maria},
		"123.456.789-09 maria silva":    {maria},
		"silva 12345678909 maria":       {maria},
		"MARIA ***456789**":             {maria, mariana}, // last word as a prefix
		"Mária da Silva ***.456.789-**": {maria},          // accents and stopwords as in name search
		"pedro 123.456.789-09":          nil,
		"maria 99999 123.456.789-09":    {maria},             // a dropped long number at the end: "maria" matches whole
		"de da 123.456.789-09":          {maria, mariana, 0}, // only stopwords: the fragment alone
	}
	all := ids(f.find(s, "123.456.789-09", claims.KindPerson, 20).Items)
	if len(all) != 3 {
		t.Fatalf("fragment alone: %v", all)
	}
	for q, want := range cases {
		if slices.Contains(want, 0) {
			want = all
		}
		// kind person: the intersection alone (with no kind, companies found
		// by the words follow; see TestCPFWithWordsKeepsCompaniesByName).
		r := f.find(s, q, claims.KindPerson, 20)
		got := ids(r.Items)
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		w := slices.Clone(want)
		sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
		if !r.CPFFragment || !slices.Equal(got, w) {
			t.Errorf("Find(%q) = %v, want %v", q, got, w)
		}
	}
}

// TestCPFFragmentSharedByManyPersons: a fragment shared by hundreds of
// persons (the worst case expected on the national base) answers in order,
// within the limit and quickly, through the fragment index.
func TestCPFFragmentSharedByManyPersons(t *testing.T) {
	f := setup(t)
	hub := f.entity(claims.KindCompany, "30000003", "CENTRO FICTICIO")
	_, err := f.pool.Exec(ctx, `WITH e AS (
			INSERT INTO entity (kind, name)
			SELECT 'person', 'PESSOA FICTICIA ' || lpad(g::text, 4, '0') FROM generate_series(1, 320) g RETURNING id, name)
		INSERT INTO person_record (source_id, context_key, name, name_norm, cpf_fragment, entity_id, first_collection_id, record_ref)
		SELECT $1, 'ctx' || e.id, e.name, '', '456789', e.id, $2, 'test:p' || e.id FROM e`, f.srcID, f.col)
	if err != nil {
		t.Fatal(err)
	}
	// Some noise on other fragments, and degrees on a few of them.
	for i := range 50 {
		f.person(fmt.Sprintf("OUTRA PESSOA %d", i), fmt.Sprintf("%06d", 100000+i))
	}
	var some []int64
	if err := f.pool.QueryRow(ctx, `SELECT array_agg(id ORDER BY id) FROM (SELECT id FROM entity WHERE name LIKE 'PESSOA FICTICIA%' ORDER BY id DESC LIMIT 5) x`).Scan(&some); err != nil {
		t.Fatal(err)
	}
	for i, id := range some {
		f.link(id, hub, i+1)
	}
	s := search.New(f.pool)
	f.rebuild(s)

	// Expected: the five linked ones by degree, then the others by name.
	var rest []int64
	if err := f.pool.QueryRow(ctx, `SELECT array_agg(id ORDER BY name, id) FROM entity
		WHERE name LIKE 'PESSOA FICTICIA%' AND id <> ALL($1)`, some).Scan(&rest); err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(some), rest...)
	slices.Reverse(want[:5])

	start := time.Now()
	r := f.find(s, "123.456.789-09", "", 50)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("took %v", el)
	}
	if !slices.Equal(ids(r.Items), want[:50]) {
		t.Errorf("got %v, want %v", ids(r.Items), want[:50])
	}
	// With a word that only some of them have.
	r = f.find(s, "ficticia 0007 ***456789**", "", 50)
	if got := ids(r.Items); len(got) != 1 {
		t.Errorf("ficticia 0007: %v", got)
	}

	// The lookup is index-backed: with sequential scans priced out, the
	// planner reaches person_record through the fragment index.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off; SET LOCAL plan_cache_mode = force_custom_plan`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN "+search.FragmentSQL, "456789", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "person_record_cpf_fragment_entity") {
		t.Errorf("plan does not use the fragment index:\n%s", plan.String())
	}
}

// TestNameWithCPFIsNotFoundByIt: a company or person whose NAME carries a
// CPF is never found by typing that CPF; only a person whose record has the
// fragment is, whatever its name.
func TestNameWithCPFIsNotFoundByIt(t *testing.T) {
	f := setup(t)
	c1 := f.entity(claims.KindCompany, "30000004", "JOANA FICTICIA 12345678909")
	c2 := f.entity(claims.KindCompany, "30000005", "JOANA FICTICIA 123.456.789-09 ME")
	f.person("JOANA FICTICIA 123.456.789-09") // no fragment
	f.person("JOANA FICTICIA", "111222")      // other fragment
	s := search.New(f.pool)
	f.rebuild(s)
	for _, q := range []string{"12345678909", "joana ***456789**", "joana ficticia 12345678909"} {
		if r := f.find(s, q, claims.KindPerson, 50); len(r.Items) != 0 {
			t.Errorf("Find(%q, person) = %+v, want nothing", q, r.Items)
		}
	}
	if r := f.find(s, "12345678909", "", 50); len(r.Items) != 0 {
		t.Errorf("CPF alone: %+v", r.Items)
	}
	// With no kind, companies come from the words only: the same whatever
	// (valid) CPF is typed, so the CPF in their names is never confirmed.
	other := punct(withDV("987654321"))
	for _, words := range []string{"joana ficticia", "JOANA FICTICIA ME"} {
		a := f.find(s, words+" 123.456.789-09", "", 50)
		b := f.find(s, words+" "+other, "", 50)
		if a.FragmentHits != 0 || !slices.Equal(ids(a.Items), ids(b.Items)) {
			t.Errorf("%q: %v (fragment hits %d) vs other CPF %v", words, ids(a.Items), a.FragmentHits, ids(b.Items))
		}
		got := ids(a.Items)
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if !slices.Equal(got, []int64{c1, c2}) {
			t.Errorf("%q: %v, want the companies %d, %d", words, got, c1, c2)
		}
	}
	if got := f.find(s, "joana ficticia", "", 50).Items; len(got) != 4 {
		t.Errorf("by its words: %+v", got)
	}
}

// TestCPFWithWordsKeepsCompaniesByName: a displayed company name with a
// masked CPF, pasted back, lists the person candidates first and then the
// companies its words find (m5).
func TestCPFWithWordsKeepsCompaniesByName(t *testing.T) {
	f := setup(t)
	co := f.entity(claims.KindCompany, "30000006", "JOAO DA SILVA FICTICIO 12345678909")
	p := f.person("JOAO DA SILVA FICTICIO", "456789")
	f.person("JOAO DA SILVA FICTICIO", "222333")
	s := search.New(f.pool)
	f.rebuild(s)
	r := f.find(s, "JOAO DA SILVA FICTICIO ***456789**", "", 20)
	if !r.CPFFragment || r.FragmentHits != 1 || !slices.Equal(ids(r.Items), []int64{p, co}) {
		t.Errorf("no kind: %+v", r)
	}
	if r := f.find(s, "JOAO DA SILVA FICTICIO ***456789**", claims.KindPerson, 20); !slices.Equal(ids(r.Items), []int64{p}) {
		t.Errorf("person: %v", ids(r.Items))
	}
	if r := f.find(s, "JOAO DA SILVA FICTICIO ***456789**", claims.KindCompany, 20); len(r.Items) != 0 {
		t.Errorf("company: %v", ids(r.Items))
	}
	// Limit: the candidates first.
	if r := f.find(s, "joao silva ***456789**", "", 1); !slices.Equal(ids(r.Items), []int64{p}) || r.FragmentHits != 1 {
		t.Errorf("limit 1: %+v", r)
	}
}

// TestCPFFragmentAboveCap: a fragment with more records than the cap is not
// listed alone (flagged); with name words it is searched from the name side.
func TestCPFFragmentAboveCap(t *testing.T) {
	f := setup(t)
	hub := f.entity(claims.KindCompany, "30000007", "CENTRO FICTICIO")
	var many []int64
	for i := range 8 {
		many = append(many, f.person(fmt.Sprintf("PESSOA COMUM %c", 'A'+i), "456789"))
	}
	target := f.person("ROSA FICTICIA", "456789")
	f.person("ROSA FICTICIA", "222333")
	rare := f.person("RUI FICTICIO", "222333")
	f.link(many[3], hub, 2)
	s := search.New(f.pool)
	s.SetFragmentCap(5)
	f.rebuild(s)

	r := f.find(s, "123.456.789-09", "", 20)
	if !r.CPFFragment || !r.FragmentTooCommon || len(r.Items) != 0 {
		t.Errorf("alone: %+v", r)
	}
	r = f.find(s, "rosa 123.456.789-09", claims.KindPerson, 20)
	if r.FragmentTooCommon || !slices.Equal(ids(r.Items), []int64{target}) || r.FragmentHits != 1 {
		t.Errorf("rosa: %+v", r)
	}
	// Name side, rank order: the linked one first.
	r = f.find(s, "pessoa comum ***456789**", claims.KindPerson, 3)
	if got := ids(r.Items); len(got) != 3 || got[0] != many[3] {
		t.Errorf("pessoa comum: %v", got)
	}
	// Too-short words do not unlock it.
	if r := f.find(s, "de 123.456.789-09", "", 20); !r.FragmentTooCommon || len(r.Items) != 0 {
		t.Errorf("stopword only: %+v", r)
	}
	// A fragment under the cap is listed as usual.
	if r := f.find(s, "111.222.333-96", "", 20); r.FragmentTooCommon || len(r.Items) != 2 {
		t.Errorf("222333: %+v (rare %d)", r, rare)
	}
}

// TestCPFFragmentAfterMerge: an entity emptied by a merge (its records moved
// to another) is not a candidate; the receiving entity is listed once.
func TestCPFFragmentAfterMerge(t *testing.T) {
	f := setup(t)
	a := f.person("MARTA FICTICIA", "456789")
	b := f.person("MARTA FICTICIA", "456789")
	if _, err := f.pool.Exec(ctx, `UPDATE person_record SET entity_id = $1 WHERE entity_id = $2`, b, a); err != nil {
		t.Fatal(err)
	}
	s := search.New(f.pool)
	f.rebuild(s)
	if r := f.find(s, "123.456.789-09", "", 20); !slices.Equal(ids(r.Items), []int64{b}) {
		t.Errorf("got %v, want only %d", ids(r.Items), b)
	}
}

// TestCPFInCompatibilityDigits: fullwidth digits and punctuation are a CPF
// too (never left as words).
func TestCPFInCompatibilityDigits(t *testing.T) {
	f := setup(t)
	p := f.person("MARIA FICTICIA", "456789")
	s := search.New(f.pool)
	f.rebuild(s)
	for _, q := range []string{"１２３.４５６.７８９-０９", "maria １２３４５６７８９０９", "＊＊＊４５６７８９＊＊"} {
		if r := f.find(s, q, claims.KindPerson, 20); !r.CPFFragment || !slices.Equal(ids(r.Items), []int64{p}) {
			t.Errorf("Find(%q) = %+v", q, r)
		}
	}
	if _, err := s.Find(ctx, "１２３.４５６.７８９-０１", "", 20); !errors.Is(err, search.ErrInvalidCPF) {
		t.Errorf("fullwidth invalid: %v", err)
	}
}
