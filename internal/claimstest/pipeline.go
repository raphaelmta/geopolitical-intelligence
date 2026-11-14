package claimstest

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/identity"
	"geoint/internal/rawstore"
	"geoint/internal/search"
	"geoint/internal/sources"
)

// CloseUnseenMinRatio is the worker's shrink guard (cmd/geoint,
// closeUnseenMinRatio), repeated here because that package cannot be imported.
const CloseUnseenMinRatio = 0.9

// ObservedAfter is how long after its reference date a fixture edition of an
// edition source is published (its observed date, which bounds what
// CloseUnseen closes): fixed, so the dump never depends on the day a test runs.
const ObservedAfter = 3 * 24 * time.Hour

// Pipeline runs collections the way the worker does (cmd/geoint: the hooks of
// collectionHooks around connector.Collect): Load, with the early identity
// pass (ResolveRecords) handed to it, then CloseUnseen for a snapshot source,
// Resolve, Finish, and the search refresh once the collection is done. Both
// identity passes are the one Resolver, as in the worker. Only Fetch is
// replaced: it returns files already put in the raw store.
type Pipeline struct {
	Pool     *pgxpool.Pool
	Raw      rawstore.Store
	Resolver *identity.Resolver
	Search   *search.Service
	// NoEarlyResolve leaves the early identity pass out of the hooks: the
	// order before task 3 of the load optimisation (relationships written
	// first, everything resolved and re-pointed by Resolve), which loaders
	// without an early pass still follow.
	NoEarlyResolve bool
	// Early and Final hold, per collection id, what the early pass
	// (ResolveRecords, called by the loader) and the Resolve at the end
	// reported. A collection is in Early only if its loader ran the pass.
	Early, Final map[int64]identity.Result
	// SearchErr is the first error of a search refresh (the Finished hook
	// cannot fail a collection, as in the worker).
	SearchErr error
}

// NewPipeline returns a Pipeline whose resolver uses threshold as
// CommonNameThreshold.
func NewPipeline(pool *pgxpool.Pool, raw rawstore.Store, threshold int) *Pipeline {
	return &Pipeline{Pool: pool, Raw: raw, Resolver: &identity.Resolver{Pool: pool, CommonNameThreshold: threshold},
		Search: search.New(pool), Early: map[int64]identity.Result{}, Final: map[int64]identity.Result{}}
}

// Hooks are the worker's collection hooks over p's services.
func (p *Pipeline) Hooks() connector.Hooks {
	h := p.hooks()
	if p.NoEarlyResolve {
		h.ResolveRecords = nil
	}
	return h
}

func (p *Pipeline) hooks() connector.Hooks {
	return connector.Hooks{
		ResolveRecords: func(ctx context.Context, collectionID int64) error {
			res, err := p.Resolver.ResolveRecords(ctx, collectionID)
			if err == nil {
				p.Early[collectionID] = res
			}
			return err
		},
		CloseUnseen: func(ctx context.Context, src sources.Source, col collection.Collection) (int64, error) {
			return claims.NewStore(p.Pool).CloseUnseen(ctx, src, col, CloseUnseenMinRatio)
		},
		Resolve: func(ctx context.Context, collectionID int64) error {
			res, err := p.Resolver.Resolve(ctx, collectionID)
			if err == nil {
				p.Final[collectionID] = res
			}
			return err
		},
		Finished: func(ctx context.Context, collectionID int64) {
			if _, _, err := p.Search.RefreshCollection(ctx, collectionID); err != nil && p.SearchErr == nil {
				p.SearchErr = fmt.Errorf("search refresh after collection %d: %w", collectionID, err)
			}
		},
	}
}

// Collect runs one collection of c over files (already in p.Raw) through
// connector.Collect and returns its error, if any (a failed collection is
// recorded as failed, as in the worker). For an edition source the edition is
// ref, published ObservedAfter later.
func (p *Pipeline) Collect(ctx context.Context, c connector.Connector, files []rawstore.File, ref time.Time, scope collection.Scope) (collection.Collection, error) {
	src, ok := sources.ByKey(c.Key())
	if !ok {
		return collection.Collection{}, fmt.Errorf("claimstest: unknown source %q", c.Key())
	}
	var w connector.Connector = fixed{inner: c, files: files}
	if src.Editions {
		w = fixedEdition{fixed: fixed{inner: c, files: files}, observed: ref.Add(ObservedAfter)}
	}
	return connector.Collect(ctx, w, connector.Request{Source: c.Key(), ReferenceDate: ref, Scope: scope}, p.Pool, p.Raw, p.Hooks())
}

// fixed is a connector whose Fetch returns files already stored.
type fixed struct {
	inner connector.Connector
	files []rawstore.File
}

func (f fixed) Key() string { return f.inner.Key() }

func (f fixed) Fetch(context.Context, time.Time, rawstore.Store) ([]rawstore.File, error) {
	return f.files, nil
}

func (f fixed) Load(ctx context.Context, col collection.Collection, files []rawstore.File, raw rawstore.Store, pool *pgxpool.Pool) (connector.Stats, error) {
	return f.inner.Load(ctx, col, files, raw, pool)
}

// fixedEdition is fixed for an edition source: the requested date is the
// edition, with a fixed publication date.
type fixedEdition struct {
	fixed
	observed time.Time
}

func (f fixedEdition) ResolveReference(_ context.Context, requested time.Time, _ *pgxpool.Pool) (connector.Edition, error) {
	return connector.Edition{ReferenceDate: requested, ObservedDate: f.observed}, nil
}

// ShiftIDs moves the id sequences of the claim and identity tables forward,
// each by a different amount, before anything is written: a scenario run
// once on a fresh schema and once after ShiftIDs must give the same
// canonical dump, which shows that the dump does not depend on surrogate ids
// (nor on which table's ids happen to coincide).
func ShiftIDs(ctx context.Context, pool *pgxpool.Pool) error {
	for i, table := range []string{"collection", "entity", "person_record", "relationship", "company_version",
		"identity_decision", "identity_proposal"} {
		if _, err := pool.Exec(ctx, `SELECT setval(pg_get_serial_sequence($1, 'id'), $2, false)`,
			table, 1000*(i+1)+37*i+1); err != nil {
			return fmt.Errorf("claimstest: shift %s ids: %w", table, err)
		}
	}
	return nil
}
