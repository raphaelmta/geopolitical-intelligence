package connector_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
)

// editionFake is a fake connector published as dated editions.
type editionFake struct {
	*fake
	resolve  func(time.Time) (time.Time, error)
	observed time.Time   // the edition's publication date (zero: unknown)
	resolved []time.Time // requested dates seen
}

func (e *editionFake) ResolveReference(_ context.Context, requested time.Time, _ *pgxpool.Pool) (connector.Edition, error) {
	e.resolved = append(e.resolved, requested)
	t, err := e.resolve(requested)
	return connector.Edition{ReferenceDate: t, ObservedDate: e.observed}, err
}

var (
	requested = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	editionA  = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

func newEditionFake(edition time.Time) *editionFake {
	return &editionFake{fake: &fake{key: "receita"}, resolve: func(time.Time) (time.Time, error) { return edition, nil }}
}

func fullReq() connector.Request {
	return connector.Request{Source: "receita", ReferenceDate: requested, Scope: collection.Scope{Full: true}}
}

func TestRunUsesTheResolvedEditionEverywhere(t *testing.T) {
	pool := setup(t)
	f := newEditionFake(editionA)
	col, err := connector.Collect(ctx, f, fullReq(), pool, nil, connector.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.resolved) != 1 || !f.resolved[0].Equal(requested) {
		t.Errorf("resolver saw %v, want the requested date", f.resolved)
	}
	var stored time.Time
	if err := pool.QueryRow(ctx, `SELECT reference_date FROM collection WHERE id = $1`, col.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Equal(editionA) || !col.ReferenceDate.Equal(editionA) {
		t.Errorf("collection reference_date = %v (returned %v), want %v", stored, col.ReferenceDate, editionA)
	}
	if !f.gotRefDay.Equal(editionA) || !f.gotCol.ReferenceDate.Equal(editionA) {
		t.Errorf("Fetch saw %v, Load saw %v; want %v", f.gotRefDay, f.gotCol.ReferenceDate, editionA)
	}
}

func TestRunRecordsAFailedCollectionWhenResolutionFails(t *testing.T) {
	for name, cause := range map[string]error{
		"no edition": connector.ErrNoEdition,
		"listing":    errors.New("listing: unexpected status 503"),
	} {
		t.Run(name, func(t *testing.T) {
			pool := setup(t)
			f := &editionFake{fake: &fake{key: "receita"}, resolve: func(time.Time) (time.Time, error) { return time.Time{}, cause }}
			finished := 0
			err := connector.Run(ctx, f, fullReq(), pool, nil, connector.Hooks{Finished: func(context.Context, int64) { finished++ }})
			var runErr *connector.RunError
			if !errors.As(err, &runErr) || !errors.Is(err, cause) {
				t.Fatalf("err = %v, want a *RunError wrapping %v", err, cause)
			}
			var (
				status string
				ref    time.Time
				msg    string
			)
			if err := pool.QueryRow(ctx, `SELECT status, reference_date, error FROM collection WHERE id = $1`, runErr.CollectionID).
				Scan(&status, &ref, &msg); err != nil {
				t.Fatal(err)
			}
			if status != "failed" || !ref.Equal(requested) || !strings.Contains(msg, cause.Error()) {
				t.Errorf("collection = %s %v %q; want failed, the requested date and the cause", status, ref, msg)
			}
			if f.fetched != 0 || f.loaded != 0 || finished != 0 {
				t.Errorf("fetch=%d load=%d finished=%d after a resolution failure", f.fetched, f.loaded, finished)
			}
		})
	}
}

func TestRunSkipsAFullRequestForAnEditionAlreadyCollected(t *testing.T) {
	pool := setup(t)
	f := newEditionFake(editionA)
	if err := connector.Run(ctx, f, fullReq(), pool, nil, connector.Hooks{}); err != nil {
		t.Fatal(err)
	}
	before := count(t, pool)
	finished := 0
	h := connector.Hooks{Finished: func(context.Context, int64) { finished++ }}

	_, err := connector.Collect(ctx, f, fullReq(), pool, nil, h)
	if !errors.Is(err, connector.ErrNothingNew) {
		t.Fatalf("Collect err = %v, want ErrNothingNew", err)
	}
	// Run acknowledges the same case as a success.
	if err := connector.Run(ctx, f, fullReq(), pool, nil, h); err != nil {
		t.Fatalf("Run err = %v, want nil for nothing new", err)
	}
	if n := count(t, pool); n != before {
		t.Errorf("collections = %d, want %d (no row for nothing new)", n, before)
	}
	if f.loaded != 1 || f.fetched != 1 || finished != 0 {
		t.Errorf("fetch=%d load=%d finished=%d, want 1, 1, 0", f.fetched, f.loaded, finished)
	}
	// Each nothing-new check is recorded, on the database clock.
	var outcome string
	var fresh bool
	if err := pool.QueryRow(ctx, `SELECT last_outcome, last_checked_at > now() - interval '1 minute' FROM source_check WHERE source_id = 1`).
		Scan(&outcome, &fresh); err != nil {
		t.Fatal(err)
	}
	if outcome != "nothing_new" || !fresh {
		t.Errorf("source_check = %q fresh=%v", outcome, fresh)
	}
}

func TestRunNeverSkipsASliceOfAnEditionAlreadyCollected(t *testing.T) {
	pool := setup(t)
	f := newEditionFake(editionA)
	if err := connector.Run(ctx, f, fullReq(), pool, nil, connector.Hooks{}); err != nil {
		t.Fatal(err)
	}
	slice := connector.Request{Source: "receita", ReferenceDate: requested, Scope: collection.Scope{CNPJs: []string{"00000000"}, Depth: 1}}
	col, err := connector.Collect(ctx, f, slice, pool, nil, connector.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if f.loaded != 2 || !col.ReferenceDate.Equal(editionA) {
		t.Errorf("load=%d ref=%v; the slice must load, at the resolved edition", f.loaded, col.ReferenceDate)
	}
}

func TestRunWithoutResolverKeepsTheRequestedDate(t *testing.T) {
	pool := setup(t)
	f := &fake{key: "receita"}
	r := fullReq()
	if err := connector.Run(ctx, f, r, pool, nil, connector.Hooks{}); err != nil {
		t.Fatal(err)
	}
	// Same date again: without a resolver nothing is ever skipped.
	if err := connector.Run(ctx, f, r, pool, nil, connector.Hooks{}); err != nil {
		t.Fatal(err)
	}
	if f.loaded != 2 || !f.gotRefDay.Equal(requested) {
		t.Errorf("load=%d ref=%v", f.loaded, f.gotRefDay)
	}
}
