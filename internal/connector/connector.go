// Package connector defines the contract every public-data source implements
// and the runner that drives one collection from start to finish.
package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/rawstore"
	"geoint/internal/sources"
)

// Request asks for one collection of a source.
// It is the wire format of the ingest queue.
type Request struct {
	Source        string           `json:"source"`
	ReferenceDate time.Time        `json:"reference_date"`
	Scope         collection.Scope `json:"scope"`
}

// Stats are the counters of a collection. "rows_read" and "rows_skipped" are mandatory.
type Stats map[string]int64

// Connector collects one source.
type Connector interface {
	Key() string
	// Fetch downloads the source files for ref into raw.
	Fetch(ctx context.Context, ref time.Time, raw rawstore.Store) ([]rawstore.File, error)
	// Load reads the raw files and writes claims, returning the counters.
	Load(ctx context.Context, col collection.Collection, files []rawstore.File, raw rawstore.Store, pool *pgxpool.Pool) (Stats, error)
}

// EditionResolver is implemented by sources published as dated editions.
// ResolveReference maps a requested date to the newest edition published on
// or before it. pool lets the resolver compare the candidate with what was
// already collected (Receita requires at least as many file parts as the
// last full collection had).
type EditionResolver interface {
	ResolveReference(ctx context.Context, requested time.Time, pool *pgxpool.Pool) (Edition, error)
}

// Edition is a resolved edition.
type Edition struct {
	// ReferenceDate names the edition (Receita: the first day of its month);
	// it becomes the collection's reference date.
	ReferenceDate time.Time
	// ObservedDate is the edition's publication date (Receita: the newest
	// Last-Modified among its data parts, UTC): a date by which the
	// edition's snapshot had been taken. It becomes the collection's observed
	// date (see collection.Collection), the upper bound of what it closes.
	// Zero: unknown; the collection's start day is used.
	ObservedDate time.Time
}

// Errors of edition resolution.
var (
	ErrNoEdition  = errors.New("connector: no published edition on or before the requested date")
	ErrNothingNew = errors.New("connector: newest edition already collected")
)

// Hooks are optional steps that run after a successful Load; nil does nothing.
type Hooks struct {
	// CloseUnseen runs only for snapshot sources.
	CloseUnseen func(ctx context.Context, src sources.Source, col collection.Collection) (int64, error)
	Resolve     func(ctx context.Context, collectionID int64) error
	// Finished runs after the collection is marked done, for derived work
	// (rebuilding search) that must not fail the collection: it returns
	// nothing, so it handles and reports its own errors.
	Finished func(ctx context.Context, collectionID int64)
}

var (
	mu       sync.RWMutex
	registry = map[string]Connector{}
)

// Register makes c available to Get. Registering a key twice is a programming error.
func Register(c Connector) {
	mu.Lock()
	defer mu.Unlock()
	k := c.Key()
	if k == "" {
		panic("connector: empty key")
	}
	if _, dup := registry[k]; dup {
		panic(fmt.Sprintf("connector: %q registered twice", k))
	}
	registry[k] = c
}

// Get returns the connector registered under key.
func Get(key string) (Connector, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[key]
	return c, ok
}
