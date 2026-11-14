package db_test

import (
	"fmt"
	"testing"
	"time"

	"geoint/internal/db"
	"geoint/internal/testutil"
)

// BenchmarkUpdateGuard measures what the update guard costs a "seen" update
// of the monthly re-load (last_seen_collection_id moved forward on every
// row, the statement of CloseUnseen's counterpart in the loaders) on
// relationship and company_version, with the original guard of migration
// 0001 (put back in the transaction), the installed guard, and no guard
// (the trigger disabled in the transaction). Every UPDATE is rolled back
// and the table vacuumed before the next one; only the UPDATE is timed.
//
//	make test TEST_PROJECT=perf-t4 PKG='-bench BenchmarkUpdateGuard -benchtime 5x ./internal/db' RUN=NONE
//
// us/row is the whole UPDATE per row; the guard's cost is the difference
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
	for _, sql := range []string{
		`INSERT INTO entity (kind, natural_key, name, name_norm)
			SELECT 'company', lpad(g::text, 8, '0'), 'EMPRESA ' || g, 'EMPRESA ' || g FROM generate_series(1, $1) g
			WHERE $2::bigint IS NOT NULL`,
		`INSERT INTO company_version (entity_id, name, legal_nature, size, uf, city, cnae, status, capital,
				first_collection_id, record_ref, last_seen_collection_id)
			SELECT e.id, e.name, '2062', '03', 'SP', '7107', '6201501', '02', 1000, $2, 'cv-' || e.id, $2
			FROM entity e WHERE e.id <= $1`,
		`INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, attrs, kind, valid_from,
				first_collection_id, record_ref, last_seen_collection_id)
			SELECT 'partner_of', g, (g % $1) + 1, jsonb_build_object('role', '49'), 'fact', date '2010-01-01',
				$2, 'socio-' || g, $2
			FROM generate_series(1, $1) g`,
	} {
		if _, err := pool.Exec(roleCtx, sql, rows, c1); err != nil {
			b.Fatalf("%s: %v", sql, err)
		}
	}
	for _, table := range []string{"relationship", "company_version"} {
		if _, err := pool.Exec(roleCtx, "VACUUM (ANALYZE) "+table); err != nil {
			b.Fatal(err)
		}
		for _, variant := range []string{"original", "installed", "none"} {
			b.Run(table+"/"+variant, func(b *testing.B) {
				var spent time.Duration
				for range b.N {
					tx, err := pool.Begin(roleCtx)
					if err != nil {
						b.Fatal(err)
					}
					switch variant {
					case "original":
						_, err = tx.Exec(roleCtx, originalGuard(table))
					case "none":
						_, err = tx.Exec(roleCtx, "ALTER TABLE "+table+" DISABLE TRIGGER "+table+"_guard_update")
					}
					if err != nil {
						b.Fatal(err)
					}
					start := time.Now()
					tag, err := tx.Exec(roleCtx, fmt.Sprintf("UPDATE %s SET last_seen_collection_id = %d", table, c2))
					spent += time.Since(start)
					if err != nil || tag.RowsAffected() != rows {
						b.Fatalf("update: %v (%d rows)", err, tag.RowsAffected())
					}
					if err := tx.Rollback(roleCtx); err != nil {
						b.Fatal(err)
					}
					if _, err := pool.Exec(roleCtx, "VACUUM "+table); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(spent.Microseconds())/float64(b.N*rows), "us/row")
			})
		}
	}
}
