package graph

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"geoint/internal/testutil"
)

// TestIdentityBasisPlanUsesIndexes checks that the per-page identity basis
// aggregate reads person_record by its primary key and by entity_id, never
// by a scan (sequential scans disabled: the tables are tiny here).
func TestIdentityBasisPlanUsesIndexes(t *testing.T) {
	t.Parallel()
	pool := testutil.DB(t)
	ctx := context.Background()
	var plan []string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "EXPLAIN "+identityBasisSQL, []int64{1, 2, 3})
		if err != nil {
			return err
		}
		plan, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	for _, idx := range []string{"person_record_pkey", "person_record_entity_id"} {
		if !strings.Contains(text, idx) {
			t.Errorf("plan does not use %s:\n%s", idx, text)
		}
	}
	if strings.Contains(text, "Seq Scan") {
		t.Errorf("plan scans a table:\n%s", text)
	}
}
