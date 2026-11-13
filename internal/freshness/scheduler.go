package freshness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/queue"
	"geoint/internal/sources"
)

const (
	// DefaultRunningStaleAfter: a national Receita load takes hours; a live
	// collection still running after 24 h is logged as possibly stuck (it is
	// never failed for its age while its run lock is held).
	DefaultRunningStaleAfter = 24 * time.Hour
	// retryWindow: after a request reached the queue, the source is not
	// requested again for this long, whether or not the collection started
	// and whether or not it failed (so a failing source is retried once a day).
	retryWindow = 24 * time.Hour
	// DefaultQueuedRequestBlocks: a published request that no collection
	// (nor a "nothing new" check) has followed yet is still waiting in the
	// queue, behind other long collections; it blocks a new request for
	// this long (see Service.QueuedRequestBlocks).
	DefaultQueuedRequestBlocks = 72 * time.Hour
	// unpublishedWindow: a request that never reached the queue (publish
	// failed, or the process died after the insert) blocks only this long.
	unpublishedWindow = 10 * time.Minute
	// schedulerBy is recorded in collection_request.requested_by.
	schedulerBy = "scheduler"
	// schedulerLockKey is the transaction-level advisory lock that serializes
	// the decide+insert step across processes ("schedulr").
	schedulerLockKey int64 = 0x7363686564756c72
)

// Publisher is what the scheduler needs from the queue; *queue.Client is one.
type Publisher interface {
	Publish(ctx context.Context, subject string, v any) error
}

// Tick requests a full collection of every source that needs one and returns
// their keys. See TickWith for the rule.
func (s *Service) Tick(ctx context.Context, q *queue.Client) (requested []string, err error) {
	return s.TickWith(ctx, q)
}

// TickWith is Tick on any Publisher.
//
// A source with a connector is requested when none of these holds:
//   - a collection of it is running, its run lock held by a live process
//     (whatever its age: age alone never fails nor unblocks a live run);
//   - a request for it was published in the last 24 h, or recorded without
//     being published in the last 10 min (collection_request);
//   - a request for it was published in the last QueuedRequestBlocks
//     (default 72 h) and no collection of the source started, and no
//     successful check was recorded, after it: the request is still waiting
//     in the queue, and a second one would mean a second full collection.
//     Once a full-scope collection started after the request exists, the
//     other rules apply (running blocks; a failure is retried at most once
//     per 24 h; a done collection is due by age);
//
// and the source already has a done full-scope collection (slices never
// count) and one of these holds:
//   - the newest done full-scope collection has a reference date at least
//     PublishEvery old (exactly PublishEvery old is due);
//   - the most recent finished full-scope collection failed and no
//     successful check (source_check: nothing new) came after it (retried at
//     most once per day, by the 24 h rule).
//
// The scheduler never makes the FIRST collection of a source: a source with
// no done full-scope collection is never requested, so the first (national)
// load of each source starts only when someone asks (geoint collect, or the
// "Coletar agora" button). That includes a source whose first explicit
// collection failed: the scheduler does not retry it; the owner does (the
// failure is on the sources screen). Once a full collection is done, the
// scheduler keeps the source fresh and retries its failures as above.
//
// The request carries today's date. For a source published as dated
// editions (connector.EditionResolver, e.g. Receita) the runner resolves it
// to the newest published edition, and that edition's date is the
// collection's reference date. So once the last edition is PublishEvery
// old, the source is requested once a day; while no newer edition exists,
// each request costs one listing and creates no collection (ErrNothingNew),
// so the source's status keeps following the last edition's date.
//
// Before deciding, dead runs are marked failed ("interrupted: worker
// stopped"): running collections whose run lock no live process holds
// (collection.FailDead; the worker also does this at start and before every
// collection, whether or not the scheduler is enabled). A live run is never
// failed for its age: a run older than RunningStaleAfter whose lock is held
// is only logged, as a hint that it may be stuck. Only a row whose lock
// cannot be checked (an id beyond the lock key range, never opened by
// collection.Start) is still failed by age. The source is then judged in
// the same tick.
//
// Only full-scope requests count (slice rows never block). Requests made by
// an admin (Request) count as the scheduler's own. Every window above is
// measured on the database clock, the clock that stamps requested_at,
// collection.started_at and source_check; the Service's clock only gives
// the requested date and the age of the newest data.
//
// The decision and the insert of the collection_request rows happen in one
// transaction under an advisory lock (the one Request takes); a process
// that cannot get the lock returns (nil, nil). The rows are committed before publishing, so a request
// is never published without a record. If publishing fails the row stays
// unpublished: the error is returned (sources published before it are in
// requested) and the source is tried again once the row is 10 minutes old.
func (s *Service) TickWith(ctx context.Context, pub Publisher) (requested []string, err error) {
	now := s.now()
	type pending struct {
		id  int64
		req connector.Request
	}
	var todo []pending
	if ids, err := collection.FailDead(ctx, s.pool); err != nil {
		return nil, fmt.Errorf("scheduler: dead runs: %w", err)
	} else if len(ids) > 0 {
		slog.Warn("scheduler: running collections without a live worker marked failed", "collections", ids)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, schedulerLockKey).Scan(&locked); err != nil {
		return nil, fmt.Errorf("scheduler: lock: %w", err)
	}
	if !locked {
		return nil, nil
	}
	// Age alone never fails a run whose lock is held (FailDead above already
	// failed every run whose lock is free). Only an id beyond the lock key
	// range, whose lock cannot be checked, is still failed by age; the same
	// guarded update as collection.Fail: only running rows.
	if _, err := tx.Exec(ctx, `UPDATE collection SET status = $1, finished_at = now(), error = $2
		WHERE status = $3 AND id > 2147483647 AND started_at < now() - make_interval(secs => $4)`,
		collection.StatusFailed, collection.InterruptedError, collection.StatusRunning, s.runningStaleAfter().Seconds()); err != nil {
		return nil, fmt.Errorf("scheduler: fail stale running collections: %w", err)
	}
	// Only a log line depends on it: a failure is logged and the tick goes
	// on. It runs under a savepoint so a failed query does not abort tx.
	if err := logLongRuns(ctx, tx, s.runningStaleAfter()); err != nil {
		slog.Warn("scheduler: could not list long runs (tick continues)", "error", err)
	}
	// The requested date; edition sources resolve it (see above).
	ref := now.UTC().Truncate(24 * time.Hour)
	for _, src := range s.Catalogue {
		if !src.HasConnector {
			continue
		}
		due, err := s.due(ctx, tx, src.ID, src.PublishEvery, now)
		if err != nil {
			return nil, fmt.Errorf("scheduler: %s: %w", src.Key, err)
		}
		if !due {
			continue
		}
		id, err := insertRequest(ctx, tx, src.ID, schedulerBy, true)
		if err != nil {
			return nil, fmt.Errorf("scheduler: %s: %w", src.Key, err)
		}
		todo = append(todo, pending{id, connector.Request{Source: src.Key, ReferenceDate: ref, Scope: collection.Scope{Full: true}}})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("scheduler: commit: %w", err)
	}

	var errs []error
	for _, p := range todo {
		published, err := s.publish(ctx, pub, p.id, p.req)
		if err != nil {
			errs = append(errs, fmt.Errorf("scheduler: %w", err))
		}
		if published {
			requested = append(requested, p.req.Source)
		}
	}
	return requested, errors.Join(errs...)
}

// ErrRequestBlocked: a full collection of the source is running, or a full
// request for it is waiting to start (see Request).
var ErrRequestBlocked = errors.New("freshness: a collection of the source is running or a request for it is waiting to start")

// Request records a request for a collection of src asked by `by` (a user's
// sub) and publishes req for the worker: the insert, publish and stamp
// sequence of TickWith, so a manual request counts for the scheduler too
// (it will not ask again for 24 h, nor while the request waits).
//
// A full request is refused with ErrRequestBlocked when a collection of the
// source is running (a live process holds its run lock) or a full request
// is waiting to start: published within QueuedRequestBlocks with no
// full-scope collection started and no successful check recorded after it,
// or recorded without being published within the last 10 minutes. These
// are the scheduler's blocking rules (blocking), except its 24 h rule: that
// rule spaces the scheduler's own periodic asking (and its retries of a
// failing source), while an admin who asks explicitly once the previous
// collection started wants a new one now — a second request then is not a
// duplicate of one still in the queue. The check and the insert happen in
// one transaction under the scheduler's advisory lock, so an admin and a
// tick cannot both pass.
//
// A slice request (not req.Scope.Full) is never refused; it is recorded
// with full_scope false, which no rule counts.
//
// As in TickWith the row is committed before publishing; if publishing
// fails the error is returned and the row stays unpublished (it blocks a
// full request for 10 minutes).
func (s *Service) Request(ctx context.Context, pub Publisher, src sources.Source, req connector.Request, by string) error {
	req.Source = src.Key
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("request %s: begin: %w", src.Key, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	full := req.Scope.Full
	if full {
		// Waits for a tick in progress (which holds it briefly).
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, schedulerLockKey); err != nil {
			return fmt.Errorf("request %s: lock: %w", src.Key, err)
		}
		b, err := s.blocking(ctx, tx, src.ID)
		if err != nil {
			return fmt.Errorf("request %s: %w", src.Key, err)
		}
		switch {
		case b.running:
			return fmt.Errorf("request %s: a collection is running: %w", src.Key, ErrRequestBlocked)
		case b.waiting:
			return fmt.Errorf("request %s: a request is waiting to start: %w", src.Key, ErrRequestBlocked)
		}
	}
	id, err := insertRequest(ctx, tx, src.ID, by, full)
	if err != nil {
		return fmt.Errorf("request %s: %w", src.Key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("request %s: commit: %w", src.Key, err)
	}
	_, err = s.publish(ctx, pub, id, req)
	return err
}

// insertRequest records a request; requested_at is the database clock at
// the insert itself (clock_timestamp(), not the transaction start): the
// caller holds the scheduler lock by then, which it may have waited for, and
// a collection that started while it waited must not count as following
// this request.
func insertRequest(ctx context.Context, tx pgx.Tx, sourceID int16, by string, full bool) (int64, error) {
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO collection_request (source_id, requested_at, requested_by, full_scope)
		VALUES ($1, clock_timestamp(), $2, $3) RETURNING id`, sourceID, by, full).Scan(&id); err != nil {
		return 0, fmt.Errorf("record request: %w", err)
	}
	return id, nil
}

// publish sends the recorded request id to the worker and stamps it
// published (database clock). published reports whether the message
// reached the queue, even if the stamp then failed.
func (s *Service) publish(ctx context.Context, pub Publisher, id int64, req connector.Request) (published bool, err error) {
	if err := pub.Publish(ctx, "ingest.request."+req.Source, req); err != nil {
		return false, fmt.Errorf("request %s: %w", req.Source, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE collection_request SET published_at = now() WHERE id = $1`, id); err != nil {
		// Published but not marked: it blocks for 10 min only, so the
		// source may be requested twice; the second run is a harmless
		// repeat (loads are idempotent).
		return true, fmt.Errorf("mark %s published: %w", req.Source, err)
	}
	return true, nil
}

// longRunsSQL lists the live collections running for longer than $2
// seconds (a variable so a test can make it fail).
var longRunsSQL = `SELECT id FROM collection WHERE status = $1
	AND started_at < now() - make_interval(secs => $2) AND ` + collection.LiveRunSQL("id") + ` ORDER BY id`

// logLongRuns logs, as a hint for the operator, every live collection that
// has been running for longer than after: it is left running (its run lock
// is held by a live process), but a run that long may be stuck.
func logLongRuns(ctx context.Context, tx pgx.Tx, after time.Duration) error {
	sp, err := tx.Begin(ctx) // savepoint: a failure here leaves tx usable
	if err != nil {
		return err
	}
	defer func() { _ = sp.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := sp.Query(ctx, longRunsSQL, collection.StatusRunning, after.Seconds())
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		slog.Warn("scheduler: collections running for a long time (their run lock is held: left running)",
			"collections", ids, "running_longer_than", after.String())
	}
	return nil
}

func (s *Service) runningStaleAfter() time.Duration {
	if s.RunningStaleAfter <= 0 {
		return DefaultRunningStaleAfter
	}
	return s.RunningStaleAfter
}

func (s *Service) queuedRequestBlocks() time.Duration {
	if s.QueuedRequestBlocks <= 0 {
		return DefaultQueuedRequestBlocks
	}
	return s.QueuedRequestBlocks
}

// waitingSQL: a full request of source $1 published after the
// QueuedRequestBlocks window ($2, seconds) that no full-scope collection
// started at or after it, nor a successful check, has followed yet: it is
// still waiting in the queue. Aliased r.
const waitingSQL = `r.source_id = $1 AND r.full_scope AND r.published_at IS NOT NULL
	AND r.requested_at > now() - make_interval(secs => $2)
	AND NOT EXISTS (SELECT 1 FROM collection c WHERE c.source_id = $1 AND c.started_at >= r.requested_at
		AND (c.scope ->> 'full')::boolean IS TRUE)
	AND NOT EXISTS (SELECT 1 FROM source_check k WHERE k.source_id = $1 AND k.last_checked_at >= r.requested_at)`

// blocks is what holds a new full request of a source back.
type blocks struct {
	// running: a collection of the source (any scope) is running and a live
	// process holds its run lock, whatever its age (a run whose lock is
	// free is dead: it never blocks). An id beyond the lock key range
	// cannot be checked: it blocks while younger than RunningStaleAfter.
	running bool
	// waiting: a full request is waiting to start (waitingSQL), or was
	// recorded in the last 10 minutes without being published (in flight).
	waiting bool
	// recent: a full request was published in the last 24 h.
	recent bool
}

// blocking evaluates the blocking rules of a source, all on the database
// clock. Only full-scope requests count.
func (s *Service) blocking(ctx context.Context, tx pgx.Tx, sourceID int16) (blocks, error) {
	var b blocks
	err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM collection WHERE source_id = $1 AND status = 'running'
			AND (`+collection.LiveRunSQL("id")+` OR (id > 2147483647 AND started_at >= now() - make_interval(secs => $3)))),
		EXISTS (SELECT 1 FROM collection_request r WHERE `+waitingSQL+`)
			OR EXISTS (SELECT 1 FROM collection_request WHERE source_id = $1 AND full_scope
				AND published_at IS NULL AND requested_at > now() - make_interval(secs => $4)),
		EXISTS (SELECT 1 FROM collection_request WHERE source_id = $1 AND full_scope
			AND published_at IS NOT NULL AND requested_at > now() - make_interval(secs => $5))`,
		sourceID, s.queuedRequestBlocks().Seconds(), s.runningStaleAfter().Seconds(),
		unpublishedWindow.Seconds(), retryWindow.Seconds()).Scan(&b.running, &b.waiting, &b.recent)
	return b, err
}

// due reports whether the source needs a request now.
func (s *Service) due(ctx context.Context, tx pgx.Tx, sourceID int16, every time.Duration, now time.Time) (bool, error) {
	b, err := s.blocking(ctx, tx, sourceID)
	if err != nil {
		return false, err
	}
	if b.running || b.waiting || b.recent {
		return false, nil
	}
	var (
		lastDone, lastFinished, lastCheck *time.Time
		lastStatus                        *string
	)
	err = tx.QueryRow(ctx, `SELECT
		(SELECT reference_date FROM collection WHERE source_id = $1 AND status = 'done'
			AND (scope ->> 'full')::boolean IS TRUE ORDER BY reference_date DESC, id DESC LIMIT 1),
		lf.status, lf.finished_at,
		(SELECT last_checked_at FROM source_check WHERE source_id = $1)
		FROM (SELECT 1) one
		LEFT JOIN LATERAL (SELECT status, finished_at FROM collection WHERE source_id = $1 AND status <> 'running'
			AND (scope ->> 'full')::boolean IS TRUE ORDER BY id DESC LIMIT 1) lf ON true`, sourceID).
		Scan(&lastDone, &lastStatus, &lastFinished, &lastCheck)
	if err != nil {
		return false, err
	}
	switch {
	case lastDone == nil:
		// Never the first collection of a source (see TickWith).
		return false, nil
	case failedNow(lastStatus, lastFinished, lastCheck):
		// A failure not followed by a successful check: retried once a day.
		return true, nil
	}
	r := time.Date(lastDone.Year(), lastDone.Month(), lastDone.Day(), 0, 0, 0, 0, time.UTC)
	return !r.Add(every).After(now), nil
}
