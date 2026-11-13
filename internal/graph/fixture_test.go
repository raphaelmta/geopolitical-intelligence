package graph_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/graph/graphtest"
)

var ctx = context.Background()

// The test graph lives in graphtest so other packages can reuse it; these
// short names keep the graph tests readable.

func seed(t *testing.T, p *pgxpool.Pool) graphtest.IDs { return graphtest.Seed(t, p) }

func freshReceita(t *testing.T, p *pgxpool.Pool) (graphtest.Builder, int64) {
	return graphtest.FreshReceita(t, p)
}

func ent(id int64) graphtest.End             { return graphtest.Ent(id) }
func rec(record, entity int64) graphtest.End { return graphtest.Rec(record, entity) }
func date(y int, m time.Month, d int) *time.Time {
	return graphtest.Date(y, m, d)
}
