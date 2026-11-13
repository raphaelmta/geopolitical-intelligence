package tse

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestReceiptLookupsUseTheIndex checks that the receipt-conflict check can
// probe relationship_open_receipt (migration 0016) with both of its columns
// as index conditions. Tables are tiny here, so
// sequential scans are disabled to see which index the planner can use.
func TestReceiptLookupsUseTheIndex(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, sql string
		args      []any
	}{
		{"receipt conflicts", sqlReceiptConflicts, []any{sourceID}},
	}
	for _, c := range cases {
		var plan []string
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, "EXPLAIN "+c.sql, c.args...)
			if err != nil {
				return err
			}
			plan, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		text := strings.Join(plan, "\n")
		i := strings.Index(text, "relationship_open_receipt")
		if i < 0 {
			t.Errorf("%s: plan does not use relationship_open_receipt:\n%s", c.name, text)
			continue
		}
		if cond := text[i:]; !strings.Contains(cond, "Index Cond: ((dst_record_id = ") || !strings.Contains(cond, "->> 'role'") {
			t.Errorf("%s: dst_record_id and role are not both index conditions:\n%s", c.name, text)
		}
	}
}
