package claimstest

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Checks of task 3 of the load optimisation (the early identity pass): that
// the pass really runs inside the loads of a Pipeline, and that no
// relationship is re-pointed afterwards.

// WatchRepoints makes the (test) schema behind pool log every UPDATE of a
// relationship that changes one of its entity ends, whoever makes it: the
// resolver's re-point and sweep, a loader's "seen" update following a moved
// record, an analyst decision. Repoints counts them.
func WatchRepoints(ctx context.Context, pool *pgxpool.Pool) error {
	for _, q := range []string{
		`CREATE TABLE claimstest_repoint (relationship_id bigint NOT NULL)`,
		`CREATE FUNCTION claimstest_log_repoint() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				INSERT INTO claimstest_repoint (relationship_id) VALUES (NEW.id);
				RETURN NULL;
			END $$`,
		`CREATE TRIGGER claimstest_repoint AFTER UPDATE ON relationship FOR EACH ROW
			WHEN (OLD.src_entity_id IS DISTINCT FROM NEW.src_entity_id OR OLD.dst_entity_id IS DISTINCT FROM NEW.dst_entity_id)
			EXECUTE FUNCTION claimstest_log_repoint()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("claimstest: watch re-points: %w", err)
		}
	}
	return nil
}

// Repoints is the number of relationship updates that changed an entity end
// since WatchRepoints.
func Repoints(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM claimstest_repoint`).Scan(&n); err != nil {
		return 0, fmt.Errorf("claimstest: count re-points: %w", err)
	}
	return n, nil
}

// StalePointers is the number of relationship ends that differ from the
// current entity of their person record.
func StalePointers(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM relationship r
		LEFT JOIN person_record s ON s.id = r.src_record_id LEFT JOIN person_record d ON d.id = r.dst_record_id
		WHERE s.entity_id <> r.src_entity_id OR d.entity_id <> r.dst_entity_id`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("claimstest: count stale pointers: %w", err)
	}
	return n, nil
}

// CheckEarlyPass fails the test unless every done collection in the schema
// ran the early identity pass inside its Load (the loader called the
// ResolveRecords hook) and was then resolved, and no relationship end is
// stale. It is what keeps a scenario from passing on the order before task
// 3: a harness that does not hand the pass to the loaders, or a loader that
// stops calling it, fails here. Only for pipelines whose connectors all have
// an early pass (Receita, TSE).
func (p *Pipeline) CheckEarlyPass(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	rows, err := p.Pool.Query(ctx, `SELECT id FROM collection WHERE status = 'done' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if len(done) == 0 {
		t.Error("no done collection: nothing shows that the early identity pass runs")
	}
	for _, id := range done {
		if _, ok := p.Early[id]; !ok {
			t.Errorf("collection %d was loaded without the early identity pass (ResolveRecords)", id)
		}
		if _, ok := p.Final[id]; !ok {
			t.Errorf("collection %d is done without a Resolve", id)
		}
	}
	if n, err := StalePointers(ctx, p.Pool); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("%d relationship ends differ from their record's entity", n)
	}
}
