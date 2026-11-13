package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/rawstore"
	"geoint/internal/sources"
)

// failTimeout bounds the write that records a failure; the run's own context
// may already be cancelled by then.
const failTimeout = 15 * time.Second

// RunError reports a run that failed after its collection was opened. The
// failure is recorded in that collection, so retrying means a new collection.
type RunError struct {
	CollectionID int64
	Err          error
}

func (e *RunError) Error() string { return fmt.Sprintf("collection %d: %v", e.CollectionID, e.Err) }
func (e *RunError) Unwrap() error { return e.Err }

// Run executes one collection: Start, Fetch, Load, CloseUnseen (snapshot
// sources only), Resolve, Finish, then the Finished hook. Any error, panic or
// cancellation before Finish marks the collection failed, stops the later
// steps and is returned as a *RunError. Errors before Start (unknown source,
// database down) are returned as is. Finished cannot fail the collection: a
// panic in it is logged and the run still succeeds.
//
// For a connector that is an EditionResolver, the requested date is first
// resolved to the newest published edition (see Collect). A full-scope
// request for an edition that already has a done full-scope collection is
// not run: Run logs "nothing new" and returns nil, so the queue consumer
// acknowledges it like a success; Collect reports the same case as
// ErrNothingNew.
//
// Redelivery (queue outages, killed workers). A request is acknowledged only
// when its run ends. If the worker is killed, or loses NATS for longer than
// the stream's AckWait, the request is delivered again and run again: the
// interrupted collection (if any) is failed by the dead-run check at the
// start of the next Collect and its person records are resolved by the next
// Resolve. A source without edition resolution (TSE, Câmara, Senado, CVM)
// may therefore be collected twice for one request; every loader is
// idempotent (natural keys, last_seen only advances), so the second run only
// costs time and a second copy of nothing (raw files are content-addressed
// by key and kept once).
func Run(ctx context.Context, c Connector, req Request, pool *pgxpool.Pool, raw rawstore.Store, h Hooks) error {
	_, err := Collect(ctx, c, req, pool, raw, h)
	if errors.Is(err, ErrNothingNew) {
		return nil
	}
	return err
}

// Collect is Run returning the collection it opened (with the reference date
// actually collected) and ErrNothingNew, wrapped, when a full-scope request
// finds its edition already collected. In that case no collection row is
// created and no hook runs; the successful check is recorded in source_check
// (so the source's status no longer shows an earlier failed attempt).
//
// Before anything else, running collections that no live process holds
// (their run lock is free: a killed worker) are marked failed
// (collection.FailDead), so the redelivered request that follows a kill
// starts from a consistent state and Resolve later recovers the dead run's
// person records. This runs whatever SCHEDULER_ENABLED says.
//
// Every collection is opened with collection.Start and holds its run lock
// until it is finished or failed.
//
// When c is an EditionResolver, ResolveReference runs before Start and the
// resolved edition gives the collection's reference date (the one Fetch and
// Load see) and its observed date. If resolution fails, a collection is opened with the requested
// date and immediately marked failed with the cause, so the attempt is
// visible like any failed collection; the error is a *RunError. A
// cancellation during resolution is returned as is, with no collection.
// Slice requests are never skipped: re-loading a slice of the same edition
// is legitimate.
func Collect(ctx context.Context, c Connector, req Request, pool *pgxpool.Pool, raw rawstore.Store, h Hooks) (collection.Collection, error) {
	src, ok := sources.ByKey(req.Source)
	if !ok {
		return collection.Collection{}, fmt.Errorf("connector: unknown source %q", req.Source)
	}
	if c.Key() != req.Source {
		return collection.Collection{}, fmt.Errorf("connector: %q cannot serve source %q", c.Key(), req.Source)
	}
	FailDeadRuns(ctx, pool)
	var observed time.Time
	if er, ok := c.(EditionResolver); ok {
		ed, err := resolveEdition(ctx, er, src, req.ReferenceDate, pool)
		if err != nil {
			if ctx.Err() != nil {
				return collection.Collection{}, err
			}
			run, berr := collection.Start(ctx, pool, src.ID, req.ReferenceDate, time.Time{}, req.Scope)
			if berr != nil {
				return collection.Collection{}, errors.Join(err, berr)
			}
			defer run.Release()
			return run.Collection, fail(ctx, pool, src, run.Collection, err)
		}
		resolved := ed.ReferenceDate
		if req.Scope.Full {
			done, err := doneFull(ctx, pool, src.ID, resolved)
			if err != nil {
				return collection.Collection{}, err
			}
			if done {
				slog.Info("connector: nothing new", "source", src.Key,
					"requested", req.ReferenceDate.Format(time.DateOnly), "edition", resolved.Format(time.DateOnly))
				recordCheck(ctx, pool, src)
				return collection.Collection{}, fmt.Errorf("connector %s: edition %s: %w", src.Key, resolved.Format(time.DateOnly), ErrNothingNew)
			}
		}
		if !resolved.Equal(req.ReferenceDate) {
			slog.Info("connector: edition resolved", "source", src.Key,
				"requested", req.ReferenceDate.Format(time.DateOnly), "edition", resolved.Format(time.DateOnly))
		}
		req.ReferenceDate = resolved
		observed = ed.ObservedDate
	}
	run, err := collection.Start(ctx, pool, src.ID, req.ReferenceDate, observed, req.Scope)
	if err != nil {
		return collection.Collection{}, err
	}
	col := run.Collection
	err = execute(ctx, c, src, col, req, pool, raw, h)
	if err != nil {
		err = fail(ctx, pool, src, col, err)
	}
	// Finished or failed: the run lock is no longer needed (the Finished hook
	// below may take long and needs no lock).
	run.Release()
	if err != nil {
		return col, err
	}
	if h.Finished != nil {
		finished(ctx, h.Finished, src, col.ID)
	}
	return col, nil
}

// fail records err in col and returns it as a *RunError.
func fail(ctx context.Context, pool *pgxpool.Pool, src sources.Source, col collection.Collection, err error) error {
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failTimeout)
	defer cancel()
	if ferr := collection.Fail(failCtx, pool, col.ID, err); ferr != nil {
		slog.Error("connector: could not record failure", "source", src.Key, "collection", col.ID, "error", ferr)
		err = errors.Join(err, ferr)
	}
	return &RunError{CollectionID: col.ID, Err: err}
}

// FailDeadRuns marks failed the running collections no live process holds
// (collection.FailDead) and logs them. A failure to check is only logged:
// the age rule of the scheduler remains as a fallback.
func FailDeadRuns(ctx context.Context, pool *pgxpool.Pool) {
	ids, err := collection.FailDead(ctx, pool)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("connector: could not check for dead collection runs", "error", err)
		}
	}
	if len(ids) > 0 {
		slog.Warn("connector: running collections without a live worker marked failed",
			"collections", ids, "error_text", collection.InterruptedError)
	}
}

// resolveEdition calls the resolver, turning a panic into an error.
func resolveEdition(ctx context.Context, er EditionResolver, src sources.Source, requested time.Time, pool *pgxpool.Pool) (ed Edition, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("connector: edition resolver panicked", "source", src.Key, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("connector %s: resolve edition: panic: %v", src.Key, r)
		}
	}()
	ed, err = er.ResolveReference(ctx, requested, pool)
	if err != nil {
		return Edition{}, fmt.Errorf("connector %s: resolve edition for %s: %w", src.Key, requested.Format(time.DateOnly), err)
	}
	if ed.ReferenceDate.IsZero() {
		return Edition{}, fmt.Errorf("connector %s: resolve edition for %s: resolver returned no date", src.Key, requested.Format(time.DateOnly))
	}
	return ed, nil
}

// recordCheck notes a successful check that found nothing new (source_check,
// DB clock), so a failed attempt before it no longer marks the source failed.
// The check itself succeeded, so a failure to record it is only logged.
func recordCheck(ctx context.Context, pool *pgxpool.Pool, src sources.Source) {
	_, err := pool.Exec(ctx, `INSERT INTO source_check (source_id, last_checked_at, last_outcome)
		VALUES ($1, now(), 'nothing_new')
		ON CONFLICT (source_id) DO UPDATE SET last_checked_at = EXCLUDED.last_checked_at, last_outcome = EXCLUDED.last_outcome`, src.ID)
	if err != nil {
		slog.Warn("connector: could not record the source check", "source", src.Key, "error", err)
	}
}

// doneFull reports whether a done full-scope collection of the source has
// exactly this reference date.
func doneFull(ctx context.Context, pool *pgxpool.Pool, sourceID int16, ref time.Time) (bool, error) {
	var done bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM collection WHERE source_id = $1 AND status = 'done'
		AND (scope ->> 'full')::boolean IS TRUE AND reference_date = $2::date)`, sourceID, ref).Scan(&done)
	if err != nil {
		return false, fmt.Errorf("connector: check collected editions: %w", err)
	}
	return done, nil
}

func finished(ctx context.Context, f func(context.Context, int64), src sources.Source, collectionID int64) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("connector: finished hook panicked", "source", src.Key, "collection", collectionID,
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	f(ctx, collectionID)
}

func execute(ctx context.Context, c Connector, src sources.Source, col collection.Collection, req Request,
	pool *pgxpool.Pool, raw rawstore.Store, h Hooks) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("connector panicked", "source", src.Key, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("connector %s: panic: %v", src.Key, r)
		}
	}()

	files, err := c.Fetch(ctx, req.ReferenceDate, raw)
	if err != nil {
		return fmt.Errorf("connector %s: fetch: %w", src.Key, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("connector %s: after fetch: %w", src.Key, err)
	}
	stats, err := c.Load(ctx, col, files, raw, pool)
	if err != nil {
		return fmt.Errorf("connector %s: load: %w", src.Key, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("connector %s: after load: %w", src.Key, err)
	}
	for _, k := range []string{"rows_read", "rows_skipped"} {
		if _, ok := stats[k]; !ok {
			return fmt.Errorf("connector %s: load returned stats without mandatory key %q", src.Key, k)
		}
	}
	if src.Snapshot && h.CloseUnseen != nil {
		if _, err := h.CloseUnseen(ctx, src, col); err != nil {
			return fmt.Errorf("connector %s: close unseen: %w", src.Key, err)
		}
	}
	if h.Resolve != nil {
		if err := h.Resolve(ctx, col.ID); err != nil {
			return fmt.Errorf("connector %s: resolve: %w", src.Key, err)
		}
	}
	if err := collection.Finish(ctx, pool, col.ID, stats); err != nil {
		return fmt.Errorf("connector %s: %w", src.Key, err)
	}
	return nil
}
