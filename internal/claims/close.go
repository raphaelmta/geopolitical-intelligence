package claims

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"geoint/internal/collection"
	"geoint/internal/identity"
	"geoint/internal/sources"
)

// ErrShrunkCollection: the collection saw too few of the claims that were
// open before it, which is more likely a truncated or broken publication than
// a real change, so nothing was closed.
var ErrShrunkCollection = errors.New("collection shrank beyond guard; nothing closed")

// closeAttempts bounds how many times CloseUnseen runs its transaction when
// it hits a serialization failure or a deadlock.
const closeAttempts = 3

// closeBackoff is the wait before the second attempt, doubled after that.
var closeBackoff = 100 * time.Millisecond

// CloseUnseen closes the open fact relationships and the open company
// versions of src that the full collection cur did not see again. minRatio
// is the shrink guard (0.9 in production): when, in either table, fewer than
// minRatio of the rows open before cur were seen by cur, it returns
// ErrShrunkCollection and closes nothing. It returns the number of rows closed.
//
// Only a full collection closes anything: a slice returns 0 at once. The
// scope, the source, the reference date and the observed date are read from
// the STORED collection row (cur only names it): closing is irreversible, and
// a caller's copy could disagree with what was recorded. A slice cannot tell
// a departure from a row it simply did not load; departures are recorded by
// the next full collection.
//
// A row belongs to src when its first collection is a collection of src.
// Known limitation: a relationship is owned by the source that first wrote
// it; if another source later asserts the same open natural key, that only
// advances last_seen on the row, which stays closable by the first source
// alone (so no two connectors may write the same rel_type between two
// entity-only ends: see the connector package's rel-type overlap test). Only
// kind = 'fact' relationships are closed: inferences and analyst rows are
// never re-seen by a loader. Rows are kept: a claim that comes back later is
// a new open row. A company version only gets closed_collection_id (it has
// no validity columns).
//
// The closing interval of a relationship (valid_to_before is exclusive):
//   - valid_to_before = cur's observed date + 1 day. The row is known absent
//     in the source's state as observed on that date (for Receita, the
//     publication date of cur's edition, whose snapshot was taken on or
//     before it), so it ended on or before it and the first day on which it
//     certainly no longer holds is the day after. The reference date is NOT
//     used: for Receita it is the first day of the edition month, earlier
//     than the snapshot, which would date exits early in a month before they
//     happened.
//   - valid_to_after = the reference date of the collection that saw it last
//     (possibly a slice): a true lower bound. For Receita that is the first
//     day of the last edition that listed it; that edition's observed
//     (publication) date is not used, because its snapshot was extracted some
//     days before publication and the link may have ended in between.
//
// The transaction takes the identity lock first (identity.LockTx), so an
// analyst merge or split that re-points a relationship waits for the close
// instead of making this REPEATABLE READ transaction fail with a
// serialization error after hours of loading. Lock order, the same in every
// writer that takes both: the identity advisory lock, then row locks
// (person_record in id order, then relationship / company_version rows).
//
// Without an earlier done collection of src there is nothing to compare
// with and nothing is closed. An earlier done collection with a later
// reference date than cur, or an unseen row last seen at a later date than
// cur's, is an error: closing would put the end of a claim before its last
// sighting. All checks and both updates run in one REPEATABLE READ
// transaction, retried on serialization failure or deadlock.
func (s *Store) CloseUnseen(ctx context.Context, src sources.Source, cur collection.Collection, minRatio float64) (closed int64, err error) {
	if !(minRatio > 0 && minRatio <= 1) {
		return 0, fmt.Errorf("claims: close unseen: minRatio must be in (0, 1], got %v", minRatio)
	}
	if cur.SourceID != src.ID {
		return 0, fmt.Errorf("claims: close unseen: collection %d belongs to source %d, not %s", cur.ID, cur.SourceID, src.Key)
	}
	wait := closeBackoff
	for attempt := 1; ; attempt++ {
		err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
			closed, err = closeUnseen(ctx, tx, src, cur, minRatio)
			return err
		})
		if err == nil {
			return closed, nil
		}
		if attempt == closeAttempts || !retryable(err) {
			return 0, fmt.Errorf("claims: close unseen: collection %d: %w", cur.ID, err)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("claims: close unseen: collection %d: %w", cur.ID, errors.Join(err, ctx.Err()))
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// retryable reports a serialization failure or a deadlock: the transaction
// lost a race and may succeed when run again.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// Statements of CloseUnseen. Named parameters: @source (source id), @cur
// (cur's id), @ref (cur's reference date), @obs (cur's observed date). A row is "open before cur" when
// it is open and its first collection is an earlier collection of the
// source, and "unseen" when, among those, it was last seen before cur (a row
// never marked seen counts as seen by its first collection). All are
// set-based: each table is scanned once to count and once to update.
const (
	sqlCurrent = `SELECT c.source_id, c.reference_date, c.observed_date, coalesce((c.scope ->> 'full')::boolean, false),
			(SELECT max(p.reference_date) FROM collection p
			 WHERE p.source_id = c.source_id AND p.status = 'done' AND p.id <> c.id)
		FROM collection c WHERE c.id = $1`

	sqlSourceBefore = `(SELECT id FROM collection WHERE source_id = @source AND id < @cur)`
	sqlRelLastSeen  = `coalesce(r.last_seen_collection_id, r.first_collection_id)`
	sqlCVLastSeen   = `coalesce(v.last_seen_collection_id, v.first_collection_id)`

	// open before cur, unseen, unseen but last seen after cur's reference date.
	sqlRelCount = `SELECT count(*),
			count(*) FILTER (WHERE ` + sqlRelLastSeen + ` < @cur),
			count(*) FILTER (WHERE ` + sqlRelLastSeen + ` < @cur AND lc.reference_date > @ref::date)
		FROM relationship r JOIN collection lc ON lc.id = ` + sqlRelLastSeen + `
		WHERE r.closed_collection_id IS NULL AND r.kind = 'fact' AND r.first_collection_id IN ` + sqlSourceBefore
	sqlRelClose = `UPDATE relationship r
		SET closed_collection_id = @cur, valid_to_after = lc.reference_date, valid_to_before = @obs::date + 1
		FROM collection lc
		WHERE lc.id = ` + sqlRelLastSeen + ` AND ` + sqlRelLastSeen + ` < @cur
		  AND r.closed_collection_id IS NULL AND r.kind = 'fact' AND r.first_collection_id IN ` + sqlSourceBefore
	sqlCVCount = `SELECT count(*),
			count(*) FILTER (WHERE ` + sqlCVLastSeen + ` < @cur),
			count(*) FILTER (WHERE ` + sqlCVLastSeen + ` < @cur AND lc.reference_date > @ref::date)
		FROM company_version v JOIN collection lc ON lc.id = ` + sqlCVLastSeen + `
		WHERE v.closed_collection_id IS NULL AND v.first_collection_id IN ` + sqlSourceBefore
	sqlCVClose = `UPDATE company_version v SET closed_collection_id = @cur
		WHERE ` + sqlCVLastSeen + ` < @cur
		  AND v.closed_collection_id IS NULL AND v.first_collection_id IN ` + sqlSourceBefore
)

// closeTables are the tables CloseUnseen works on, with their count and
// close statements.
var closeTables = []struct{ name, count, close string }{
	{"relationship", sqlRelCount, sqlRelClose},
	{"company_version", sqlCVCount, sqlCVClose},
}

func closeUnseen(ctx context.Context, tx pgx.Tx, src sources.Source, cur collection.Collection, minRatio float64) (int64, error) {
	// Identity lock first: see CloseUnseen for the lock order.
	if err := identity.LockTx(ctx, tx); err != nil {
		return 0, err
	}
	var (
		sourceID int16
		ref, obs time.Time
		full     bool
		prev     *time.Time
	)
	if err := tx.QueryRow(ctx, sqlCurrent, cur.ID).Scan(&sourceID, &ref, &obs, &full, &prev); err != nil {
		return 0, fmt.Errorf("read collection: %w", err)
	}
	if sourceID != src.ID {
		return 0, fmt.Errorf("collection belongs to source %d, not %s", sourceID, src.Key)
	}
	if !full {
		return 0, nil // a slice never closes (the stored scope decides)
	}
	if prev == nil {
		return 0, nil // first done collection of the source: nothing to compare with
	}
	if prev.After(ref) {
		return 0, fmt.Errorf("reference date %s is older than %s, already collected; nothing closed",
			ref.Format(time.DateOnly), prev.Format(time.DateOnly))
	}
	args := pgx.NamedArgs{"source": src.ID, "cur": cur.ID, "ref": ref, "obs": obs}

	// Every check before any write.
	for _, t := range closeTables {
		var openBefore, unseen, regressed int64
		if err := tx.QueryRow(ctx, t.count, args).Scan(&openBefore, &unseen, &regressed); err != nil {
			return 0, fmt.Errorf("count %s: %w", t.name, err)
		}
		if regressed > 0 {
			return 0, fmt.Errorf("%d unseen %s rows were last seen after reference date %s; nothing closed",
				regressed, t.name, ref.Format(time.DateOnly))
		}
		if seen := openBefore - unseen; openBefore > 0 && float64(seen) < minRatio*float64(openBefore) {
			return 0, fmt.Errorf("%w: %s: %d of %d open rows seen again, below %.0f%%",
				ErrShrunkCollection, t.name, seen, openBefore, minRatio*100)
		}
	}
	var closed int64
	for _, t := range closeTables {
		tag, err := tx.Exec(ctx, t.close, args)
		if err != nil {
			return 0, fmt.Errorf("close %s: %w", t.name, err)
		}
		closed += tag.RowsAffected()
	}
	return closed, nil
}
