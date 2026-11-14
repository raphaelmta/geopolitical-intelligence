package db_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"geoint/internal/db"
	"geoint/internal/testutil"
)

// pinnedTriggerFunctions: the functions 0023 runs with search_path =
// pg_catalog, pg_temp; "unpinned" variants reset them in the transaction.
var pinnedTriggerFunctions = []string{"forbid_delete()", "forbid_update()", "guard_update()",
	"guard_proposal_update()", "guard_collection_status()", "guard_entity_update()"}

// BenchmarkUpdateGuard measures what the update guards cost the statements
// of a load, per row, on 200 k rows:
//
//   - "seen": UPDATE … SET last_seen_collection_id to the next collection on
//     every row of relationship and company_version, with the original guard
//     of migration 0001 (put back in the transaction), the installed guard,
//     and no guard (the trigger disabled in the transaction); the original
//     guard also without the pinned search_path;
//   - "insert": a bulk INSERT … SELECT into relationship (no row trigger
//     fires on INSERT), with and without the pinned search_path;
//   - "proposal": UPDATE identity_proposal SET connections (Resolve's refresh;
//     guard_proposal_update runs for every row), with and without it.
//
// Every statement is rolled back and the table vacuumed before the next
// one; only the statement is timed.
//
//	make test TEST_PROJECT=perf-t4 PKG='-bench BenchmarkUpdateGuard -benchtime 5x ./internal/db' RUN=NONE
//
// us/row is the whole statement per row; a guard's cost is the difference
// with "none".
func BenchmarkUpdateGuard(b *testing.B) {
	const rows = 200_000
	u := testutil.EmptySchemaURL(b)
	if err := db.Migrate(roleCtx, u); err != nil {
		b.Fatal(err)
	}
	pool, err := db.Open(roleCtx, u)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(pool.Close)
	testutil.SeedSources(b, pool)
	var c1, c2 int64
	for i, c := range []*int64{&c1, &c2} {
		if err := pool.QueryRow(roleCtx, `INSERT INTO collection (source_id, reference_date, observed_date)
			VALUES ((SELECT id FROM source WHERE key = 'receita'), $1::date, $1::date) RETURNING id`,
			fmt.Sprintf("2026-0%d-01", i+1)).Scan(c); err != nil {
			b.Fatal(err)
		}
	}
	seed := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO entity (kind, natural_key, name, name_norm)
			SELECT 'company', lpad(g::text, 8, '0'), 'EMPRESA ' || g, 'EMPRESA ' || g FROM generate_series(1, $1) g`,
			[]any{rows}},
		{`INSERT INTO company_version (entity_id, name, legal_nature, size, uf, city, cnae, status, capital,
				first_collection_id, record_ref, last_seen_collection_id)
			SELECT e.id, e.name, '2062', '03', 'SP', '7107', '6201501', '02', 1000, $2, 'cv-' || e.id, $2
			FROM entity e WHERE e.id <= $1`, []any{rows, c1}},
		{`INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, attrs, kind, valid_from,
				first_collection_id, record_ref, last_seen_collection_id)
			SELECT 'partner_of', g, (g % $1) + 1, jsonb_build_object('role', '49'), 'fact', date '2010-01-01',
				$2, 'socio-' || g, $2
			FROM generate_series(1, $1) g`, []any{rows, c1}},
		{`INSERT INTO identity_proposal (group_key, record_ids, reason)
			SELECT 'NOME ' || g || '|123456', ARRAY[g, g + 1]::bigint[], 'common name' FROM generate_series(1, $1) g`,
			[]any{rows}},
	}
	for _, s := range seed {
		if _, err := pool.Exec(roleCtx, s.sql, s.args...); err != nil {
			b.Fatalf("%s: %v", s.sql, err)
		}
	}
	unpin := make([]string, len(pinnedTriggerFunctions))
	for i, f := range pinnedTriggerFunctions {
		unpin[i] = "ALTER FUNCTION " + f + " RESET search_path"
	}
	seen := func(table string) string {
		return fmt.Sprintf("UPDATE %s SET last_seen_collection_id = %d", table, c2)
	}
	insert := fmt.Sprintf(`INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, attrs, kind, valid_from,
			first_collection_id, record_ref, last_seen_collection_id)
		SELECT 'controls', g, (g %% %[1]d) + 1, '{}', 'fact', NULL, %[2]d, 'ctl-' || g, %[2]d
		FROM generate_series(1, %[1]d) g`, rows, c2)
	proposal := "UPDATE identity_proposal SET connections = connections + 1"
	type variant struct {
		name, table string
		setup       []string
		stmt        string
	}
	var variants []variant
	for _, table := range []string{"relationship", "company_version"} {
		variants = append(variants,
			variant{table + "/original", table, []string{originalGuard(table)}, seen(table)},
			variant{table + "/original-unpinned", table, append([]string{originalGuard(table)}, unpin...), seen(table)},
			variant{table + "/installed", table, nil, seen(table)},
			variant{table + "/installed-unpinned", table, unpin, seen(table)},
			variant{table + "/none", table, []string{"ALTER TABLE " + table + " DISABLE TRIGGER " + table + "_guard_update"}, seen(table)})
	}
	variants = append(variants,
		variant{"insert/pinned", "relationship", nil, insert},
		variant{"insert/unpinned", "relationship", unpin, insert},
		variant{"proposal/pinned", "identity_proposal", nil, proposal},
		variant{"proposal/unpinned", "identity_proposal", unpin, proposal},
		variant{"proposal/none", "identity_proposal", []string{"ALTER TABLE identity_proposal DISABLE TRIGGER identity_proposal_guard_update"}, proposal})
	for _, table := range []string{"relationship", "company_version", "identity_proposal"} {
		if _, err := pool.Exec(roleCtx, "VACUUM (ANALYZE) "+table); err != nil {
			b.Fatal(err)
		}
	}
	for _, v := range variants {
		b.Run(v.name, func(b *testing.B) {
			var spent time.Duration
			for range b.N {
				tx, err := pool.Begin(roleCtx)
				if err != nil {
					b.Fatal(err)
				}
				for _, s := range v.setup {
					if _, err := tx.Exec(roleCtx, s); err != nil {
						b.Fatalf("%s: %v", s, err)
					}
				}
				start := time.Now()
				tag, err := tx.Exec(roleCtx, v.stmt)
				spent += time.Since(start)
				if err != nil || tag.RowsAffected() != rows {
					b.Fatalf("%s: %v (%d rows)", strings.Fields(v.stmt)[0], err, tag.RowsAffected())
				}
				if err := tx.Rollback(roleCtx); err != nil {
					b.Fatal(err)
				}
				if _, err := pool.Exec(roleCtx, "VACUUM "+v.table); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(spent.Microseconds())/float64(b.N*rows), "us/row")
		})
	}
}
