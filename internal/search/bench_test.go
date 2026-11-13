package search_test

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"geoint/internal/claims"
	"geoint/internal/search"
)

// TestSearchPerformanceSanity is a throwaway sanity run, not the national
// measurement: it builds SEARCH_BENCH_N (default 1,000,000) synthetic
// entities, rebuilds and times a mix of 30 queries in 5 scopes (all,
// company, person, foreign, party; the last two live in search_other). It only runs
// with SEARCH_BENCH=1, for example:
//
//	docker compose ... run --rm -e SEARCH_BENCH=1 go go test -v -run TestSearchPerformanceSanity -timeout 60m ./internal/search
func TestSearchPerformanceSanity(t *testing.T) {
	if os.Getenv("SEARCH_BENCH") != "1" {
		t.Skip("set SEARCH_BENCH=1 to run")
	}
	n := 1_000_000
	if v := os.Getenv("SEARCH_BENCH_N"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil {
			t.Fatal(err)
		}
	}
	f := setup(t)
	start := time.Now()
	generate(f, n)
	t.Logf("generated %d entities in %v", n, time.Since(start).Round(time.Millisecond))

	// Threshold scaled from the national one (100,000 of 88.8 M names).
	threshold := max(int64(n)*100000/88_800_000, 10)
	s := search.NewForTest(f.pool, threshold, 30*time.Second)
	st := f.rebuild(s)
	t.Logf("rebuild: %v, entities %v, frequent tokens %d (threshold %d)", st.Duration.Round(time.Millisecond),
		st.Entities, st.FrequentTokens, threshold)

	queries := []string{
		"MARIA", "JOSE DA SILVA", "SILVA", "SANTOS", "OLIVEIRA SANT", "JOSE DA SIL", "ANA PAULA", "COMERCIO",
		"COMERCIO DE ALIM", "SERVICOS", "COMERCIO E SERVICOS", "CONSTRUT", "TRANSPORTES", "PARTICIPACOES",
		"HOLDING", "MARIA APARECIDA DA S", "FRANCISCO DAS CHA", "BALOTA", "BALOTA KE", "TIRAMU", "KEMONI SAPIRA",
		"PETROB", "ASSOCIACAO DE MOR", "LTDA COMERCIO", "JOAO", "SOUZA PEREIRA", "00012345", "0001", "123",
		"MARIA JOSE SILVA SANTOS OLIVEIRA",
	}
	scopes := []claims.EntityKind{"", claims.KindCompany, claims.KindPerson, claims.KindForeign, claims.KindParty}
	var all []time.Duration
	byScope := map[claims.EntityKind][]time.Duration{}
	for pass := range 2 {
		var times []time.Duration
		var slowest string
		var slowestD time.Duration
		for _, q := range queries {
			for _, k := range scopes {
				t0 := time.Now()
				if _, err := s.Query(ctx, q, k, 20); err != nil {
					t.Fatalf("Query(%q, %q): %v", q, k, err)
				}
				d := time.Since(t0)
				times = append(times, d)
				byScope[k] = append(byScope[k], d)
				if d > slowestD {
					slowestD, slowest = d, fmt.Sprintf("%q/%q", q, k)
				}
			}
		}
		p50, p95, mx := percentiles(times)
		t.Logf("pass %d: %d queries p50 %v p95 %v max %v (slowest %s)", pass+1, len(times), p50, p95, mx, slowest)
		all = append(all, times...)
	}
	p50, p95, mx := percentiles(all)
	t.Logf("both passes: p50 %v p95 %v max %v", p50, p95, mx)
	for _, k := range scopes {
		p50, p95, mx := percentiles(byScope[k])
		t.Logf("scope %q: p50 %v p95 %v max %v", k, p50, p95, mx)
	}

	if os.Getenv("SEARCH_BENCH_REF") == "0" {
		return // the reference comparisons below take about 20 min at 1 M
	}
	// The physical-order trick at this size: compare a few word queries with
	// the fully sorted reference.
	for _, rq := range []struct{ q, last string }{{"OLIVEIRA SANT", "sant"}, {"JOSE DA SIL", "sil"}, {"BALOTA KE", "ke"}, {"CONSTRUT", "construt"}} {
		words := strings.Fields(strings.ToLower(rq.q))
		var parts []string
		for _, w := range words {
			if w != "da" && w != "de" {
				parts = append(parts, "'"+w+"'")
			}
		}
		var frequent bool
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM search_frequent_token WHERE token = $1)`, rq.last).Scan(&frequent); err != nil {
			t.Fatal(err)
		}
		if len(rq.last) >= 3 && !frequent {
			parts[len(parts)-1] += ":*"
		}
		tsq := strings.Join(parts, " & ")
		got := ids(f.query(s, rq.q, "", 20))
		want := reference(f, rq.q, tsq, "", 20)
		if !slices.Equal(got, want) {
			t.Errorf("%q (%s): got %v, want %v", rq.q, tsq, got, want)
		}
	}
}

func percentiles(ds []time.Duration) (p50, p95, mx time.Duration) {
	s := slices.Clone(ds)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s))))] }
	return at(0.50).Round(10 * time.Microsecond), at(0.95).Round(10 * time.Microsecond), s[len(s)-1].Round(10 * time.Microsecond)
}

// generate inserts n synthetic entities (70% companies, 26% persons, 3%
// foreign, 1% parties) with skewed common words and syllable words, and
// about 1.5 relationships per entity with hubs among the first ids.
func generate(f *fx, n int) {
	f.t.Helper()
	stmts := []string{
		`INSERT INTO entity (kind, natural_key, name)
		SELECT k, CASE WHEN k = 'company' THEN lpad(g::text, 8, '0') WHEN k IN ('foreign', 'party') THEN k || g END, nm
		FROM (
			SELECT g,
				CASE WHEN g % 100 < 70 THEN 'company' WHEN g % 100 < 96 THEN 'person' WHEN g % 100 < 99 THEN 'foreign' ELSE 'party' END AS k,
				array_to_string(ARRAY(
					SELECT CASE WHEN random() < 0.6
						THEN (ARRAY['MARIA','JOSE','SILVA','SANTOS','OLIVEIRA','SOUZA','PEREIRA','LIMA','ANA','PAULA',
							'JOAO','FRANCISCO','CARLOS','APARECIDA','DA','DE','DOS','COMERCIO','SERVICOS','LTDA','ME',
							'TRANSPORTES','CONSTRUTORA','PARTICIPACOES','HOLDING','ALIMENTOS','ASSOCIACAO','MORADORES',
							'E','CHAGAS','EIRELI','AGROPECUARIA','IGREJA','PETROBRAS','INDUSTRIA','DISTRIBUIDORA'])
							[1 + floor(power(random(), 2.5) * 36)::int]
						ELSE (ARRAY['BA','LO','TA','KE','MO','NI','SA','PI','RA','TI','RU','MU','VE','DO','GA','LU','ZE','CO','FI','NA'])
							[1 + floor(random() * 20)::int]
						  || (ARRAY['BA','LO','TA','KE','MO','NI','SA','PI','RA','TI','RU','MU','VE','DO','GA','LU','ZE','CO','FI','NA'])
							[1 + floor(random() * 20)::int]
						  || CASE WHEN random() < 0.7 THEN (ARRAY['BA','LO','TA','KE','MO','NI','SA','PI','RA','TI','RU','MU','VE','DO','GA','LU','ZE','CO','FI','NA'])
							[1 + floor(random() * 20)::int] ELSE '' END
						END
					FROM generate_series(1, 2 + (g * 7 + 3) % 4) AS w(i)), ' ') AS nm
			FROM generate_series(1, $1::int) AS g) x`,
		`INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, attrs, kind, first_collection_id, record_ref)
		SELECT 'partner_of', src, dst, jsonb_build_object('role', i), 'fact', $1, 'bench:' || i
		FROM (
			SELECT i, (SELECT min(id) FROM entity) + floor(random() * $2)::bigint AS src,
				(SELECT min(id) FROM entity) + floor(power(random(), 3) * $2)::bigint AS dst
			FROM generate_series(1, ($2 * 3 / 2)::int) AS i) r
		WHERE src <> dst`,
		`ANALYZE entity`,
		`ANALYZE relationship`,
	}
	args := [][]any{{n}, {f.col, n}, nil, nil}
	for i, sql := range stmts {
		if _, err := f.pool.Exec(ctx, sql, args[i]...); err != nil {
			f.t.Fatalf("generate step %d: %v", i, err)
		}
		if i == 0 {
			f.recordPersons()
		}
	}
}
