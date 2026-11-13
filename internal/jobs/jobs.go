// Package jobs runs long queries in the background. The API enqueues a job
// and returns its id; a worker runs it; the browser polls Get for the result.
//
// The job row in PostgreSQL is the source of truth. NATS only carries the
// job id, so a lost or duplicated message can never run a job twice or lose a
// result.
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/queue"
)

// Errors returned by the service.
var (
	ErrNotFound    = errors.New("job not found")
	ErrUnknownType = errors.New("unknown job type")
	ErrBadParams   = errors.New("invalid job params")
)

const (
	maxParamsSize = 64 << 10
	maxResultSize = 8 << 20
	// workerConcurrency is how many jobs a worker runs at once; they are
	// heavy read queries.
	workerConcurrency = 2
	consumerName      = "jobs-worker"

	defaultTimeout          = 5 * time.Minute
	defaultProgressInterval = time.Second
	// A running job writes its row at least every ProgressInterval; if it
	// has not for StaleAfter, its worker is presumed dead.
	defaultStaleAfter = 30 * time.Second
	// defaultSweepRunningAfter exceeds the queue's AckWait of 2 minutes.
	defaultSweepRunningAfter = 3 * time.Minute
	defaultSweepQueuedAfter  = time.Hour
	defaultSweepInterval     = time.Minute
	// defaultRetainFor is how long a finished job row is kept.
	defaultRetainFor = 30 * 24 * time.Hour
	// defaultPurgeBatch bounds the rows one Purge deletes.
	defaultPurgeBatch = 10_000
	// finalWriteTimeout bounds the write of a job's final state, which must
	// happen even if the worker is shutting down.
	finalWriteTimeout = 10 * time.Second
)

// Job is a background job as stored.
type Job struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Params    json.RawMessage `json:"params"`
	Status    string          `json:"status"` // queued | running | done | failed
	Progress  float64         `json:"progress"`
	Result    json.RawMessage `json:"result"`
	Error     string          `json:"error"`
	CreatedBy string          `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
	// FinishedAt is set when Status is done or failed.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Handler runs one job. progress may be called often; values are clamped to
// [0,1], never go down, and reach the row at most once per ProgressInterval.
// The returned value is stored as the job's JSON result.
type Handler func(ctx context.Context, params json.RawMessage, progress func(float64)) (any, error)

// Service enqueues jobs and runs them. Set the exported tuning fields, if at
// all, before RunWorker or Enqueue; zero values are not defaults, New sets them.
type Service struct {
	// Timeout is the per-job limit; a job over it fails with "timeout".
	Timeout time.Duration
	// ProgressInterval is how often a running job's row is written.
	ProgressInterval time.Duration
	// StaleAfter is the age of a running job's last write (its heartbeat,
	// refreshed every ProgressInterval) after which its worker is presumed
	// dead and a delivery takes the job over. It should be a few
	// ProgressIntervals. A delivery that finds a live heartbeat does not
	// return to the queue: it waits in the handler (the queue keeps the
	// message in progress), so a healthy concurrent run burns no delivery and
	// StaleAfter is independent of the queue's nak delay (30 s).
	//
	// Redelivery handles the common cases but is not a guarantee: every
	// shutdown Naks its message and uses up a delivery of the 5 allowed, and
	// after the last one JetStream never redelivers. The sweeper (Sweep) is
	// the backstop that guarantees no job stays running or queued forever.
	StaleAfter time.Duration
	// SweepRunningAfter is the age of a running job's last write after which
	// Sweep fails it as "interrupted". It must stay above the queue's AckWait
	// (2 min), so ordinary redelivery gets its chance first.
	SweepRunningAfter time.Duration
	// SweepQueuedAfter is the age of a queued job after which Sweep fails it
	// as "expired": its message was lost or exhausted before any claim.
	SweepQueuedAfter time.Duration
	// SweepInterval is how often RunWorker sweeps (and purges); <= 0 means
	// the default (1 min).
	SweepInterval time.Duration
	// RetainFor is how long a job in a final state (done or failed) is kept
	// after it finished; older rows are deleted by Purge, which RunWorker runs
	// with every sweep. The job table is operational data (results are
	// re-computable; the audit log keeps who asked what). <= 0 means the
	// default (30 days).
	RetainFor time.Duration

	// purgeBatch bounds the rows one Purge deletes; 0 means defaultPurgeBatch.
	purgeBatch int

	pool *pgxpool.Pool
	q    *queue.Client

	mu       sync.RWMutex
	handlers map[string]registration
}

type registration struct {
	h        Handler
	validate func(json.RawMessage) error
}

// New returns a Service storing jobs in p and queueing them on q.
func New(p *pgxpool.Pool, q *queue.Client) *Service {
	return &Service{
		Timeout: defaultTimeout, ProgressInterval: defaultProgressInterval, StaleAfter: defaultStaleAfter,
		SweepRunningAfter: defaultSweepRunningAfter, SweepQueuedAfter: defaultSweepQueuedAfter, SweepInterval: defaultSweepInterval,
		RetainFor: defaultRetainFor,
		pool:      p, q: q, handlers: map[string]registration{},
	}
}

var typeName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Register sets the handler of a job type. The name becomes part of the NATS
// subject jobs.run.<type>, so it must be lowercase letters, digits and
// underscores; Register panics otherwise (a programming error).
//
// A handler must honor its context: it is cancelled at the timeout and on
// shutdown. The service does not wait for a handler that ignores it past the
// timeout (the job fails with "timeout" and the result is discarded), but
// such a handler leaks its goroutine for as long as it runs.
func (s *Service) Register(typ string, h Handler) { s.RegisterValidated(typ, h, nil) }

// RegisterValidated is Register with a params validator, which Enqueue runs
// before storing anything; its errors should wrap ErrBadParams.
func (s *Service) RegisterValidated(typ string, h Handler, validate func(json.RawMessage) error) {
	if !typeName.MatchString(typ) || h == nil {
		panic(fmt.Sprintf("jobs: invalid registration of %q", typ))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[typ] = registration{h: h, validate: validate}
}

func (s *Service) handler(typ string) (registration, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.handlers[typ]
	return r, ok
}

// Enqueue stores a job of a registered type and queues it. params must
// marshal to a JSON object of at most 64 KiB; they are never logged, since
// they may name people. user is the OIDC subject of the owner.
func (s *Service) Enqueue(ctx context.Context, typ string, params any, user string) (uuid.UUID, error) {
	reg, ok := s.handler(typ)
	if !ok {
		return uuid.Nil, fmt.Errorf("%w: %q", ErrUnknownType, typ)
	}
	if user == "" {
		return uuid.Nil, errors.New("jobs: user is required")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: not encodable as JSON", ErrBadParams)
	}
	if len(raw) > maxParamsSize {
		return uuid.Nil, fmt.Errorf("%w: larger than %d bytes", ErrBadParams, maxParamsSize)
	}
	if !isObject(raw) {
		return uuid.Nil, fmt.Errorf("%w: not a JSON object", ErrBadParams)
	}
	if reg.validate != nil {
		if err := reg.validate(raw); err != nil {
			return uuid.Nil, err
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("jobs: new id: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO job (id, type, params, status, created_by) VALUES ($1, $2, $3, 'queued', $4)`,
		id, typ, raw, user); err != nil {
		return uuid.Nil, fmt.Errorf("jobs: insert: %w", err)
	}
	if err := s.q.Publish(ctx, "jobs.run."+typ, message{ID: id}); err != nil {
		// Without a message nobody would ever run the row.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalWriteTimeout)
		defer cancel()
		if _, ferr := s.pool.Exec(fctx, `UPDATE job SET status = 'failed', error = 'could not enqueue', finished_at = now(), updated_at = now()
			WHERE id = $1 AND status = 'queued'`, id); ferr != nil {
			slog.Error("jobs: could not store the outcome", "job", id, "error", ferr)
		}
		return uuid.Nil, fmt.Errorf("jobs: enqueue: %w", err)
	}
	return id, nil
}

func isObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}

// message is the NATS payload: only the job id.
type message struct {
	ID uuid.UUID `json:"id"`
}

// Get returns a job. A job that does not exist and one that belongs to
// another user (unless admin) are indistinguishable: both are ErrNotFound.
func (s *Service) Get(ctx context.Context, id uuid.UUID, user string, admin bool) (Job, error) {
	var j Job
	err := s.pool.QueryRow(ctx, `SELECT id, type, params, status, progress, result, error, created_by, created_at, finished_at
		FROM job WHERE id = $1 AND (created_by = $2 OR $3)`, id, user, admin).
		Scan(&j.ID, &j.Type, &j.Params, &j.Status, &j.Progress, &j.Result, &j.Error, &j.CreatedBy, &j.CreatedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("jobs: get: %w", err)
	}
	return j, nil
}

// RunWorker runs queued jobs, workerConcurrency at a time, until ctx is
// cancelled (then it returns nil). A job interrupted by the cancellation is
// not marked failed: it stays running and is taken over when its message is
// redelivered, or failed as "interrupted" by the sweeper, which RunWorker also
// runs (at startup and every SweepInterval).
func (s *Service) RunWorker(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sweeper sync.WaitGroup
	sweeper.Add(1)
	go func() { defer sweeper.Done(); s.sweepLoop(ctx) }()
	defer sweeper.Wait()
	errs := make(chan error, workerConcurrency)
	for range workerConcurrency {
		go func() {
			err := s.q.Consume(ctx, queue.StreamJobs, consumerName, "jobs.run.*", s.handleMessage)
			cancel() // one consumer gone: stop the others
			errs <- err
		}()
	}
	var first error
	for range workerConcurrency {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Service) sweepLoop(ctx context.Context) {
	interval := s.SweepInterval
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		interrupted, expired, err := s.Sweep(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Warn("jobs: sweep failed", "error", err)
		case interrupted+expired > 0:
			slog.Warn("jobs: sweep failed stuck jobs", "interrupted", interrupted, "expired", expired)
		}
		// Separate from the sweep: a failing purge never keeps stuck jobs
		// from being failed.
		purged, err := s.Purge(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Warn("jobs: purge of old jobs failed", "error", err)
		case purged > 0:
			slog.Info("jobs: purged old finished jobs", "deleted", purged)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep fails the jobs that no delivery will ever finish: running jobs whose
// last write is older than SweepRunningAfter ("interrupted") and queued jobs
// created more than SweepQueuedAfter ago ("expired"). It is idempotent and
// safe to run from several processes. A runner that is still alive cannot
// undo it: its writes require status running.
func (s *Service) Sweep(ctx context.Context) (interrupted, expired int64, err error) {
	tag, err := s.pool.Exec(ctx, `UPDATE job SET status = 'failed', error = 'interrupted', finished_at = now(), updated_at = now()
		WHERE status = 'running' AND updated_at < now() - make_interval(secs => $1)`, s.SweepRunningAfter.Seconds())
	if err != nil {
		return 0, 0, fmt.Errorf("jobs: sweep running: %w", err)
	}
	interrupted = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `UPDATE job SET status = 'failed', error = 'expired', finished_at = now(), updated_at = now()
		WHERE status = 'queued' AND created_at < now() - make_interval(secs => $1)`, s.SweepQueuedAfter.Seconds())
	if err != nil {
		return interrupted, 0, fmt.Errorf("jobs: sweep queued: %w", err)
	}
	return interrupted, tag.RowsAffected(), nil
}

// Purge deletes jobs in a final state (done or failed) that finished more
// than RetainFor ago, at most purgeBatch (10 000) rows per call, oldest
// first, and returns how many it deleted. Queued and running rows are never
// deleted (Sweep fails them first). Safe to run from several processes.
func (s *Service) Purge(ctx context.Context) (int64, error) {
	retain := s.RetainFor
	if retain <= 0 {
		retain = defaultRetainFor
	}
	limit := s.purgeBatch
	if limit <= 0 {
		limit = defaultPurgeBatch
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM job WHERE id IN (
			SELECT id FROM job WHERE status IN ('done', 'failed') AND finished_at < now() - make_interval(secs => $1)
			ORDER BY finished_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, retain.Seconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("jobs: purge: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Service) handleMessage(ctx context.Context, body []byte) error {
	var m message
	if err := json.Unmarshal(body, &m); err != nil || m.ID == uuid.Nil {
		slog.Error("jobs: dropping malformed message")
		return nil
	}
	a, _ := queue.AttemptFrom(ctx)
	return s.handleJob(ctx, m.ID, a)
}

// handleJob runs the job once if it can be claimed. The returned error is the
// queue's cue to redeliver; nil acknowledges the message. It returns an error
// only when ctx is cancelled before the job reached a final state.
//
//   - queued: claimed and run.
//   - running with a stale heartbeat (its worker died): on the last delivery
//     it fails with "interrupted", otherwise it is claimed and run again.
//   - running with a live heartbeat: the handler waits, polling the row every
//     ProgressInterval, until the job is final (acknowledge) or its heartbeat
//     goes stale (as above). A healthy concurrent run burns no delivery.
//   - done, failed or unknown: acknowledged and skipped.
func (s *Service) handleJob(ctx context.Context, id uuid.UUID, a queue.Attempt) error {
	for {
		if a.Last {
			n, err := s.markInterrupted(ctx, id)
			if err != nil {
				return err
			}
			if n > 0 {
				slog.Warn("jobs: job interrupted by a dead worker", "job", id)
				return nil
			}
		}
		runID := uuid.New()
		var typ string
		var params []byte
		err := s.pool.QueryRow(ctx, `UPDATE job SET status = 'running', run_id = $3, updated_at = now()
			WHERE id = $1 AND (status = 'queued' OR (status = 'running' AND updated_at < now() - make_interval(secs => $2)))
			RETURNING type, params`, id, s.StaleAfter.Seconds(), runID).Scan(&typ, &params)
		if err == nil {
			return s.run(ctx, id, runID, typ, params)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("jobs: claim: %w", err)
		}
		var status string
		err = s.pool.QueryRow(ctx, `SELECT status FROM job WHERE id = $1`, id).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("jobs: read job: %w", err)
		}
		if status != "running" {
			return nil
		}
		// Running with a live heartbeat: wait and look again.
		t := time.NewTimer(s.ProgressInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (s *Service) markInterrupted(ctx context.Context, id uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE job SET status = 'failed', error = 'interrupted', finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'running' AND updated_at < now() - make_interval(secs => $2)`, id, s.StaleAfter.Seconds())
	if err != nil {
		return 0, fmt.Errorf("jobs: mark interrupted: %w", err)
	}
	return tag.RowsAffected(), nil
}

// shutdownGrace is how long, after shutdown is requested, a handler that is
// slow to honor its cancelled context is waited for before it is abandoned.
const shutdownGrace = 5 * time.Second

type outcome struct {
	result any
	err    error
}

// run executes a job claimed under runID and stores its outcome; every write
// is conditioned on runID, so a runner that lost the job (taken over after a
// stale heartbeat) neither writes nor has its result kept, and its handler
// context is cancelled. It returns ctx's error only when shutdown left the
// job unfinished; a job that reached a final state is acknowledged even if
// shutdown was requested meanwhile.
func (s *Service) run(ctx context.Context, id, runID uuid.UUID, typ string, params []byte) error {
	reg, ok := s.handler(typ)
	if !ok {
		s.finish(ctx, id, runID, `status = 'failed', error = 'unknown job type'`)
		return nil
	}
	hctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	pw := newProgressWriter(s.ProgressInterval, func(c context.Context, v float64) error {
		tag, err := s.pool.Exec(c, `UPDATE job SET progress = GREATEST(progress, $3), updated_at = now()
			WHERE id = $1 AND run_id = $2 AND status = 'running'`, id, runID, v)
		if err == nil && tag.RowsAffected() == 0 {
			return errLostOwnership
		}
		return err
	})
	pw.lost = cancel
	pwDone := make(chan struct{})
	go func() { defer close(pwDone); pw.run(hctx) }()
	stop := func() { cancel(); <-pwDone }

	// The handler runs in its own goroutine so that the timeout holds even
	// if it ignores its context; its late result is dropped (buffered
	// channel, nobody reads it).
	out := make(chan outcome, 1)
	go func() {
		r, err := s.call(hctx, reg.h, params, pw.report, id)
		out <- outcome{r, err}
	}()

	var o outcome
	select {
	case o = <-out:
	case <-hctx.Done():
		switch {
		case ctx.Err() != nil: // shutdown: give the handler a moment to finish
			select {
			case o = <-out:
			case <-time.After(shutdownGrace):
				stop()
				return ctx.Err()
			}
		case errors.Is(hctx.Err(), context.DeadlineExceeded):
			stop()
			s.finish(ctx, id, runID, `status = 'failed', error = 'timeout'`)
			return nil
		default: // ownership lost: the new owner has the job
			stop()
			return nil
		}
	}
	stop()

	switch {
	case o.err != nil && ctx.Err() != nil:
		return ctx.Err() // shutdown: leave the row for redelivery
	case o.err != nil && errors.Is(hctx.Err(), context.DeadlineExceeded):
		s.finish(ctx, id, runID, `status = 'failed', error = 'timeout'`)
	case o.err != nil:
		msg := o.err.Error()
		if errors.Is(o.err, errPanic) {
			msg = "internal error"
		}
		s.finish(ctx, id, runID, `status = 'failed', error = $3`, msg)
	default:
		raw, merr := json.Marshal(o.result)
		switch {
		case merr != nil:
			slog.Error("jobs: result not encodable", "job", id, "type", typ)
			s.finish(ctx, id, runID, `status = 'failed', error = 'internal error'`)
		case len(raw) > maxResultSize:
			s.finish(ctx, id, runID, `status = 'failed', error = 'result too large'`)
		default:
			s.finish(ctx, id, runID, `status = 'done', progress = 1, result = $3`, raw)
		}
	}
	return nil
}

var (
	errPanic         = errors.New("handler panic")
	errLostOwnership = errors.New("jobs: job was taken over by another run")
)

// call runs h, turning a panic into errPanic. The panic value is not logged:
// it may carry the job's params.
func (s *Service) call(ctx context.Context, h Handler, params []byte, progress func(float64), id uuid.UUID) (res any, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("jobs: handler panicked", "job", id, "stack", string(debug.Stack()))
			res, err = nil, errPanic
		}
	}()
	return h(ctx, params, progress)
}

// finish sets the final columns of a job still running under runID, even when
// ctx is already cancelled. set is the SET clause; args fill $3 onwards.
func (s *Service) finish(ctx context.Context, id, runID uuid.UUID, set string, args ...any) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalWriteTimeout)
	defer cancel()
	q := `UPDATE job SET ` + set + `, finished_at = now(), updated_at = now() WHERE id = $1 AND run_id = $2 AND status = 'running'`
	if _, err := s.pool.Exec(ctx, q, append([]any{id, runID}, args...)...); err != nil {
		slog.Error("jobs: could not store the outcome", "job", id, "error", err)
	}
}

// progressWriter keeps the largest progress reported and flushes it every
// interval; the flush also refreshes the job's heartbeat.
type progressWriter struct {
	interval time.Duration
	flush    func(context.Context, float64) error
	// lost, when set, is called once if a flush reports errLostOwnership;
	// the writer then stops.
	lost func()

	mu  sync.Mutex
	cur float64
}

func newProgressWriter(interval time.Duration, flush func(context.Context, float64) error) *progressWriter {
	return &progressWriter{interval: interval, flush: flush}
}

// report records v, clamped to [0,1]; it never lowers the progress.
func (w *progressWriter) report(v float64) {
	if v != v { // NaN
		return
	}
	v = min(max(v, 0), 1)
	w.mu.Lock()
	w.cur = max(w.cur, v)
	w.mu.Unlock()
}

// run flushes every interval until ctx is done or ownership is lost.
func (w *progressWriter) run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.mu.Lock()
			v := w.cur
			w.mu.Unlock()
			err := w.flush(ctx, v)
			if errors.Is(err, errLostOwnership) {
				if w.lost != nil {
					w.lost()
				}
				return
			}
			if err != nil && ctx.Err() == nil {
				slog.Warn("jobs: could not write progress", "error", err)
			}
		}
	}
}
