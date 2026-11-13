// Package freshness reports, for each source of the catalogue, how current
// its data is: when it was last collected, when the next publication is
// expected and whether the latest collection failed.
package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/sources"
)

// Status of a source.
type Status string

// Statuses.
const (
	// Fresh: the last done full-scope collection was observed within
	// StaleAfter (see SourceStatus.LastObservedDate).
	Fresh Status = "fresh"
	// Stale: the last done full-scope collection was observed more than
	// StaleAfter ago.
	Stale Status = "stale"
	// Never: no full-scope collection of the source ever finished.
	Never Status = "never"
	// Failed: the most recent finished collection of the source failed.
	Failed Status = "failed"
)

// SourceStatus is the freshness of one source.
//
// LastCollectedAt is when the last done full-scope collection finished,
// LastReferenceDate the date of its data (a date, at UTC midnight) and
// LastObservedDate the date by which that data was observed (for an edition
// source its publication date, otherwise the day the collection started);
// NextExpectedAt is LastReferenceDate + PublishEvery. All four are nil when
// the status is Never. For a source published as editions
// (sources.Source.Editions) staleness counts from LastObservedDate: an
// edition published on the 20th of its month is as current as its
// publication, not as old as the month's first day. Other sources count
// from LastReferenceDate. LastError is the full error text of the most recent
// collection, only while it is Failed: the most recent finished collection
// failed and no successful check (source_check) came after it. Callers decide
// who sees it and how much of it. LastStats are the counters of the last done
// collection of any scope.
type SourceStatus struct {
	Key, Name         string
	Status            Status
	LastCollectedAt   *time.Time
	NextExpectedAt    *time.Time
	LastReferenceDate *time.Time
	LastObservedDate  *time.Time
	// LastCheckedAt is the later of LastCollectedAt and the last successful
	// check that found nothing new to collect (source_check).
	LastCheckedAt *time.Time
	LastError     string
	LastStats     map[string]int64
	// PendingRequestAt is when the newest full request still waiting to
	// start was made (published, and no full-scope collection started nor a
	// successful check recorded after it, within QueuedRequestBlocks): the
	// one that blocks a new full request (see Request). Nil when none waits.
	PendingRequestAt *time.Time
}

// Service computes source freshness.
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
	// Catalogue is the list of sources the scheduler considers (default
	// sources.All); only those with HasConnector are ever requested.
	Catalogue []sources.Source
	// RunningStaleAfter is how long a live collection may stay running
	// before the scheduler logs it as possibly stuck (default
	// DefaultRunningStaleAfter); a live run is never failed for its age.
	// See Tick.
	RunningStaleAfter time.Duration
	// QueuedRequestBlocks is how long a published request that no
	// collection has followed yet blocks a new one (default
	// DefaultQueuedRequestBlocks). See TickWith.
	QueuedRequestBlocks time.Duration
}

// New returns a Service on p; now is the clock (nil means time.Now).
func New(p *pgxpool.Pool, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: p, now: now, Catalogue: sources.All, RunningStaleAfter: DefaultRunningStaleAfter}
}

// List returns the freshness of every source of the catalogue, in catalogue order.
func (s *Service) List(ctx context.Context) ([]SourceStatus, error) {
	now := s.now()
	out := make([]SourceStatus, 0, len(sources.All))
	for _, src := range sources.All {
		st, err := s.one(ctx, src, now)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// lastFinished is the most recent finished (done or failed) collection by
// id: a running collection changes nothing until it ends. lastFull is the
// done full-scope collection with the newest data (graph uses the same row;
// staleness counts from its observed_date); lastDone the most recent done collection of any scope; check the
// last successful check that found nothing new (source_check); pending the
// newest full request still waiting to start (waitingSQL; $2 is
// QueuedRequestBlocks in seconds).
const oneSQL = `
	SELECT lf.status, lf.error, lf.finished_at, full_c.reference_date, full_c.observed_date, full_c.finished_at, done_c.stats, chk.last_checked_at,
		(SELECT max(r.requested_at) FROM collection_request r WHERE ` + waitingSQL + `)
	FROM (SELECT 1) one
	LEFT JOIN LATERAL (SELECT status, error, finished_at FROM collection
		WHERE source_id = $1 AND status <> 'running' ORDER BY id DESC LIMIT 1) lf ON true
	LEFT JOIN LATERAL (SELECT reference_date, observed_date, finished_at FROM collection
		WHERE source_id = $1 AND status = 'done' AND (scope ->> 'full')::boolean IS TRUE
		ORDER BY reference_date DESC, id DESC LIMIT 1) full_c ON true
	LEFT JOIN LATERAL (SELECT stats FROM collection
		WHERE source_id = $1 AND status = 'done' ORDER BY id DESC LIMIT 1) done_c ON true
	LEFT JOIN source_check chk ON chk.source_id = $1`

// failedNow: the most recent finished collection failed and no successful
// check (one that found nothing new) came after it. A check after the failure
// shows the source is reachable and up to date again.
func failedNow(lastStatus *string, lastFinished, lastCheck *time.Time) bool {
	if lastStatus == nil || *lastStatus != collection.StatusFailed {
		return false
	}
	return lastCheck == nil || lastFinished == nil || !lastCheck.After(*lastFinished)
}

func (s *Service) one(ctx context.Context, src sources.Source, now time.Time) (SourceStatus, error) {
	st := SourceStatus{Key: src.Key, Name: src.Name}
	var (
		lastStatus, lastErr     *string
		lastFinished, lastCheck *time.Time
		ref, observed, finished *time.Time
		stats                   []byte
	)
	err := s.pool.QueryRow(ctx, oneSQL, src.ID, s.queuedRequestBlocks().Seconds()).
		Scan(&lastStatus, &lastErr, &lastFinished, &ref, &observed, &finished, &stats, &lastCheck, &st.PendingRequestAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return st, fmt.Errorf("freshness: %s: %w", src.Key, err)
	}
	if len(stats) > 0 {
		if err := json.Unmarshal(stats, &st.LastStats); err != nil {
			return st, fmt.Errorf("freshness: %s: decode stats: %w", src.Key, err)
		}
	}
	if ref != nil {
		r := time.Date(ref.Year(), ref.Month(), ref.Day(), 0, 0, 0, 0, time.UTC)
		next := r.Add(src.PublishEvery)
		st.LastReferenceDate, st.NextExpectedAt, st.LastCollectedAt = &r, &next, finished
		o := r
		if observed != nil {
			o = time.Date(observed.Year(), observed.Month(), observed.Day(), 0, 0, 0, 0, time.UTC)
		}
		st.LastObservedDate = &o
	}
	st.LastCheckedAt = later(finished, lastCheck)
	switch {
	case failedNow(lastStatus, lastFinished, lastCheck):
		st.Status = Failed
		if lastErr != nil {
			st.LastError = *lastErr
		}
	case ref == nil:
		st.Status = Never
	case staleFrom(src, st).Add(src.StaleAfter).Before(now):
		st.Status = Stale
	default:
		st.Status = Fresh
	}
	return st, nil
}

// staleFrom is the date staleness counts from (see SourceStatus).
func staleFrom(src sources.Source, st SourceStatus) time.Time {
	if src.Editions && st.LastObservedDate != nil {
		return *st.LastObservedDate
	}
	return *st.LastReferenceDate
}

// later returns the later of two optional instants.
func later(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil || a.After(*b):
		return a
	default:
		return b
	}
}
