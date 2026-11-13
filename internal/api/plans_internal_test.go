package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"geoint/internal/testutil"
)

// TestQueryPlansUseIndexes checks that the entity page, provenance and
// proposal queries can be answered from their indexes (and, where noSort,
// in index order). Tables are tiny here, so sequential scans are disabled to
// see which index the planner can use.
func TestQueryPlansUseIndexes(t *testing.T) {
	t.Parallel()
	pool := testutil.DB(t)
	ctx := context.Background()
	pendingFirst, pendingFirstArgs := proposalsSQL("pending", nil, 51)
	pendingNext, pendingNextArgs := proposalsSQL("pending", &proposalCursor{connections: 3, id: 9}, 51)
	otherNext, otherNextArgs := proposalsSQL("confirmed", &proposalCursor{connections: 3, id: 9}, 51)
	cases := []struct {
		name, sql string
		args      []any
		index     []string
		// cond: index → a substring its Index Cond must contain.
		cond   map[string]string
		noSort bool
	}{
		{"versions", versionsSQL, []any{int64(1), maxVersions}, []string{"company_version_entity_id", "collection_pkey"},
			map[string]string{"company_version_entity_id": "entity_id = "}, true},
		// Records are few per entity: read by entity_id, then sorted by id.
		{"records", entityRecordsSQL, []any{int64(1), maxRecords + 1}, []string{"person_record_entity_id", "collection_pkey"},
			map[string]string{"person_record_entity_id": "entity_id = "}, false},
		{"provenance", provenanceSQL, []any{int64(1), time.Now()},
			[]string{"relationship_pkey", "collection_pkey", "collection_file_pkey"},
			map[string]string{"relationship_pkey": "id = ", "collection_file_pkey": "collection_id = r.first_collection_id"}, false},
		{"pending first page", pendingFirst, pendingFirstArgs, []string{"identity_proposal_pending_queue"}, nil, true},
		{"pending next page", pendingNext, pendingNextArgs, []string{"identity_proposal_pending_queue"},
			map[string]string{"identity_proposal_pending_queue": "connections <= "}, true},
		{"confirmed next page", otherNext, otherNextArgs, []string{"identity_proposal_status_queue"},
			map[string]string{"identity_proposal_status_queue": "connections <= "}, true},
	}
	for _, c := range cases {
		var plan []string
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
				return err
			}
			// With sorts discouraged too, a Sort left in the plan means no
			// index can deliver the order (the tables are empty here, so the
			// planner would otherwise happily sort two rows).
			if c.noSort {
				if _, err := tx.Exec(ctx, `SET LOCAL enable_sort = off`); err != nil {
					return err
				}
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
		t.Logf("%s:\n%s", c.name, text)
		for _, idx := range c.index {
			if !strings.Contains(text, idx) {
				t.Errorf("%s: plan does not use %s:\n%s", c.name, idx, text)
			}
		}
		for idx, want := range c.cond {
			if got := indexCond(plan, idx); !strings.Contains(got, want) {
				t.Errorf("%s: Index Cond of %s is %q, want it to contain %q:\n%s", c.name, idx, got, want, text)
			}
		}
		if c.noSort && strings.Contains(text, "Sort") {
			t.Errorf("%s: sorts instead of reading the index in order:\n%s", c.name, text)
		}
	}
}

// indexCond returns the Index Cond line of the plan node that uses idx
// ("" when that node has none).
func indexCond(plan []string, idx string) string {
	for i, line := range plan {
		if !strings.Contains(line, " using "+idx+" ") && !strings.Contains(line, " on "+idx+" ") {
			continue
		}
		for _, next := range plan[i+1:] {
			if strings.Contains(next, "->") {
				break
			}
			if strings.Contains(next, "Index Cond:") {
				return strings.TrimSpace(next)
			}
		}
	}
	return ""
}
