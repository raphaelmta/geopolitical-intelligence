package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/connector/camara"
	"geoint/internal/connector/cvm"
	"geoint/internal/connector/receita"
	"geoint/internal/connector/senado"
	"geoint/internal/connector/tse"
	"geoint/internal/freshness"
	"geoint/internal/graph"
	"geoint/internal/identity"
	"geoint/internal/jobs"
	"geoint/internal/queue"
	"geoint/internal/rawstore"
	"geoint/internal/search"
	"geoint/internal/sources"
)

func requireEnv(names ...string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, n := range names {
		v := os.Getenv(n)
		if v == "" {
			return nil, fmt.Errorf("%s is not set", n)
		}
		out[n] = v
	}
	return out, nil
}

// commonNameThreshold parses IDENTITY_COMMON_NAME_THRESHOLD: empty means
// the default, anything else must be a positive integer.
func commonNameThreshold(v string) (int, error) {
	if v == "" {
		return identity.DefaultCommonNameThreshold, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("IDENTITY_COMMON_NAME_THRESHOLD must be a positive integer, got %q", v)
	}
	return n, nil
}

// closeUnseenMinRatio is the shrink guard of CloseUnseen: a snapshot that
// sees again less than this share of the claims open before it closes nothing
// and fails.
const closeUnseenMinRatio = 0.9

// The services the collection hooks call (interfaces so tests can fake them).
type unseenCloser interface {
	CloseUnseen(ctx context.Context, src sources.Source, col collection.Collection, minRatio float64) (int64, error)
}
type identityResolver interface {
	Resolve(ctx context.Context, collectionID int64) (identity.Result, error)
}
type searchRebuilder interface {
	RefreshCollection(ctx context.Context, collectionID int64) (search.Stats, bool, error)
	RebuildIfDirty(ctx context.Context) (search.Stats, bool, error)
}

// collectionHooks wires the steps that follow a collection's Load:
// CloseUnseen, Resolve and, once the collection is done, the search refresh.
//
// The refresh runs in the Finished hook, after the collection is marked done:
// its claims are loaded correctly whatever happens to search, so a failed
// rebuild is logged as an error with the collection id and nothing else; the
// previous search tables keep serving. RefreshCollection first marks the search tables
// dirty (durably), so a rebuild that fails or is cut short by shutdown is
// redone at the next worker start (searchCatchUp) or after the next
// collection. It runs inside the queue handler, whose heartbeat keeps the
// message in progress during the rebuild (~14 min or more at national scale);
// meanwhile this worker takes no other ingest message.
func collectionHooks(c unseenCloser, r identityResolver, s searchRebuilder) connector.Hooks {
	return connector.Hooks{
		CloseUnseen: func(ctx context.Context, src sources.Source, col collection.Collection) (int64, error) {
			closed, err := c.CloseUnseen(ctx, src, col, closeUnseenMinRatio)
			if err != nil {
				return 0, err
			}
			slog.Info("unseen claims closed", "source", src.Key, "collection", col.ID, "closed", closed)
			return closed, nil
		},
		Resolve: func(ctx context.Context, collectionID int64) error {
			res, err := r.Resolve(ctx, collectionID)
			if err != nil {
				return err
			}
			// Numbers only: the distribution of distinct CPF fragments per
			// touched name (1, 2-10, 11-100, 101-1000, >1000) is what the
			// national load reports to choose and justify the threshold.
			b := res.NamesByFragments
			slog.Info("identities resolved", "collection", collectionID,
				"auto_merged", res.AutoMerged, "auto_merged_groups", res.AutoMergedGroups,
				"proposed", res.Proposed, "proposals_created", res.ProposalsCreated,
				"names_n1", b[0], "names_n2_10", b[1], "names_n11_100", b[2], "names_n101_1000", b[3], "names_n_over_1000", b[4],
				"earlier_collections_recovered", res.Recovered)
			return nil
		},
		Finished: func(ctx context.Context, collectionID int64) {
			st, rebuilt, err := s.RefreshCollection(ctx, collectionID)
			switch {
			case err != nil:
				slog.Error("search rebuild failed; the previous search tables keep serving",
					"collection", collectionID, "error", err)
			case rebuilt:
				slog.Info("search rebuilt", "collection", collectionID, "entities", st.Entities,
					"frequent_tokens", st.FrequentTokens, "duration", st.Duration)
			default:
				slog.Info("search already rebuilt by another worker", "collection", collectionID)
			}
		},
	}
}

// searchCatchUp rebuilds the search tables in the background when they are
// dirty (a rebuild was interrupted, or data exists that was never indexed).
// It returns at once; the returned channel is closed when the rebuild ends or
// ctx is cancelled. Errors are logged.
func searchCatchUp(ctx context.Context, s searchRebuilder) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		st, rebuilt, err := s.RebuildIfDirty(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			slog.Warn("search catch-up interrupted by shutdown; it runs again at the next start")
		case err != nil:
			slog.Error("search catch-up rebuild failed", "error", err)
		case rebuilt:
			slog.Info("search caught up", "entities", st.Entities, "frequent_tokens", st.FrequentTokens,
				"duration", st.Duration)
		}
	}()
	return done
}

// Connections the worker may hold at once (the pool must allow at least
// workerMinConns; see openWorkerPool):
//   - an ingest run: 1 run lock (collection.Start) + 1 load lock (the
//     connector's session advisory lock) + 4 parse/COPY workers (Receita,
//     TSE) = 6;
//   - search: 1 for the rebuild of the Finished hook and 1 for the catch-up
//     rebuild at start (it holds the rebuild lock; the other waits on it
//     holding its connection) = 2;
//   - jobs: 2 concurrent graph jobs + 1 for the sweeper/heartbeat writes = 3;
//   - the scheduler tick (its transaction; FailDead between ticks) = 1.
const (
	workerMinConns = 12
	// workerDefaultConns is the pool size when DATABASE_URL sets no
	// pool_max_conns (pgx's own default, max(4, CPUs), is too small).
	workerDefaultConns = 16
)

// openWorkerPool opens the worker's pool. Without pool_max_conns in the URL
// the pool allows workerDefaultConns; an explicit value below workerMinConns
// is refused before anything starts.
func openWorkerPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL: %w", err)
	}
	if err := sizeWorkerPool(cfg, strings.Contains(url, "pool_max_conns")); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// sizeWorkerPool applies the worker's pool rule to cfg (explicit: the URL
// sets pool_max_conns).
func sizeWorkerPool(cfg *pgxpool.Config, explicit bool) error {
	if !explicit {
		cfg.MaxConns = workerDefaultConns
		return nil
	}
	if cfg.MaxConns < workerMinConns {
		return fmt.Errorf("DATABASE_URL sets pool_max_conns=%d; the worker needs at least %d connections "+
			"(collection run lock, load lock, 4 COPY workers, search rebuild, graph jobs, scheduler); "+
			"use pool_max_conns=%d or remove it", cfg.MaxConns, workerMinConns, workerDefaultConns)
	}
	return nil
}

// spoolStaleAfter: a spool file untouched this long belongs to no live
// download (a download writes it continuously, the upload follows at once).
const spoolStaleAfter = 15 * time.Minute

// worker consumes collection requests until the context is cancelled.
func worker(ctx context.Context) error {
	env, err := requireEnv("DATABASE_URL", "NATS_URL", "S3_ENDPOINT", "S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_BUCKET")
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	threshold, err := commonNameThreshold(os.Getenv("IDENTITY_COMMON_NAME_THRESHOLD"))
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	schedOn, err := schedulerEnabled(os.Getenv("SCHEDULER_ENABLED"))
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	pool, err := openWorkerPool(ctx, env["DATABASE_URL"])
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	defer pool.Close()
	// Runs left behind by a killed worker are failed now, whatever
	// SCHEDULER_ENABLED says (and again before every collection).
	connector.FailDeadRuns(ctx, pool)
	// Spool files of downloads cut short by a killed worker (several GB each).
	if n, err := rawstore.SweepSpool(os.Getenv("RAW_SPOOL_DIR"), spoolStaleAfter); err != nil {
		slog.Warn("could not remove every stale spool file", "removed", n, "error", err)
	} else if n > 0 {
		slog.Info("stale spool files removed", "removed", n)
	}
	raw, err := rawstore.NewS3(env["S3_ENDPOINT"], env["S3_ACCESS_KEY"], env["S3_SECRET_KEY"], env["S3_BUCKET"])
	if err != nil {
		return err
	}
	q, err := queue.Connect(env["NATS_URL"])
	if err != nil {
		return err
	}
	defer q.Close()

	registerConnectors()

	resolver := &identity.Resolver{Pool: pool, CommonNameThreshold: threshold}
	searchSvc := search.New(pool)
	hooks := collectionHooks(claims.NewStore(pool), resolver, searchSvc)
	jobSvc := jobs.New(pool, q)
	jobs.RegisterGraphHandlers(jobSvc, graph.New(pool))

	slog.Info("worker started")
	// Rebuild search if a previous worker was stopped mid-rebuild, without
	// delaying the consumers; stopped (and waited for) when the worker exits.
	catchUpCtx, stopCatchUp := context.WithCancel(ctx)
	catchUp := searchCatchUp(catchUpCtx, searchSvc)
	defer func() { stopCatchUp(); <-catchUp }()
	// Keeps every source fresh without anyone asking; stopped (and waited
	// for) when the worker exits.
	schedCtx, stopSched := context.WithCancel(ctx)
	sched := startScheduler(schedCtx, schedOn, func(ctx context.Context) ([]string, error) {
		return freshness.New(pool, nil).Tick(ctx, q)
	}, schedulerFirstDelay, schedulerInterval)
	defer func() { stopSched(); <-sched }()
	return runBoth(ctx,
		func(ctx context.Context) error {
			return q.Consume(ctx, queue.StreamIngest, "worker-ingest", "ingest.request.*",
				func(ctx context.Context, body []byte) error {
					return handleRequest(ctx, body, func(ctx context.Context, c connector.Connector, req connector.Request) error {
						_, err := connector.Collect(ctx, c, req, pool, raw, hooks)
						return err
					})
				})
		},
		jobSvc.RunWorker)
}

// runBoth runs both loops until ctx is cancelled. An error from either stops
// the other and is returned.
func runBoth(ctx context.Context, a, b func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	for _, f := range []func(context.Context) error{a, b} {
		go func() {
			err := f(ctx)
			if err != nil {
				cancel()
			}
			errs <- err
		}()
	}
	return errors.Join(<-errs, <-errs)
}

// registerConnectors registers the source connectors. Done here rather than
// in each package's init because their configuration comes from the
// environment. RECEITA_BASE_URL overrides the official Receita WebDAV folder,
// TSE_BASE_URL the official TSE CDN folder (default election years),
// CAMARA_BASE_URL and SENADO_BASE_URL the roots of the Câmara and Senado
// open-data APIs (default legislatures), CVM_BASE_URL the CVM FRE folder.
func registerConnectors() {
	registerOnce.Do(func() {
		connector.Register(receita.New(os.Getenv("RECEITA_BASE_URL")))
		connector.Register(tse.New(os.Getenv("TSE_BASE_URL"), nil))
		connector.Register(camara.New(os.Getenv("CAMARA_BASE_URL"), nil))
		connector.Register(senado.New(os.Getenv("SENADO_BASE_URL"), nil))
		connector.Register(cvm.New(os.Getenv("CVM_BASE_URL")))
	})
}

var registerOnce sync.Once

// lookupConnector finds the connector of a source (connector.Get; tests
// replace it so they never register into the process-wide registry).
var lookupConnector = connector.Get

// handleRequest decodes a request and runs it. The returned error decides
// redelivery: nil acknowledges the message, an error makes the queue retry.
//
//   - Requests that can never succeed (malformed, invalid, unknown source or
//     connector) are logged and acknowledged.
//   - A run that failed after its collection was opened is recorded in that
//     collection and acknowledged; retrying is an explicit new `collect`.
//   - A full request whose newest published edition is already loaded
//     (connector.ErrNothingNew) is a successful check: logged as "nothing new
//     to collect" and acknowledged.
//   - A run that never started (for example the database is down) or that was
//     cut short by shutdown is returned, so the message is redelivered.
func handleRequest(ctx context.Context, body []byte, run func(context.Context, connector.Connector, connector.Request) error) error {
	var req connector.Request
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Error("dropping malformed ingest request", "error", err)
		return nil
	}
	if req.ReferenceDate.IsZero() {
		slog.Error("dropping ingest request without reference date", "source", req.Source)
		return nil
	}
	if _, ok := sources.ByKey(req.Source); !ok {
		slog.Error("dropping ingest request: unknown source", "source", req.Source)
		return nil
	}
	c, ok := lookupConnector(req.Source)
	if !ok {
		slog.Error("dropping ingest request: no connector registered", "source", req.Source)
		return nil
	}
	slog.Info("collection starting", "source", req.Source, "reference_date", req.ReferenceDate)
	err := run(ctx, c, req)
	var runErr *connector.RunError
	switch {
	case err == nil:
		slog.Info("collection finished", "source", req.Source)
		return nil
	case errors.Is(err, connector.ErrNothingNew):
		// The newest published edition is already loaded: a successful check.
		slog.Info("nothing new to collect", "source", req.Source, "detail", err.Error())
		return nil
	case ctx.Err() != nil:
		return err
	case errors.As(err, &runErr):
		slog.Error("collection failed", "source", req.Source, "collection", runErr.CollectionID, "error", runErr.Err)
		return nil
	default:
		return fmt.Errorf("collection of %s did not start: %w", req.Source, err)
	}
}
