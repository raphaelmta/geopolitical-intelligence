// Package collection records each run that pulls data from a source.
package collection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status values of a collection.
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// InterruptedError is the error text of a collection found dead: running,
// with no live process holding its run lock (see FailDead).
const InterruptedError = "interrupted: worker stopped"

// Scope says what a collection covers: everything, or the listed companies down to Depth.
type Scope struct {
	Full  bool     `json:"full"`
	CNPJs []string `json:"cnpjs,omitempty"`
	Depth int      `json:"depth,omitempty"`
}

// Collection is one run against one source.
//
// ReferenceDate names the data (for an edition source, the edition: the
// first day of its month for Receita). ObservedDate is a date by which the
// source's state was actually observed: for an edition source the edition's
// publication date, otherwise the day the collection started (UTC). It is
// never before ReferenceDate. A claim the collection did not see again is
// known absent in the state observed on ObservedDate, so closing it uses
// ObservedDate + 1 day as the exclusive upper bound (claims.CloseUnseen).
type Collection struct {
	ID            int64
	SourceID      int16
	ReferenceDate time.Time
	ObservedDate  time.Time
	Scope         Scope
	Status        string // running|done|failed
}

// ErrNotRunning is returned when finishing or failing a collection that is not running.
var ErrNotRunning = errors.New("collection is not running")

// sqlInsert opens a running collection. observed_date is $4, or the start
// day (UTC, database clock) when $4 is NULL, and never before the reference
// date: a reference date in the future (refused by the CLI; only tests use
// one) is its own observation date.
const sqlInsert = `INSERT INTO collection (source_id, reference_date, scope, observed_date)
	VALUES ($1, $2, $3::jsonb, GREATEST(coalesce($4::date, (statement_timestamp() AT TIME ZONE 'UTC')::date), $2::date))
	RETURNING id, observed_date`

// Begin opens a running collection observed on its start day, without a run
// lock: FailDead treats it as dead. Connectors run through Start; Begin is
// for callers that finish the collection at once (and for tests).
func Begin(ctx context.Context, p *pgxpool.Pool, sourceID int16, ref time.Time, s Scope) (Collection, error) {
	return BeginObserved(ctx, p, sourceID, ref, time.Time{}, s)
}

// BeginObserved is Begin with an explicit observed date (zero: the start
// day, UTC); it is raised to ref when earlier.
func BeginObserved(ctx context.Context, p *pgxpool.Pool, sourceID int16, ref, observed time.Time, s Scope) (Collection, error) {
	c, args, err := newCollection(sourceID, ref, observed, s)
	if err != nil {
		return Collection{}, err
	}
	if err := p.QueryRow(ctx, sqlInsert, args...).Scan(&c.ID, &c.ObservedDate); err != nil {
		return Collection{}, fmt.Errorf("collection: begin: %w", err)
	}
	return c, nil
}

func newCollection(sourceID int16, ref, observed time.Time, s Scope) (Collection, []any, error) {
	scope, err := json.Marshal(s)
	if err != nil {
		return Collection{}, nil, fmt.Errorf("collection: encode scope: %w", err)
	}
	var obs any
	if !observed.IsZero() {
		obs = observed.UTC().Format(time.DateOnly)
	}
	c := Collection{SourceID: sourceID, ReferenceDate: ref, Scope: s, Status: StatusRunning}
	return c, []any{sourceID, ref, string(scope), obs}, nil
}

// Run lock. Every collection opened by Start holds a session-level advisory
// lock for its whole run on a dedicated connection, so a running collection
// whose lock is free has no live process behind it (FailDead). The lock uses
// the two-integer form of pg_advisory_lock, a key space distinct from every
// bigint key of the platform (identity, scheduler, search rebuild, load
// locks): class = a hash of a fixed label and the current schema (one
// deployment per schema), object = the collection id.
const (
	sqlRunLock    = `SELECT pg_advisory_lock(hashtext('geoint.collection_run.' || current_schema()), $1::int4)`
	sqlRunTryLock = `SELECT pg_try_advisory_lock(hashtext('geoint.collection_run.' || current_schema()), $1::int4)`
	sqlRunUnlock  = `SELECT pg_advisory_unlock(hashtext('geoint.collection_run.' || current_schema()), $1::int4)`
)

// LiveRunSQL returns a boolean SQL expression that is true when some session
// holds the run lock of the collection whose id is the SQL expression idExpr:
// a live process is running it. It reads pg_locks (two-integer advisory keys
// appear as classid = first key as an unsigned 32-bit value, objid = second
// key, objsubid = 2), takes no lock and needs no privilege, so it fits in any
// query of any role, inside any transaction. An id beyond the lock key range
// (never locked by Start) is never live.
func LiveRunSQL(idExpr string) string {
	// CASE, not AND: the oid cast must never see an id beyond the range.
	return fmt.Sprintf(`(CASE WHEN (%[1]s) BETWEEN 1 AND 2147483647 THEN EXISTS (SELECT 1 FROM pg_locks l
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2
		AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND l.classid = (hashtext('geoint.collection_run.' || current_schema())::bigint & 4294967295)::oid
		AND l.objid = (%[1]s)::bigint::oid) ELSE false END)`, idExpr)
}

// releaseTimeout bounds the unlock when a run ends (its context may be gone).
const releaseTimeout = 15 * time.Second

// Run is a running collection whose run lock this process holds on a
// dedicated pool connection. Release it once the collection is finished or
// failed (never before: a running collection without its lock is dead).
type Run struct {
	Collection
	conn *pgxpool.Conn
	once sync.Once
}

// Start opens a running collection like Begin, observed on observed (zero:
// the start day, UTC), and takes its run lock before the row becomes visible:
// the insert and the lock share one transaction on the run's connection, so
// no other process can see the row running without its lock. The pool must
// have a connection to spare for the whole run.
func Start(ctx context.Context, p *pgxpool.Pool, sourceID int16, ref, observed time.Time, s Scope) (*Run, error) {
	c, args, err := newCollection(sourceID, ref, observed, s)
	if err != nil {
		return nil, err
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("collection: start: acquire the run lock connection: %w", err)
	}
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, sqlInsert, args...).Scan(&c.ID, &c.ObservedDate); err != nil {
			return err
		}
		if c.ID > math.MaxInt32 {
			return fmt.Errorf("collection id %d does not fit the run lock key", c.ID)
		}
		_, err := tx.Exec(ctx, sqlRunLock, c.ID)
		return err
	})
	if err != nil {
		// The session lock may have been taken before the rollback; closing
		// the session releases it.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		_ = conn.Conn().Close(cctx)
		conn.Release()
		return nil, fmt.Errorf("collection: start: %w", err)
	}
	return &Run{Collection: c, conn: conn}, nil
}

// Release unlocks the run and returns its connection to the pool. It is
// idempotent. When the unlock fails the connection is closed (which releases
// the lock too).
func (r *Run) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		if _, err := r.conn.Exec(ctx, sqlRunUnlock, r.ID); err != nil {
			slog.Warn("collection: run unlock failed, closing the connection", "collection", r.ID, "error", err)
			_ = r.conn.Conn().Close(ctx)
		}
		r.conn.Release()
	})
}

// FailDead marks failed ("interrupted: worker stopped") every running
// collection whose run lock is free: no live process is running it (its
// worker was killed, or it was opened by Begin and never finished). A
// collection whose lock another session holds is left alone. It returns the
// ids it failed. Safe to call from several processes at once: each id is
// failed under its own lock, by the guarded update collection.Fail uses.
func FailDead(ctx context.Context, p *pgxpool.Pool) ([]int64, error) {
	conn, err := p.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("collection: dead runs: acquire: %w", err)
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, `SELECT id FROM collection WHERE status = $1 ORDER BY id`, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("collection: dead runs: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("collection: dead runs: %w", err)
	}
	var failed []int64
	for _, id := range ids {
		if id > math.MaxInt32 {
			continue // never locked by Start either; left to the age rule
		}
		var free bool
		if err := conn.QueryRow(ctx, sqlRunTryLock, id).Scan(&free); err != nil {
			return failed, fmt.Errorf("collection: dead runs: try lock %d: %w", id, err)
		}
		if !free {
			continue
		}
		tag, uerr := conn.Exec(ctx, `UPDATE collection SET status = $2, finished_at = now(), error = $3
			WHERE id = $1 AND status = $4`, id, StatusFailed, InterruptedError, StatusRunning)
		if _, err := conn.Exec(ctx, sqlRunUnlock, id); err != nil {
			// The connection goes back to the pool holding the lock: close it.
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
			return failed, errors.Join(uerr, fmt.Errorf("collection: dead runs: unlock %d: %w", id, err))
		}
		if uerr != nil {
			return failed, fmt.Errorf("collection: dead runs: fail %d: %w", id, uerr)
		}
		if tag.RowsAffected() == 1 {
			failed = append(failed, id)
		}
	}
	return failed, nil
}

// Finish marks a running collection as done and stores its statistics.
func Finish(ctx context.Context, p *pgxpool.Pool, id int64, stats map[string]int64) error {
	if stats == nil {
		stats = map[string]int64{}
	}
	raw, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("collection: encode stats: %w", err)
	}
	return finish(ctx, p, id, `UPDATE collection SET status='done', finished_at=now(), stats=$2::jsonb
		WHERE id=$1 AND status='running'`, string(raw))
}

// Fail marks a running collection as failed and stores the cause.
func Fail(ctx context.Context, p *pgxpool.Pool, id int64, cause error) error {
	msg := "unknown error"
	if cause != nil {
		msg = cause.Error()
	}
	return finish(ctx, p, id, `UPDATE collection SET status='failed', finished_at=now(), error=$2
		WHERE id=$1 AND status='running'`, msg)
}

func finish(ctx context.Context, p *pgxpool.Pool, id int64, query string, arg any) error {
	tag, err := p.Exec(ctx, query, id, arg)
	if err != nil {
		return fmt.Errorf("collection: update %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("collection %d: %w", id, ErrNotRunning)
	}
	return nil
}

// Get returns the stored collection id.
func Get(ctx context.Context, q Querier, id int64) (Collection, error) {
	var (
		c     Collection
		scope []byte
	)
	err := q.QueryRow(ctx, `SELECT id, source_id, reference_date, observed_date, scope, status FROM collection WHERE id = $1`, id).
		Scan(&c.ID, &c.SourceID, &c.ReferenceDate, &c.ObservedDate, &scope, &c.Status)
	if err != nil {
		return Collection{}, fmt.Errorf("collection: get %d: %w", id, err)
	}
	if err := json.Unmarshal(scope, &c.Scope); err != nil {
		return Collection{}, fmt.Errorf("collection: decode scope: %w", err)
	}
	return c, nil
}

// LastDone returns the finished collection of the source with the newest
// reference date (ties broken by id). Running and failed ones are ignored.
func LastDone(ctx context.Context, p *pgxpool.Pool, sourceID int16) (Collection, bool, error) {
	var (
		c     Collection
		scope []byte
	)
	err := p.QueryRow(ctx, `SELECT id, source_id, reference_date, observed_date, scope, status FROM collection
		WHERE source_id=$1 AND status='done' ORDER BY reference_date DESC, id DESC LIMIT 1`, sourceID).
		Scan(&c.ID, &c.SourceID, &c.ReferenceDate, &c.ObservedDate, &scope, &c.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Collection{}, false, nil
	}
	if err != nil {
		return Collection{}, false, fmt.Errorf("collection: last done: %w", err)
	}
	if err := json.Unmarshal(scope, &c.Scope); err != nil {
		return Collection{}, false, fmt.Errorf("collection: decode scope: %w", err)
	}
	return c, true, nil
}

// Querier is what RecordSourceFile and Get need: a pool, a connection or a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
