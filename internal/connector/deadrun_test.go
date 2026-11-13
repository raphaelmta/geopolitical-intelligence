package connector_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/identity"
)

func status(t *testing.T, pool *pgxpool.Pool, id int64) (string, string) {
	t.Helper()
	var st string
	var msg *string
	if err := pool.QueryRow(ctx, `SELECT status, error FROM collection WHERE id = $1`, id).Scan(&st, &msg); err != nil {
		t.Fatal(err)
	}
	if msg == nil {
		return st, ""
	}
	return st, *msg
}

// A worker killed mid-load leaves its collection running, with no process
// holding its run lock, and person records already written. The next
// Collect (the redelivered request) marks it failed before starting, so
// Resolve of the new collection also resolves the dead run's records.
func TestCollectFailsADeadRunAndResolveRecoversItsRecords(t *testing.T) {
	pool := setup(t)
	store := claims.NewStore(pool)
	// The killed run: running, never locked (opened by Begin, as by a worker
	// that died; its session and lock are gone).
	dead, err := collection.Begin(ctx, pool, 1, ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, ctxKey := range []string{"90000001", "90000002"} {
		if _, _, err := store.UpsertPersonRecord(ctx, claims.PersonRecordInput{SourceID: 1, ContextKey: ctxKey,
			Name: "PESSOA FICTICIA MORTA", CPFFragment: "112233", Prov: claims.Provenance{CollectionID: dead.ID, RecordRef: "test:" + ctxKey}}); err != nil {
			t.Fatal(err)
		}
	}
	resolver := &identity.Resolver{Pool: pool}
	h := connector.Hooks{Resolve: func(ctx context.Context, id int64) error {
		_, err := resolver.Resolve(ctx, id)
		return err
	}}
	col, err := connector.Collect(ctx, &fake{key: "tse"}, req("tse"), pool, nil, h)
	if err != nil {
		t.Fatal(err)
	}
	if st, msg := status(t, pool, dead.ID); st != collection.StatusFailed || msg != collection.InterruptedError {
		t.Fatalf("dead run: %s %q, want failed %q", st, msg, collection.InterruptedError)
	}
	if st, _ := status(t, pool, col.ID); st != collection.StatusDone {
		t.Fatalf("new collection: %s, want done", st)
	}
	var resolved bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity_resolve_done WHERE collection_id = $1)`, dead.ID).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if !resolved {
		t.Fatal("the dead run's person records were not resolved")
	}
	var entities int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT entity_id) FROM person_record WHERE first_collection_id = $1`, dead.ID).Scan(&entities); err != nil {
		t.Fatal(err)
	}
	if entities != 1 {
		t.Fatalf("the dead run's two records (same name and fragment) are in %d entities, want 1", entities)
	}
}

// A live run (its lock held by another session, here another Start) is
// never failed, whatever its age.
func TestCollectLeavesALiveRunAlone(t *testing.T) {
	pool := setup(t)
	live, err := collection.Start(ctx, pool, 1, ref, time.Time{}, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Release()
	if _, err := connector.Collect(ctx, &fake{key: "tse"}, req("tse"), pool, nil, connector.Hooks{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := status(t, pool, live.ID); st != collection.StatusRunning {
		t.Fatalf("live run: %s, want running", st)
	}
	// Once its process is gone (lock released without finishing), it is dead.
	live.Release()
	failed, err := collection.FailDead(ctx, pool)
	if err != nil || len(failed) != 1 || failed[0] != live.ID {
		t.Fatalf("FailDead = %v, %v; want [%d]", failed, err, live.ID)
	}
}

// Every collection Collect opens holds its run lock until it ends: during
// Load another FailDead must not touch it, and after the run the lock is
// released (FailDead finds nothing and the row is done).
func TestCollectHoldsTheRunLockDuringTheRun(t *testing.T) {
	pool := setup(t)
	var during []int64
	f := &fake{key: "tse", load: func(ctx context.Context) (connector.Stats, error) {
		var err error
		during, err = collection.FailDead(ctx, pool)
		return connector.Stats{"rows_read": 0, "rows_skipped": 0}, err
	}}
	col, err := connector.Collect(ctx, f, req("tse"), pool, nil, connector.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(during) != 0 {
		t.Fatalf("FailDead during the run failed %v", during)
	}
	if st, _ := status(t, pool, col.ID); st != collection.StatusDone {
		t.Fatalf("status %s, want done", st)
	}
	// The lock is free again: taking it from another session succeeds.
	var free bool
	if err := pool.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('geoint.collection_run.' || current_schema()), $1::int4)`, col.ID).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Fatal("the run lock outlived the run")
	}
}

// The observed date of an edition comes from the resolver; without one, a
// collection is observed on the day it started (UTC).
func TestCollectStoresTheObservedDate(t *testing.T) {
	pool := setup(t)
	f := newEditionFake(editionA)
	f.observed = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	col, err := connector.Collect(ctx, f, fullReq(), pool, nil, connector.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	var stored time.Time
	if err := pool.QueryRow(ctx, `SELECT observed_date FROM collection WHERE id = $1`, col.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(f.observed) || !col.ObservedDate.Equal(f.observed) || !f.gotCol.ObservedDate.Equal(f.observed) {
		t.Errorf("observed stored %v, returned %v, Load saw %v; want %v", stored, col.ObservedDate, f.gotCol.ObservedDate, f.observed)
	}
	plain, err := connector.Collect(ctx, &fake{key: "tse"}, req("tse"), pool, nil, connector.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	var today bool
	if err := pool.QueryRow(ctx, `SELECT observed_date = (now() AT TIME ZONE 'UTC')::date FROM collection WHERE id = $1`, plain.ID).Scan(&today); err != nil {
		t.Fatal(err)
	}
	if !today {
		t.Error("a collection without an edition is not observed on its start day")
	}
}
