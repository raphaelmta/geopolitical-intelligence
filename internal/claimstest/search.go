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
// build starts from EMPTY live tables (they are truncated first), so a
// rebuild that reuses what the live tables already hold (task 5) has nothing
// to reuse and computes everything. The live tables are left as the
// reference build made them.
func CheckSearchMatchesRebuild(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	live, err := SearchTables(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE search_company, search_person, search_other, search_frequent_token`); err != nil {
		t.Fatal(err)
	}
	if _, err := search.New(pool).Rebuild(ctx); err != nil {
		t.Fatalf("from-scratch search rebuild: %v", err)
	}
	ref, err := SearchTables(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(ref, live); d != "" {
		t.Errorf("live search tables differ from a from-scratch rebuild (- rebuild, + live):%s", d)
	}
}
