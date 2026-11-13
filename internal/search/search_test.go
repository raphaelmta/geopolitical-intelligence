package search_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/graph"
	"geoint/internal/search"
	"geoint/internal/sources"
	"geoint/internal/testutil"
)

// All names, CPF fragments and CNPJs in these tests are invented.

var ctx = context.Background()

type fx struct {
	t     *testing.T
	pool  *pgxpool.Pool
	srcID int16
	col   int64
	rels  int
}

func setup(t *testing.T) *fx {
	t.Helper()
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	src, _ := sources.ByKey("receita")
	col, err := collection.Begin(ctx, pool, src.ID, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	return &fx{t: t, pool: pool, srcID: src.ID, col: col.ID}
}

// entity inserts an entity; an empty key is stored as NULL. A person also
// gets a person record (person entities without one are not searchable).
func (f *fx) entity(kind claims.EntityKind, key, name string) int64 {
	f.t.Helper()
	var id int64
	err := f.pool.QueryRow(ctx, `INSERT INTO entity (kind, natural_key, name) VALUES ($1, NULLIF($2, ''), $3) RETURNING id`,
		string(kind), key, name).Scan(&id)
	if err != nil {
		f.t.Fatal(err)
	}
	if kind == claims.KindPerson {
		f.recordPersons()
	}
	return id
}

// recordPersons gives every person entity without a person record one.
func (f *fx) recordPersons() {
	f.t.Helper()
	_, err := f.pool.Exec(ctx, `INSERT INTO person_record (source_id, context_key, name, name_norm, entity_id, first_collection_id, record_ref)
		SELECT $1, 'ctx' || e.id, e.name, '', e.id, $2, 'test:p' || e.id FROM entity e
		WHERE e.kind = 'person' AND NOT EXISTS (SELECT 1 FROM person_record p WHERE p.entity_id = e.id)`, f.srcID, f.col)
	if err != nil {
		f.t.Fatal(err)
	}
}

// link adds n open relationships from src to dst.
func (f *fx) link(src, dst int64, n int) {
	f.t.Helper()
	f.rels++
	_, err := f.pool.Exec(ctx, `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, attrs, kind, first_collection_id, record_ref)
		SELECT 'partner_of', $1, $2, jsonb_build_object('role', $4::text || '-' || g), 'fact', $3, 'test:' || $4::text || ':' || g
		FROM generate_series(1, $5::int) g`, src, dst, f.col, strconv.Itoa(f.rels), n)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) rebuild(s *search.Service) search.Stats {
	f.t.Helper()
	st, err := s.Rebuild(ctx)
	if err != nil {
		f.t.Fatalf("rebuild: %v", err)
	}
	return st
}

func (f *fx) query(s *search.Service, q string, kind claims.EntityKind, limit int) []graph.EntityRef {
	f.t.Helper()
	got, err := s.Query(ctx, q, kind, limit)
	if err != nil {
		f.t.Fatalf("Query(%q, %q, %d): %v", q, kind, limit, err)
	}
	return got
}

func ids(refs []graph.EntityRef) []int64 {
	out := make([]int64, len(refs))
	for i, r := range refs {
		out[i] = r.ID
	}
	return out
}

func TestSearchNormalizesInput(t *testing.T) {
	f := setup(t)
	target := f.entity(claims.KindCompany, "12345678", "AÇÚCAR MODELO LTDA")
	other := f.entity(claims.KindCompany, "87654321", "MODELO ACUCAREIRO SA")
	f.entity(claims.KindPerson, "", "ACUCAR CANDIDO")
	f.entity(claims.KindPerson, "", "ACUCAR MODELO NORTE") // same words, same degree, name sorts after
	hub := f.entity(claims.KindCompany, "11111111", "PORTO DISTANTE")
	f.link(other, hub, 5) // higher degree, but the words do not match
	s := search.New(f.pool)
	f.rebuild(s)

	for _, q := range []string{"açúcar modelo", "ACUCAR MODELO", "12.345.678/0001-90", "12345678000190", "12345678"} {
		got := f.query(s, q, "", 20)
		if len(got) == 0 || got[0].ID != target {
			t.Errorf("Query(%q) = %+v, want entity %d first", q, got, target)
			continue
		}
		want := graph.EntityRef{ID: target, Kind: claims.KindCompany, Name: "AÇÚCAR MODELO LTDA", NaturalKey: "12345678"}
		if got[0] != want {
			t.Errorf("Query(%q)[0] = %+v, want %+v", q, got[0], want)
		}
	}
}

func TestExactNameTierBeatsWordTier(t *testing.T) {
	f := setup(t)
	exact := f.entity(claims.KindPerson, "", "Alfa Beta")
	word := f.entity(claims.KindCompany, "10000001", "ALFA BETA GAMA")
	hub := f.entity(claims.KindCompany, "10000002", "HUB")
	f.link(word, hub, 10)
	s := search.New(f.pool)
	f.rebuild(s)
	got := ids(f.query(s, "ALFA BETA", "", 20))
	if !slices.Equal(got, []int64{exact, word}) {
		t.Fatalf("got %v, want exact name %d before higher-degree word match %d", got, exact, word)
	}
}

func TestWordTierOrderedByDegreeThenNameThenID(t *testing.T) {
	f := setup(t)
	hub := f.entity(claims.KindCompany, "20000000", "CENTRO")
	low := f.entity(claims.KindCompany, "20000001", "ZETA ARCO")    // degree 1
	b2 := f.entity(claims.KindCompany, "20000002", "ZETA BRAVO")    // degree 3
	a1 := f.entity(claims.KindPerson, "", "ZETA ALVO")              // degree 3, name before ZETA BRAVO
	b1 := f.entity(claims.KindPerson, "", "ZETA BRAVO")             // degree 3, same name as b2 but higher id
	high := f.entity(claims.KindForeign, "F1", "ZETA ZULU")         // degree 7
	zero := f.entity(claims.KindCompany, "20000003", "ZETA AAA")    // degree 0
	lower := f.entity(claims.KindCompany, "20000004", "ZETA bravo") // same normalized name, degree 3, highest id
	f.link(low, hub, 1)
	f.link(b2, hub, 3)
	f.link(a1, hub, 3)
	f.link(b1, hub, 3)
	f.link(high, hub, 7)
	f.link(lower, hub, 3)
	s := search.New(f.pool)
	f.rebuild(s)
	got := ids(f.query(s, "zeta", "", 20))
	want := []int64{high, a1, b2, b1, lower, low, zero}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// vocabulary of the synthetic regression data set: overlapping words and
// prefixes (ALFA/ALFAMAR, PETRO/PETROLINA), stopwords and accents.
var vocabulary = []string{"ALFA", "ALFAMAR", "BETA", "BETÂNIA", "GAMA", "DELTA", "SOLAR", "SOLARIS", "NORTE",
	"SUL", "COMÉRCIO", "SERVIÇOS", "DE", "LTDA", "PETRO", "PETROLINA", "SÃO", "JOÃO", "MARIA", "S/A"}

// populate inserts n synthetic entities of varied kinds and degrees (0 to
// 12, through a hub) and returns their ids.
func populate(f *fx, n int, seed uint64) []int64 {
	f.t.Helper()
	r := rand.New(rand.NewPCG(seed, 7))
	kinds := make([]string, n)
	keys := make([]string, n)
	names := make([]string, n)
	for i := range n {
		words := make([]string, 1+r.IntN(4))
		for j := range words {
			words[j] = vocabulary[r.IntN(len(vocabulary))]
		}
		names[i] = strings.Join(words, " ")
		switch k := r.IntN(20); {
		case k < 10:
			kinds[i], keys[i] = "company", fmt.Sprintf("%08d", 30000000+i)
		case k < 17:
			kinds[i] = "person"
		case k < 19:
			kinds[i], keys[i] = "foreign", fmt.Sprintf("FX%d", i)
		default:
			kinds[i], keys[i] = "party", fmt.Sprintf("PT%d", i)
		}
	}
	rows, err := f.pool.Query(ctx, `INSERT INTO entity (kind, natural_key, name)
		SELECT k, NULLIF(nk, ''), nm FROM unnest($1::text[], $2::text[], $3::text[]) AS u(k, nk, nm) RETURNING id`, kinds, keys, names)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	f.recordPersons()
	hub := f.entity(claims.KindCompany, "99999999", "QQQ HUB")
	degrees := make([]int32, len(out))
	for i := range degrees {
		if r.IntN(4) > 0 {
			degrees[i] = int32(r.IntN(13))
		}
	}
	_, err = f.pool.Exec(ctx, `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, attrs, kind, first_collection_id, record_ref)
		SELECT 'partner_of', u.id, $3, jsonb_build_object('role', g), 'fact', $4, 'test:' || u.id || ':' || g
		FROM unnest($1::bigint[], $2::int[]) AS u(id, d), generate_series(1, u.d) g`, out, degrees, hub, f.col)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// reference returns the expected result computed independently of the
// search tables: every matching entity fully sorted (exact name first, then
// degree desc, normalized name in byte order, id) before the limit.
func reference(f *fx, q, tsq string, kind claims.EntityKind, limit int) []int64 {
	f.t.Helper()
	rows, err := f.pool.Query(ctx, `
		WITH deg AS (
			SELECT v.e, count(*) AS g
			FROM relationship r CROSS JOIN LATERAL (VALUES (r.src_entity_id), (r.dst_entity_id)) AS v(e)
			WHERE r.closed_collection_id IS NULL
			GROUP BY v.e),
		c AS (
			SELECT e.id, e.kind, search_norm(e.name) AS n, coalesce(d.g, 0) AS g
			FROM entity e LEFT JOIN deg d ON d.e = e.id
			WHERE e.kind <> 'person' OR EXISTS (SELECT 1 FROM person_record p WHERE p.entity_id = e.id))
		SELECT id FROM c
		WHERE n IS NOT NULL AND ($3 = '' OR kind = $3)
		  AND (n = search_norm($1) OR search_tokens(n) @@ $2::tsquery)
		ORDER BY (n = search_norm($1)) DESC, g DESC, n COLLATE "C", id
		LIMIT $4`, q, tsq, string(kind), limit)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// regressionQueries pair a query with the tsquery the strategy derives from
// it when no word is frequent.
var regressionQueries = []struct{ q, tsq string }{
	{"ALFA", "'alfa':*"},
	{"alfa beta", "'alfa' & 'beta':*"},
	{"SOLAR NOR", "'solar' & 'nor':*"},
	{"comércio de ser", "'comercio' & 'ser':*"},
	{"PETRO", "'petro':*"},
	{"GAMA DELTA", "'gama' & 'delta':*"},
	{"SUL", "'sul':*"},
	{"SAO JOAO", "'sao' & 'joao':*"},
	{"BETA LTDA", "'beta'"},
	{"MARIA", "'maria':*"},
}

// TestQueryOrderMatchesFullySortedReference guards the reliance of the word
// tier on the physical order of the search tables: a LIMIT without ORDER BY
// must return exactly the top of the fully sorted list.
func TestQueryOrderMatchesFullySortedReference(t *testing.T) {
	f := setup(t)
	populate(f, 2500, 1)
	s := search.NewForTest(f.pool, 1<<40, 30*time.Second) // no word is frequent
	f.rebuild(s)
	for _, rq := range regressionQueries {
		for _, kind := range []claims.EntityKind{"", claims.KindCompany, claims.KindPerson, claims.KindForeign, claims.KindParty} {
			for _, limit := range []int{20, 50} {
				got := ids(f.query(s, rq.q, kind, limit))
				want := reference(f, rq.q, rq.tsq, kind, limit)
				if len(want) == 0 && kind == "" {
					t.Fatalf("reference for %q kind %q is empty; the data set does not exercise it", rq.q, kind)
				}
				if !slices.Equal(got, want) {
					t.Errorf("Query(%q, %q, %d)\n got %v\nwant %v", rq.q, kind, limit, got, want)
				}
			}
		}
	}
}

func TestFrequentTokenMatchesWholeWordOnly(t *testing.T) {
	f := setup(t)
	populate(f, 2500, 2)
	s := search.NewForTest(f.pool, 150, 30*time.Second)
	st := f.rebuild(s)
	if st.FrequentTokens == 0 {
		t.Fatal("no frequent token with threshold 150; the data set does not exercise the path")
	}
	var frequent bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM search_frequent_token WHERE token = 'alfa')`).Scan(&frequent); err != nil {
		t.Fatal(err)
	}
	if !frequent {
		t.Fatal("alfa is not frequent")
	}
	for _, kind := range []claims.EntityKind{"", claims.KindCompany, claims.KindPerson} {
		got := f.query(s, "ALFA", kind, 50)
		want := reference(f, "ALFA", "'alfa'", kind, 50)
		if !slices.Equal(ids(got), want) {
			t.Errorf("kind %q: got %v, want %v", kind, ids(got), want)
		}
		for _, r := range got {
			if !strings.Contains(" "+r.Name+" ", " ALFA ") {
				t.Errorf("frequent last word matched by prefix: %q", r.Name)
			}
		}
	}
	// A frequent word before the last one does not change the last word's prefix.
	got := ids(f.query(s, "ALFA BET", "", 50))
	if want := reference(f, "ALFA BET", "'alfa' & 'bet':*", "", 50); !slices.Equal(got, want) {
		t.Errorf("ALFA BET: got %v, want %v", got, want)
	}
}

func TestPrefixOnlyOnLastWord(t *testing.T) {
	f := setup(t)
	petro := f.entity(claims.KindCompany, "40000001", "PETROLEO BRASILEIRO S A PETROBRAS")
	f.entity(claims.KindCompany, "40000002", "BRASILEIRO PETROLEO")
	s := search.New(f.pool)
	f.rebuild(s)
	if got := ids(f.query(s, "petrob", "", 20)); !slices.Equal(got, []int64{petro}) {
		t.Errorf("petrob: got %v, want [%d]", got, petro)
	}
	if got := ids(f.query(s, "brasileiro petrob", "", 20)); !slices.Equal(got, []int64{petro}) {
		t.Errorf("brasileiro petrob: got %v, want [%d]", got, petro)
	}
	if got := f.query(s, "petrob brasileiro", "", 20); len(got) != 0 {
		t.Errorf("prefix applied to an earlier word: %+v", got)
	}
	if got := f.query(s, "trobras", "", 20); len(got) != 0 {
		t.Errorf("mid-word substring matched: %+v", got)
	}
}

func TestStopwordsIgnored(t *testing.T) {
	f := setup(t)
	a := f.entity(claims.KindCompany, "50000001", "COMERCIO ALIMENTOS")
	b := f.entity(claims.KindCompany, "50000002", "DE ALIMENTOS DA SERRA")
	s := search.New(f.pool)
	f.rebuild(s)
	if got := ids(f.query(s, "comercio de alim", "", 20)); !slices.Equal(got, []int64{a}) {
		t.Errorf("comercio de alim: got %v, want [%d]", got, a)
	}
	// The last typed word is a stopword: no word gets a prefix.
	if got := f.query(s, "alim de", "", 20); len(got) != 0 {
		t.Errorf("alim de: got %+v, want nothing", got)
	}
	if got := ids(f.query(s, "alimentos de", "", 20)); !slices.Equal(got, []int64{a, b}) {
		t.Errorf("alimentos de: got %v, want [%d %d]", got, a, b)
	}
}

func TestKindFilter(t *testing.T) {
	f := setup(t)
	byKind := map[claims.EntityKind]int64{}
	for i, k := range []claims.EntityKind{claims.KindCompany, claims.KindPerson, claims.KindForeign, claims.KindParty, claims.KindPublicBody} {
		byKind[k] = f.entity(k, fmt.Sprintf("6000000%d", i), "ORION "+string(k))
	}
	s := search.New(f.pool)
	f.rebuild(s)
	if got := f.query(s, "orion", "", 20); len(got) != 5 {
		t.Fatalf("all kinds: got %+v", got)
	}
	for k, id := range byKind {
		got := f.query(s, "orion", k, 20)
		if len(got) != 1 || got[0].ID != id || got[0].Kind != k {
			t.Errorf("kind %s: got %+v, want [%d]", k, got, id)
		}
	}
	var table string
	if err := f.pool.QueryRow(ctx, `SELECT 'search_other' FROM search_other WHERE entity_id = $1`, byKind[claims.KindForeign]).Scan(&table); err != nil {
		t.Errorf("foreign entity not in search_other: %v", err)
	}
	if _, err := s.Query(ctx, "orion", "planet", 20); !errors.Is(err, search.ErrBadKind) {
		t.Errorf("unknown kind: err = %v, want ErrBadKind", err)
	}
}

func TestLimitClamp(t *testing.T) {
	f := setup(t)
	for i := range 60 {
		f.entity(claims.KindPerson, "", fmt.Sprintf("LYRA %03d", i))
	}
	s := search.New(f.pool)
	f.rebuild(s)
	for limit, want := range map[int]int{0: 20, -3: 20, 5: 5, 50: 50, 51: 50, 1000: 50} {
		if got := f.query(s, "lyra", "", limit); len(got) != want {
			t.Errorf("limit %d: got %d results, want %d", limit, len(got), want)
		}
	}
}

func TestQueryTooShort(t *testing.T) {
	f := setup(t)
	f.entity(claims.KindCompany, "70000001", "AB")
	f.entity(claims.KindCompany, "70000002", "LTDA")
	s := search.New(f.pool)
	f.rebuild(s)
	for _, q := range []string{"", "   ", "ab", "Ab", "a b", "de da", "LTDA", "de ltda s/a", "...", "--/--", "12", "1.2", "!?"} {
		if got, err := s.Query(ctx, q, "", 20); !errors.Is(err, search.ErrQueryTooShort) {
			t.Errorf("Query(%q) = %+v, %v; want ErrQueryTooShort", q, got, err)
		}
	}
	if _, err := s.Query(ctx, "abc", "", 20); err != nil {
		t.Errorf("3 letters: %v", err)
	}
	if _, err := s.Query(ctx, "123", "", 20); err != nil {
		t.Errorf("3 digits: %v", err)
	}
}

func TestCNPJLookups(t *testing.T) {
	f := setup(t)
	a := f.entity(claims.KindCompany, "12340001", "CNPJ ALFA")
	b := f.entity(claims.KindCompany, "12340002", "CNPJ BETA")
	c := f.entity(claims.KindCompany, "12399999", "CNPJ GAMA")
	d := f.entity(claims.KindCompany, "12350000", "CNPJ DELTA")
	f.entity(claims.KindForeign, "12340003", "NOT A COMPANY")
	s := search.New(f.pool)
	f.rebuild(s)
	cases := map[string][]int64{
		"12340001":           {a},
		"12340002000155":     {b},
		"12.340.002/0001-55": {b},
		" 12 340 002 ":       {b},
		"12340009":           nil,
		"1234":               {a, b},
		"123.4":              {a, b},
		"1239":               {c},
	}
	for q, want := range cases {
		if got := ids(f.query(s, q, "", 20)); !slices.Equal(got, want) {
			t.Errorf("Query(%q) = %v, want %v", q, got, want)
		}
	}
	if got := ids(f.query(s, "123", "", 20)); !slices.Equal(got, []int64{a, b, d, c}) {
		t.Errorf("prefix 123 = %v, want the 4 companies in key order", got)
	}
	if got := f.query(s, "123", "", 2); len(got) != 2 {
		t.Errorf("prefix limit: got %d", len(got))
	}
	if got := f.query(s, "12340001", claims.KindPerson, 20); len(got) != 0 {
		t.Errorf("CNPJ with kind person: %+v", got)
	}
	if got := ids(f.query(s, "12340001", claims.KindCompany, 20)); !slices.Equal(got, []int64{a}) {
		t.Errorf("CNPJ with kind company: %v", got)
	}
	// Letters and digits together are a name query.
	if got := ids(f.query(s, "12340001 GAMA", "", 20)); !slices.Equal(got, []int64{c}) {
		t.Errorf("mixed query: got %v, want the name match [%d]", got, c)
	}
}

func TestCPFLikeQueryReturnsNothing(t *testing.T) {
	f := setup(t)
	f.entity(claims.KindCompany, "12345678", "JOANA FICTICIA 12345678901")
	f.entity(claims.KindPerson, "", "12345678901")
	s := search.New(f.pool)
	f.rebuild(s)
	for _, q := range []string{"12345678901", "123.456.789-01", "123456789", "1234567890123"} {
		got, err := s.Query(ctx, q, "", 20)
		if err != nil || len(got) != 0 {
			t.Errorf("Query(%q) = %+v, %v; want nothing", q, got, err)
		}
	}
	// A full CPF inside a name query is not searched either.
	got, err := s.Query(ctx, "JOANA FICTICIA 12345678901", "", 20)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].Name != "JOANA FICTICIA ***456789**" {
		t.Errorf("name not masked: %q", got[0].Name)
	}
}

func TestEmptyNameCompanyFoundOnlyByCNPJ(t *testing.T) {
	f := setup(t)
	id := f.entity(claims.KindCompany, "80000001", "")
	blank := f.entity(claims.KindCompany, "80000002", "   ")
	s := search.New(f.pool)
	st := f.rebuild(s)
	if st.Entities["company"] != 0 {
		t.Errorf("empty names indexed: %+v", st.Entities)
	}
	if got := ids(f.query(s, "80000001", "", 20)); !slices.Equal(got, []int64{id}) {
		t.Errorf("by CNPJ: %v", got)
	}
	if got := ids(f.query(s, "8000000", "", 20)); !slices.Equal(got, []int64{id, blank}) {
		t.Errorf("by CNPJ prefix: %v", got)
	}
}

func TestQueryBeforeAnyRebuildAndRebuildOnEmptyDB(t *testing.T) {
	f := setup(t)
	s := search.New(f.pool)
	if got := f.query(s, "anything", "", 20); len(got) != 0 {
		t.Fatalf("before rebuild: %+v", got)
	}
	st := f.rebuild(s)
	for k, n := range st.Entities {
		if n != 0 {
			t.Errorf("empty DB: %s = %d", k, n)
		}
	}
	if st.FrequentTokens != 0 {
		t.Errorf("frequent tokens = %d", st.FrequentTokens)
	}
	if got := f.query(s, "anything", "", 20); len(got) != 0 {
		t.Fatalf("after rebuild: %+v", got)
	}
}

func TestRebuildStatsCountByKind(t *testing.T) {
	f := setup(t)
	f.entity(claims.KindCompany, "81000001", "STAT UM")
	f.entity(claims.KindCompany, "81000002", "STAT DOIS")
	f.entity(claims.KindPerson, "", "STAT TRES")
	f.entity(claims.KindPublicBody, "OB1", "STAT QUATRO")
	st := f.rebuild(search.New(f.pool))
	want := map[string]int64{"company": 2, "person": 1, "public_body": 1}
	for k, n := range want {
		if st.Entities[k] != n {
			t.Errorf("Entities[%s] = %d, want %d (%+v)", k, st.Entities[k], n, st.Entities)
		}
	}
	if st.Duration <= 0 {
		t.Errorf("duration %v", st.Duration)
	}
}

func generation(f *fx) int64 {
	f.t.Helper()
	var g int64
	if err := f.pool.QueryRow(ctx, `SELECT generation FROM search_state`).Scan(&g); err != nil {
		f.t.Fatal(err)
	}
	return g
}

func leftovers(f *fx) []string {
	f.t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT c.relname::text FROM pg_class c
		WHERE c.relnamespace = current_schema()::regnamespace
		  AND (c.relname LIKE 'search\_%\_new%' OR c.relname = 'search_ranked') ORDER BY 1`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestRebuildTwiceSwapsAndRefreshesCache(t *testing.T) {
	f := setup(t)
	for i := range 5 {
		f.entity(claims.KindPerson, "", fmt.Sprintf("OMEGA %d", i))
	}
	f.entity(claims.KindCompany, "82000001", "OMEGAX")
	builder := search.NewForTest(f.pool, 10, 30*time.Second)
	reader := search.NewForTest(f.pool, 10, 0) // checks the generation on every query
	g0 := generation(f)
	f.rebuild(builder)
	if g1 := generation(f); g1 != g0+1 {
		t.Fatalf("generation %d -> %d", g0, g1)
	}
	// omega is in 5 names, below the threshold: the prefix reaches OMEGAX.
	for _, s := range []*search.Service{builder, reader} {
		if got := f.query(s, "omega", "", 20); len(got) != 6 {
			t.Fatalf("before: %d results", len(got))
		}
	}
	for i := 5; i < 12; i++ {
		f.entity(claims.KindPerson, "", fmt.Sprintf("OMEGA %d", i))
	}
	added := f.entity(claims.KindCompany, "82000002", "SIRIUS NOVA")
	f.rebuild(builder)
	if g2 := generation(f); g2 != g0+2 {
		t.Fatalf("generation after second rebuild %d, want %d", g2, g0+2)
	}
	if l := leftovers(f); len(l) != 0 {
		t.Errorf("leftover tables: %v", l)
	}
	for _, s := range []*search.Service{builder, reader} {
		if got := ids(f.query(s, "sirius", "", 20)); !slices.Equal(got, []int64{added}) {
			t.Errorf("new entity not found: %v", got)
		}
		// omega is now frequent: whole word only, OMEGAX is gone.
		if got := f.query(s, "omega", "", 20); len(got) != 12 {
			t.Errorf("after: %d results, want 12 (cache not refreshed?)", len(got))
		}
	}
}

func TestFrequentCacheChecksGenerationAtMostEveryInterval(t *testing.T) {
	f := setup(t)
	for i := range 3 {
		f.entity(claims.KindPerson, "", fmt.Sprintf("VEGA %d", i))
	}
	f.entity(claims.KindCompany, "83000001", "VEGANO")
	builder := search.NewForTest(f.pool, 3, 30*time.Second)
	slow := search.NewForTest(f.pool, 3, time.Hour)
	f.rebuild(builder) // vega in 3 names: frequent at threshold 3
	if got := f.query(slow, "vega", "", 20); len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	// A rebuild with a higher threshold empties the frequent list; slow keeps
	// its cache until the interval passes.
	f.rebuild(search.NewForTest(f.pool, 100, 30*time.Second))
	if got := f.query(slow, "vega", "", 20); len(got) != 3 {
		t.Errorf("cache refreshed before the interval: %d results", len(got))
	}
	if got := f.query(builder, "vega", "", 20); len(got) != 4 {
		t.Errorf("fresh service: %d results, want 4", len(got))
	}
}

func TestQueriesKeepWorkingDuringRebuild(t *testing.T) {
	f := setup(t)
	populate(f, 3000, 3)
	s := search.New(f.pool)
	f.rebuild(s)
	reader := search.NewForTest(f.pool, 100000, 0)
	done := make(chan struct{})
	var queries atomic.Int64
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			qs := []string{"alfa beta", "petro", "3000", "sao joao", "maria"}
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				got, err := reader.Query(ctx, qs[(i+w)%len(qs)], "", 20)
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("query during rebuild: %v", err)
					return
				}
				if err == nil && len(got) == 0 {
					t.Errorf("query %q returned nothing during rebuild", qs[(i+w)%len(qs)])
					return
				}
				queries.Add(1)
			}
		}()
	}
	// Deterministic part: a query at the latest moment, with the new tables
	// built and the swap about to start.
	var hooked int
	s.SetBeforeSwap(func(int) {
		hooked++
		if got, err := reader.Query(ctx, "alfa beta", "", 20); err != nil || len(got) == 0 {
			t.Errorf("query before swap: %d results, %v", len(got), err)
		}
	})
	var rebuildErr error
	for range 3 {
		if _, rebuildErr = s.Rebuild(ctx); rebuildErr != nil {
			break
		}
	}
	close(done)
	wg.Wait()
	if rebuildErr != nil {
		t.Fatalf("rebuild: %v", rebuildErr)
	}
	if queries.Load() == 0 || hooked != 3 {
		t.Fatalf("queries %d, before-swap queries %d", queries.Load(), hooked)
	}
}

func TestConcurrentRebuildsAreSerialized(t *testing.T) {
	f := setup(t)
	populate(f, 500, 4)
	g0 := generation(f)
	var active, maxActive atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		s := search.New(f.pool)
		s.SetAfterLock(func() {
			n := active.Add(1)
			for {
				m := maxActive.Load()
				if n <= m || maxActive.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			active.Add(-1)
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Rebuild(ctx)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("rebuild: %v", err)
		}
	}
	if m := maxActive.Load(); m != 1 {
		t.Errorf("%d rebuilds held the lock at once", m)
	}
	if g := generation(f); g != g0+3 {
		t.Errorf("generation %d, want %d (every caller rebuilds)", g, g0+3)
	}
	if l := leftovers(f); len(l) != 0 {
		t.Errorf("leftover tables: %v", l)
	}
}

func TestDegreeCountsOnlyOpenRelationships(t *testing.T) {
	f := setup(t)
	hub := f.entity(claims.KindCompany, "84000000", "NUCLEO")
	lost := f.entity(claims.KindCompany, "84000001", "RIGEL PERDIDA")
	kept := f.entity(claims.KindCompany, "84000002", "RIGEL MANTIDA")
	self := f.entity(claims.KindCompany, "84000003", "RIGEL PROPRIA")
	f.link(lost, hub, 5)
	f.link(kept, hub, 2)
	f.link(self, self, 1) // a relationship with itself counts once
	src, _ := sources.ByKey("receita")
	col2, err := collection.Begin(ctx, f.pool, src.ID, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE relationship SET closed_collection_id = $1 WHERE src_entity_id = $2`, col2.ID, lost); err != nil {
		t.Fatal(err)
	}
	s := search.New(f.pool)
	f.rebuild(s)
	degree := func(id int64) int {
		var g int
		if err := f.pool.QueryRow(ctx, `SELECT grau FROM search_company WHERE entity_id = $1`, id).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	if g := degree(lost); g != 0 {
		t.Errorf("lost: degree %d, want 0", g)
	}
	if g := degree(kept); g != 2 {
		t.Errorf("kept: degree %d, want 2", g)
	}
	if g := degree(self); g != 1 {
		t.Errorf("self: degree %d, want 1", g)
	}
	if g := degree(hub); g != 2 {
		t.Errorf("hub: degree %d, want 2", g)
	}
	if got := ids(f.query(s, "rigel", "", 20)); !slices.Equal(got, []int64{kept, self, lost}) {
		t.Errorf("order %v, want [%d %d %d]", got, kept, self, lost)
	}
}

func TestWordsWithLongDigitRunsAreNotSearchable(t *testing.T) {
	f := setup(t)
	id := f.entity(claims.KindCompany, "85000001", "MARIA12345678901 COMERCIO VAREJO")
	s := search.New(f.pool)
	f.rebuild(s)
	for _, q := range []string{"MARIA12345678901", "maria1234", "maria12345", "12345678901", "maria12345678901 v"} {
		got, err := s.Query(ctx, q, "", 20)
		if err != nil && !errors.Is(err, search.ErrQueryTooShort) {
			t.Errorf("Query(%q): %v", q, err)
		}
		if len(got) != 0 {
			t.Errorf("Query(%q) found %+v", q, got)
		}
	}
	// Found by its other words, also when the glued token is typed with them
	// (it is dropped on both sides).
	for _, q := range []string{"comercio varejo", "comercio maria12345678901", "maria12345678901 comercio var"} {
		got := ids(f.query(s, q, "", 20))
		if !slices.Equal(got, []int64{id}) {
			t.Errorf("Query(%q) = %v, want [%d]", q, got, id)
		}
	}
	got := f.query(s, "comercio varejo", "", 20)
	if got[0].Name != "MARIA***456789** COMERCIO VAREJO" {
		t.Errorf("name not masked: %q", got[0].Name)
	}
}

func TestMergedAwayPersonEntityIsNotSearchable(t *testing.T) {
	f := setup(t)
	a := f.entity(claims.KindPerson, "", "HELENA FICTICIA")
	b := f.entity(claims.KindPerson, "", "HELENA FICTICIA")
	s := search.New(f.pool)
	f.rebuild(s)
	if got := ids(f.query(s, "helena ficticia", "", 20)); !slices.Equal(got, []int64{a, b}) {
		t.Fatalf("before merge: %v", got)
	}
	// Identity merges move the records; b is left without any.
	if _, err := f.pool.Exec(ctx, `UPDATE person_record SET entity_id = $1 WHERE entity_id = $2`, a, b); err != nil {
		t.Fatal(err)
	}
	f.rebuild(s)
	if got := ids(f.query(s, "helena ficticia", "", 20)); !slices.Equal(got, []int64{a}) {
		t.Errorf("after merge: %v, want [%d]", got, a)
	}
}

func TestQueryLengthCaps(t *testing.T) {
	f := setup(t)
	id := f.entity(claims.KindCompany, "86000001", "UM DOIS TRES QUATRO CINCO SEIS SETE OITO NOVE DEZ")
	s := search.New(f.pool)
	f.rebuild(s)
	if _, err := s.Query(ctx, strings.Repeat("á", 201), "", 20); !errors.Is(err, search.ErrQueryTooLong) {
		t.Errorf("201 runes: err = %v, want ErrQueryTooLong", err)
	}
	if _, err := s.Query(ctx, strings.Repeat("á", 200), "", 20); err != nil {
		t.Errorf("200 runes: %v", err)
	}
	// Words after the 10th kept one are ignored (ONZE is in no name).
	q := "um dois tres quatro cinco seis sete oito nove dez onze"
	if got := ids(f.query(s, q, "", 20)); !slices.Equal(got, []int64{id}) {
		t.Errorf("11 words: %v, want [%d]", got, id)
	}
	// Stopwords do not count toward the 10.
	q = "um de dois de tres quatro cinco seis sete oito nove dez onze"
	if got := ids(f.query(s, q, "", 20)); !slices.Equal(got, []int64{id}) {
		t.Errorf("11 words with stopwords: %v, want [%d]", got, id)
	}
}

func TestAlphanumericCNPJ(t *testing.T) {
	f := setup(t)
	alnum := f.entity(claims.KindCompany, "AB12CD34", "EMPRESA ALFANUMERICA")
	rota := f.entity(claims.KindCompany, "87000001", "ROTA2024 TRANSPORTES")
	s := search.New(f.pool)
	f.rebuild(s)
	for _, q := range []string{"AB12CD34", "ab12cd34", "AB.12C.D34", "AB12CD34000155", "AB.12C.D34/0001-55"} {
		if got := ids(f.query(s, q, "", 20)); !slices.Equal(got, []int64{alnum}) {
			t.Errorf("Query(%q) = %v, want [%d]", q, got, alnum)
		}
	}
	if got := ids(f.query(s, "AB12CD34", claims.KindCompany, 20)); !slices.Equal(got, []int64{alnum}) {
		t.Errorf("company scope: %v", got)
	}
	// Looks like a CNPJ but is a word of a name: falls through to name search.
	if got := ids(f.query(s, "ROTA2024", "", 20)); !slices.Equal(got, []int64{rota}) {
		t.Errorf("ROTA2024 = %v, want [%d]", got, rota)
	}
	// 14 characters whose last two are not digits are no CNPJ.
	if got := f.query(s, "AB12CD3400X1YZ", "", 20); len(got) != 0 {
		t.Errorf("AB12CD3400X1YZ = %+v", got)
	}
}

func dirty(f *fx, s *search.Service) bool {
	f.t.Helper()
	d, err := s.Dirty(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func TestDirtyState(t *testing.T) {
	f := setup(t)
	s := search.New(f.pool)
	if dirty(f, s) {
		t.Fatal("empty database is dirty")
	}
	if _, rebuilt, err := s.RebuildIfDirty(ctx); err != nil || rebuilt {
		t.Fatalf("clean empty database: rebuilt=%v err=%v", rebuilt, err)
	}
	f.entity(claims.KindCompany, "88000001", "VELA NOVA")
	if !dirty(f, s) {
		t.Fatal("generation 0 with entities is not dirty")
	}
	if _, rebuilt, err := s.RebuildIfDirty(ctx); err != nil || !rebuilt {
		t.Fatalf("never-built tables: rebuilt=%v err=%v", rebuilt, err)
	}
	g := generation(f)
	if dirty(f, s) {
		t.Fatal("dirty right after a rebuild")
	}
	if _, rebuilt, err := s.RebuildIfDirty(ctx); err != nil || rebuilt || generation(f) != g {
		t.Fatalf("clean state: rebuilt=%v err=%v generation %d -> %d", rebuilt, err, g, generation(f))
	}
	if err := s.MarkDirty(ctx); err != nil {
		t.Fatal(err)
	}
	if !dirty(f, s) {
		t.Fatal("not dirty after MarkDirty")
	}
}

func TestCancelledRefreshStaysDirtyAndIsCaughtUp(t *testing.T) {
	f := setup(t)
	f.entity(claims.KindCompany, "89000001", "ANTARES")
	f.rebuild(search.New(f.pool))
	added := f.entity(claims.KindCompany, "89000002", "SIRIUS NOVA")

	cctx, cancel := context.WithCancel(ctx)
	s := search.New(f.pool)
	s.SetBeforeSwap(func(int) { cancel() }) // shutdown with the tables built, before the swap
	_, _, err := s.Refresh(cctx)
	if err == nil {
		t.Fatal("cancelled refresh returned no error")
	}
	// The cancelled connection is gone, and with it the rebuild lock, so its
	// half-built tables may stay until the next rebuild drops them.
	if !dirty(f, s) {
		t.Fatal("not dirty after an interrupted refresh: the update would be lost")
	}
	next := search.New(f.pool) // the next worker start
	if _, rebuilt, err := next.RebuildIfDirty(ctx); err != nil || !rebuilt {
		t.Fatalf("catch-up: rebuilt=%v err=%v", rebuilt, err)
	}
	if got := ids(f.query(next, "sirius", "", 20)); !slices.Equal(got, []int64{added}) {
		t.Errorf("after catch-up: %v", got)
	}
	if l := leftovers(f); len(l) != 0 {
		t.Errorf("leftover tables after catch-up: %v", l)
	}
}

func TestRequestsDuringRebuildCauseExactlyOneMore(t *testing.T) {
	f := setup(t)
	populate(f, 300, 5)
	f.rebuild(search.New(f.pool))
	g0 := generation(f)

	var wg sync.WaitGroup
	results := make(chan bool, 2)
	first := search.New(f.pool)
	first.SetBeforeSwap(func(attempt int) {
		if attempt != 1 {
			return
		}
		// Two collections finish while this rebuild is running: each marks
		// the state dirty and waits for the rebuild lock.
		for range 2 {
			other := search.New(f.pool)
			if err := other.MarkDirty(ctx); err != nil {
				t.Error(err)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, rebuilt, err := other.RebuildIfDirty(ctx)
				if err != nil {
					t.Error(err)
				}
				results <- rebuilt
			}()
		}
		time.Sleep(200 * time.Millisecond) // let both block on the lock
	})
	if _, _, err := first.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(results)
	n := 0
	for r := range results {
		if r {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d extra rebuilds, want exactly 1", n)
	}
	if g := generation(f); g != g0+2 {
		t.Errorf("generation %d, want %d", g, g0+2)
	}
	if dirty(f, first) {
		t.Error("still dirty")
	}
}

func TestSwapWaitsForLongReaderAndRetries(t *testing.T) {
	f := setup(t)
	populate(f, 300, 6)
	f.rebuild(search.New(f.pool))

	// A reader holds ACCESS SHARE on a live table longer than one lock
	// timeout.
	reader, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	if _, err := reader.Exec(ctx, `SELECT count(*) FROM search_company`); err != nil {
		t.Fatal(err)
	}

	q := search.New(f.pool)
	s := search.New(f.pool)
	s.SetSwapRetry(300*time.Millisecond, 5, 50*time.Millisecond)
	var attempts int
	queued := make(chan error, 1)
	s.SetBeforeSwap(func(attempt int) {
		attempts = attempt
		switch attempt {
		case 1:
			// Issued while the swap waits for its lock: it queues behind
			// the swap until the attempt times out, then runs.
			go func() {
				time.Sleep(100 * time.Millisecond)
				_, err := q.Query(ctx, "alfa beta", "", 20)
				queued <- err
			}()
		case 2:
			if err := <-queued; err != nil {
				t.Errorf("query issued during the first swap attempt: %v", err)
			}
			if _, err := q.Query(ctx, "petro", "", 20); err != nil {
				t.Errorf("query between attempts: %v", err)
			}
			if err := reader.Commit(ctx); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := s.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild failed instead of waiting for the reader: %v", err)
	}
	if attempts != 2 {
		t.Errorf("swap attempts = %d, want 2", attempts)
	}
}

func TestSwapGivesUpCleanlyAndStaysDirty(t *testing.T) {
	f := setup(t)
	populate(f, 100, 7)
	f.rebuild(search.New(f.pool))
	reader, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	if _, err := reader.Exec(ctx, `SELECT count(*) FROM search_person`); err != nil {
		t.Fatal(err)
	}
	s := search.New(f.pool)
	s.SetSwapRetry(100*time.Millisecond, 3, 10*time.Millisecond)
	if _, _, err := s.Refresh(ctx); err == nil {
		t.Fatal("swap succeeded while a reader held the table")
	}
	if l := leftovers(f); len(l) != 0 {
		t.Errorf("leftover tables: %v", l)
	}
	if !dirty(f, s) {
		t.Error("not dirty after a failed swap")
	}
}

// sameResults asserts that every query of each group returns the same list:
// right and wrong digits must be indistinguishable.
func sameResults(t *testing.T, f *fx, s *search.Service, groups [][]string) {
	t.Helper()
	for _, g := range groups {
		var first []int64
		for i, q := range g {
			got, err := s.Query(ctx, q, "", 50)
			if err != nil && !errors.Is(err, search.ErrQueryTooShort) {
				t.Fatalf("Query(%q): %v", q, err)
			}
			if i == 0 {
				first = ids(got)
				continue
			}
			if !slices.Equal(ids(got), first) {
				t.Errorf("Query(%q) = %v but Query(%q) = %v: the digits are confirmable", g[0], first, q, ids(got))
			}
		}
	}
}

// TestCPFNeverConfirmedByWords: whatever form a CPF takes in a name (glued
// to letters, separated, split by other words) and in the query, queries
// with the right digits and with wrong digits return the same list, so the
// search never confirms which digits go with a name.
func TestCPFNeverConfirmedByWords(t *testing.T) {
	f := setup(t)
	names := []string{
		"MARIA SILVA 123.456.789-01",
		"MARIA SILVA123.456.789-01",
		"MARIA CPF123.456.789-01",
		"MARIA 123.456.789-01X",
		"MARIA SILVA123456789-01",
		"MARIA 123 456 789 DV 01",
		"MARIA 123.456.789/01",
		"MARIA 1 2 3 4 5 6 7 8 9 0 1",
	}
	var mariaIDs []int64
	for i, n := range names {
		mariaIDs = append(mariaIDs, f.entity(claims.KindCompany, fmt.Sprintf("9000%04d", i), n))
	}
	f.entity(claims.KindPerson, "", "MARIA SILVA")
	joao := f.entity(claims.KindCompany, "90010001", "JOAO 12345678901 ME")
	pedro := f.entity(claims.KindCompany, "90010002", "PEDRO 123 456 789 01")
	s := search.New(f.pool)
	f.rebuild(s)

	sameResults(t, f, s, [][]string{
		{"maria silva123 01", "maria silva124 01", "maria silva123 02"},
		{"maria silva123", "maria silva124"},
		{"maria cpf123 01", "maria cpf124 01", "maria cpf123 02"},
		{"maria 01x", "maria 02x"},
		{"maria 01", "maria 02"},
		{"maria 789 01", "maria 788 01"},
		{"maria 789/01", "maria 788/01"},
		{"maria 1 2 3", "maria 1 2 4"},
		{"maria 9 0 1", "maria 9 0 2"},
		{"maria silva 123.456.789-01", "maria silva 123.456.789-99"},
		{"maria silva123.456.789-01", "maria silva123.456.789-02"},
		{"maria 123.456.789-01x", "maria 123.456.789-02x"},
		{"maria 123 456 789 01", "maria 123 456 789 99"},
		{"maria 123 456 789 dv 01", "maria 123 456 789 dv 02"},
		{"maria 123456789-01", "maria 123456789-99"},
		{"maria 12345678901", "maria 12345678999"},
		{"maria silva 123.456", "maria silva 123.457"},
		{"maria 123", "maria 124"},
		{"123.456.789-01", "123.456.789-99"},
		{"joao 12345678901", "joao 12345678902"},
		{"joao 123.456.789-01", "joao 999.456.789-01"},
		{"joao 123 456 789 01 me", "joao 123 456 789 02 me"},
		{"pedro 123 456 789 01", "pedro 123 456 789 00"},
		{"pedro 123.456.789-01", "pedro 321.456.789-01"},
		{"pedro 456", "pedro 457"},
	})
	// Every such name is found by its digit-free words.
	all := ids(f.query(s, "maria", "", 50))
	for _, id := range mariaIDs {
		if !slices.Contains(all, id) {
			t.Errorf("entity %d not found by \"maria\": %v", id, all)
		}
	}
	for q, id := range map[string]int64{"joao": joao, "pedro": pedro} {
		if got := ids(f.query(s, q, "", 20)); !slices.Equal(got, []int64{id}) {
			t.Errorf("Query(%q) = %v, want [%d]", q, got, id)
		}
	}
	got := f.query(s, "maria silva", claims.KindCompany, 50)
	if !slices.ContainsFunc(got, func(r graph.EntityRef) bool { return r.Name == "MARIA SILVA ***.456.789-**" }) {
		t.Errorf("display name not masked: %+v", got)
	}
}

// TestNameWithLongNumberIsNotFoundByTypingItWhole: the exact tier is skipped
// for a query with 9 or more digits, so the complete name returns what its
// digit-free words return.
func TestNameWithLongNumberIsNotFoundByTypingItWhole(t *testing.T) {
	f := setup(t)
	f.entity(claims.KindCompany, "90020001", "JOAO 12345678901 ME")
	f.entity(claims.KindCompany, "90020002", "JOAO 12345678902 ME")
	f.entity(claims.KindCompany, "90020003", "JOAO 123.456.789-01 ME")
	s := search.New(f.pool)
	f.rebuild(s)
	want := ids(f.query(s, "joao me", "", 50))
	if len(want) != 3 {
		t.Fatalf("joao me = %v", want)
	}
	for _, q := range []string{"JOAO 12345678901 ME", "JOAO 12345678902 ME", "joao 123.456.789-01 me", "JOAO 12345678909 ME"} {
		if got := ids(f.query(s, q, "", 50)); !slices.Equal(got, want) {
			t.Errorf("Query(%q) = %v, want the same as \"joao me\" %v", q, got, want)
		}
	}
}

// TestCPFPropertyRandomSpellings embeds random 11-digit sequences in names
// with random separators and letters glued at random places, and checks
// that probes built from the true digits and from one-digit-off digits
// return the same results, while the name is found by its first word.
func TestCPFPropertyRandomSpellings(t *testing.T) {
	f := setup(t)
	r := rand.New(rand.NewPCG(11, 12))
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	type named struct {
		id    int64
		word  string
		name  string
		probe []string // digit-bearing words of the name, lower case
	}
	var all []named
	for i := range 40 {
		word := "ZQ" + string(letters[i/26%26]) + string(letters[i%26]) + "X"
		var b strings.Builder
		b.WriteString(word + " ")
		if r.IntN(2) == 0 {
			b.WriteString([]string{"SILVA", "CPF", "N", "LOJA "}[r.IntN(4)])
		}
		for k := range 11 {
			b.WriteByte(byte('0' + r.IntN(10)))
			if k == 10 {
				break
			}
			switch x := r.IntN(20); {
			case x < 10:
			case x < 16:
				b.WriteByte(". -/"[r.IntN(4)])
			default:
				b.WriteByte(letters[r.IntN(26)])
			}
		}
		if r.IntN(3) == 0 {
			b.WriteByte(letters[r.IntN(26)])
		}
		b.WriteString(" ME")
		name := b.String()
		n := named{word: strings.ToLower(word), name: name}
		n.id = f.entity(claims.KindCompany, fmt.Sprintf("9100%04d", i), name)
		for _, w := range strings.FieldsFunc(strings.ToLower(name), func(c rune) bool {
			return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9')
		}) {
			if strings.ContainsAny(w, "0123456789") {
				n.probe = append(n.probe, w)
			}
		}
		all = append(all, n)
	}
	s := search.New(f.pool)
	f.rebuild(s)
	offByOne := func(w string) string {
		b := []byte(w)
		for i, c := range b {
			if c >= '0' && c <= '9' {
				b[i] = '0' + (c-'0'+1)%10
				break
			}
		}
		return string(b)
	}
	var groups [][]string
	for _, n := range all {
		if got := ids(f.query(s, n.word, "", 20)); !slices.Equal(got, []int64{n.id}) {
			t.Errorf("%q not found by %q: %v", n.name, n.word, got)
		}
		for i, w := range n.probe {
			groups = append(groups, []string{n.word + " " + w, n.word + " " + offByOne(w)})
			if i+1 < len(n.probe) {
				w2 := n.probe[i+1]
				groups = append(groups, []string{n.word + " " + w + " " + w2, n.word + " " + w + " " + offByOne(w2)})
			}
		}
		groups = append(groups, []string{n.name, offByOne(n.name)})
	}
	sameResults(t, f, s, groups)
}

func TestPunctuatedCNPJInNameIsNotAWord(t *testing.T) {
	f := setup(t)
	acme := f.entity(claims.KindCompany, "12345678", "ACME 12.345.678/0001-90 LTDA")
	s := search.New(f.pool)
	f.rebuild(s)
	for _, q := range []string{"acme", "12345678", "12.345.678/0001-90"} {
		if got := ids(f.query(s, q, "", 20)); !slices.Equal(got, []int64{acme}) {
			t.Errorf("Query(%q) = %v, want [%d]", q, got, acme)
		}
	}
	if got := f.query(s, "acme 345 678", "", 20); len(got) != 0 {
		t.Errorf("CNPJ digit groups are searchable: %v", ids(got))
	}
}

func TestShortNumbersInNamesStaySearchable(t *testing.T) {
	f := setup(t)
	posto := f.entity(claims.KindCompany, "91000001", "POSTO 15")
	loja := f.entity(claims.KindCompany, "91000002", "LOJA 24 HORAS")
	rua := f.entity(claims.KindCompany, "91000003", "BAR RUA 7 123")
	b3 := f.entity(claims.KindCompany, "91000004", "B3 BOLSA")
	f.entity(claims.KindCompany, "91000005", "B4 BOLSA")
	c6 := f.entity(claims.KindCompany, "91000006", "C6 BANK")
	f.entity(claims.KindCompany, "91000007", "C7 BANK")
	taxi := f.entity(claims.KindCompany, "91000008", "99 TAXIS")
	f.entity(claims.KindCompany, "91000009", "98 TAXIS")
	eight := f.entity(claims.KindCompany, "91000010", "LOJA 1234 5678 CENTRO") // 8 digits in total
	s := search.New(f.pool)
	f.rebuild(s)
	for q, want := range map[string]int64{"posto 15": posto, "loja 24 horas": loja, "loja 24": loja, "bar rua 7 123": rua,
		"rua 7 123": rua, "b3 bolsa": b3, "c6 bank": c6, "99 taxis": taxi, "loja 1234 5678": eight} {
		if got := ids(f.query(s, q, "", 20)); !slices.Equal(got, []int64{want}) {
			t.Errorf("Query(%q) = %v, want [%d]", q, got, want)
		}
	}
}

func TestMarkDirtySurvivesCancelledContext(t *testing.T) {
	f := setup(t)
	s := search.New(f.pool)
	f.rebuild(s)
	cctx, cancel := context.WithCancel(ctx)
	cancel() // shutdown right after the collection finished
	if err := s.MarkDirty(cctx); err != nil {
		t.Fatalf("MarkDirty with a cancelled context: %v", err)
	}
	if !dirty(f, s) {
		t.Fatal("mark lost")
	}
}

func TestMarkIsMonotonic(t *testing.T) {
	f := setup(t)
	s := search.New(f.pool)
	f.rebuild(s)
	// A clock that went back: the last rebuild looks like it came from the future.
	if _, err := f.pool.Exec(ctx, `UPDATE search_state SET rebuilt_from = now() + interval '1 day',
		requested_at = now() + interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	if dirty(f, s) {
		t.Fatal("dirty before the mark")
	}
	if err := s.MarkDirty(ctx); err != nil {
		t.Fatal(err)
	}
	if !dirty(f, s) {
		t.Fatal("a mark made with an earlier clock was taken as already covered")
	}
}

func TestLostMarkIsRecoveredDespiteAClockStep(t *testing.T) {
	f := setup(t)
	s := search.New(f.pool)
	f.entity(claims.KindCompany, "93000001", "CAPELLA")
	f.rebuild(s)
	if err := collection.Finish(ctx, f.pool, f.col, nil); err != nil {
		t.Fatal(err)
	}
	// The clock stepped back: the collection looks finished long before the
	// last rebuild.
	if _, err := f.pool.Exec(ctx, `UPDATE collection SET finished_at = '2000-01-01' WHERE id = $1`, f.col); err != nil {
		t.Fatal(err)
	}
	if !dirty(f, s) {
		t.Fatal("a finished collection with an earlier timestamp was taken as covered")
	}
}

func TestLostMarkIsRecoveredFromFinishedCollections(t *testing.T) {
	f := setup(t)
	s := search.New(f.pool)
	f.entity(claims.KindCompany, "92000001", "CANOPUS")
	f.rebuild(s)
	if dirty(f, s) {
		t.Fatal("dirty after rebuild")
	}
	// A collection finishes but its mark never gets written.
	added := f.entity(claims.KindCompany, "92000002", "ALDEBARAN")
	if err := collection.Finish(ctx, f.pool, f.col, nil); err != nil {
		t.Fatal(err)
	}
	if !dirty(f, s) {
		t.Fatal("a done collection after the last rebuild does not make the state dirty")
	}
	if _, rebuilt, err := search.New(f.pool).RebuildIfDirty(ctx); err != nil || !rebuilt {
		t.Fatalf("catch-up: rebuilt=%v err=%v", rebuilt, err)
	}
	if got := ids(f.query(s, "aldebaran", "", 20)); !slices.Equal(got, []int64{added}) {
		t.Errorf("after catch-up: %v", got)
	}
	if dirty(f, s) {
		t.Error("still dirty after catch-up")
	}
}
