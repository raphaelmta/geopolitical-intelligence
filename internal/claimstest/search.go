package claimstest

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/search"
)

func sortStrings(s []string) { sort.Strings(s) }

// searchQueries dump the live search tables row for row, entity ids replaced
// by the entities' identities (c_ent). search_rank is kept: it is the order
// queries rely on.
var searchQueries = map[string]string{
	"search_company": `SELECT ` + line("s.search_rank", "ce.k", "s.name_norm", "s.grau", "s.tokens") + `
		FROM search_company s LEFT JOIN c_ent ce ON ce.id = s.entity_id`,
	"search_person": `SELECT ` + line("s.search_rank", "ce.k", "s.kind", "s.name_norm", "s.grau", "s.tokens") + `
		FROM search_person s LEFT JOIN c_ent ce ON ce.id = s.entity_id`,
	"search_other": `SELECT ` + line("s.search_rank", "ce.k", "s.kind", "s.name_norm", "s.grau", "s.tokens") + `
		FROM search_other s LEFT JOIN c_ent ce ON ce.id = s.entity_id`,
	"search_frequent_token": `SELECT ` + line("token", "ndoc") + ` FROM search_frequent_token`,
}

// SearchTables returns the live search tables as sorted text lines, one
// section per table, without surrogate ids. It also checks that each ranked
// table is stored in search_rank order (queries rely on the physical order).
func SearchTables(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	out := map[string]string{}
	err := withCanon(ctx, pool, func(tx pgx.Tx) error {
		for name, q := range searchQueries {
			lines, err := queryLines(ctx, tx, q)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			sortStrings(lines)
			out[name] = joinLines(lines)
		}
		for _, table := range []string{"search_company", "search_person", "search_other"} {
			// Only the order of the tables' physical rows is checked here;
			// ctid order is the heap order of a table written once.
			rows, err := tx.Query(ctx, `SELECT search_rank FROM `+table+` ORDER BY ctid`)
			if err != nil {
				return err
			}
			ranks, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil {
				return err
			}
			if !slices.IsSorted(ranks) {
				return fmt.Errorf("%s is not stored in search_rank order", table)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claimstest: search tables: %w", err)
	}
	return out, nil
}

// CheckSearchMatchesRebuild checks that the live search tables are, row for
// row, what a from-scratch rebuild of the same data produces. The reference
// build starts from EMPTY live tables (they are truncated first) and with no
// stored build version, so a rebuild that reuses what the live tables
// already hold (task 5) has nothing to reuse, may not reuse, and computes
// everything. The live tables are left as the reference build made them.
func CheckSearchMatchesRebuild(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	live, err := SearchTables(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	liveRows, err := SearchRows(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE search_company, search_person, search_other, search_frequent_token;
		UPDATE search_state SET build_version = NULL`); err != nil {
		t.Fatal(err)
	}
	st, err := search.New(pool).Rebuild(ctx)
	if err != nil {
		t.Fatalf("from-scratch search rebuild: %v", err)
	}
	if st.Reused {
		t.Fatal("the from-scratch search rebuild reused the live tables")
	}
	ref, err := SearchTables(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(ref, live); d != "" {
		t.Errorf("live search tables differ from a from-scratch rebuild (- rebuild, + live):%s", d)
	}
	refRows, err := SearchRows(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(refRows, liveRows); d != "" {
		t.Errorf("live search tables differ from a from-scratch rebuild in their stored rows (- rebuild, + live):%s", d)
	}
}

// SearchRows returns the live search tables exactly as stored: every column,
// entity ids included, one line per row in physical (ctid) order. Within one
// database it is the strictest comparison: same rows, same ids, same order.
func SearchRows(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	out := map[string]string{}
	for _, q := range []struct{ name, sql string }{
		{"search_company", `SELECT concat_ws(E'\t', search_rank, entity_id, name_norm, grau, tokens) FROM search_company ORDER BY ctid`},
		{"search_person", `SELECT concat_ws(E'\t', search_rank, entity_id, kind, name_norm, grau, tokens) FROM search_person ORDER BY ctid`},
		{"search_other", `SELECT concat_ws(E'\t', search_rank, entity_id, kind, name_norm, grau, tokens) FROM search_other ORDER BY ctid`},
		{"search_frequent_token", `SELECT concat_ws(E'\t', token, ndoc) FROM search_frequent_token ORDER BY token`},
	} {
		rows, err := pool.Query(ctx, q.sql)
		if err != nil {
			return nil, fmt.Errorf("claimstest: %s: %w", q.name, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("claimstest: %s: %w", q.name, err)
		}
		out[q.name+" (stored)"] = joinLines(lines)
	}
	return out, nil
}

// searchGoldenQuery renders the live search tables for the golden file,
// without anything that depends on surrogate ids. search_rank breaks ties by
// entity id (ORDER BY grau DESC, name_norm, id), so it is replaced by the
// rank of the row's tie group: the first rank of all rows, over the three
// ranked tables, with the same grau and name_norm, and the size of that
// group. Both are id-independent and pin today's order of the groups.
const searchGoldenQuery = `WITH s AS (
		SELECT 'search_company' AS t, search_rank, entity_id, 'C'::"char" AS kind, name_norm, grau, tokens FROM search_company
		UNION ALL SELECT 'search_person', search_rank, entity_id, kind, name_norm, grau, tokens FROM search_person
		UNION ALL SELECT 'search_other', search_rank, entity_id, kind, name_norm, grau, tokens FROM search_other
	), g AS (
		SELECT s.*, min(search_rank) OVER w AS tie_first, count(*) OVER w AS tie_size
		FROM s WINDOW w AS (PARTITION BY grau, name_norm)
	)
	SELECT g.t, 'tie ' || g.tie_first || '+' || g.tie_size || E'\t' || coalesce(ce.k, '~') || E'\t' || g.kind::text
		|| E'\t' || coalesce(g.name_norm, '~') || E'\t' || g.grau || E'\t' || coalesce(g.tokens::text, '~')
	FROM g LEFT JOIN c_ent ce ON ce.id = g.entity_id
	UNION ALL
	SELECT 'search_frequent_token', token || E'\t' || coalesce(ndoc::text, '~') FROM search_frequent_token`

// SearchGolden returns the live search tables as golden sections (one per
// table, named after it) in the id-independent form of searchGoldenQuery.
func SearchGolden(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	out := map[string]string{"search_company": "", "search_person": "", "search_other": "", "search_frequent_token": ""}
	err := withCanon(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, searchGoldenQuery)
		if err != nil {
			return err
		}
		lines := map[string][]string{}
		for rows.Next() {
			var t, l string
			if err := rows.Scan(&t, &l); err != nil {
				return err
			}
			lines[t] = append(lines[t], l)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for t, ls := range lines {
			sortStrings(ls)
			out[t] = joinLines(ls)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claimstest: search golden: %w", err)
	}
	return out, nil
}
