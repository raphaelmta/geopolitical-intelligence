// Package sources lists the public data sources the platform ingests.
package sources

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const day = 24 * time.Hour

// Source describes one public data source and how often it is expected to publish.
type Source struct {
	ID           int16
	Key, Name    string
	PublishEvery time.Duration
	StaleAfter   time.Duration
	// Snapshot: the source publishes its full state every time.
	Snapshot bool
	// HasConnector: a connector for this source exists, so a collection of
	// it can be requested. The others are catalogued ahead of their connector.
	HasConnector bool
	// SupportsSlice: a collection may be limited to listed companies
	// (collection.Scope CNPJs/Depth). Otherwise only full collections.
	SupportsSlice bool
	// Editions: the source is published as dated editions (its connector is
	// a connector.EditionResolver). Its staleness counts from the edition's
	// publication (collection.observed_date), not from the reference date
	// that names the edition (Receita: the first day of the month); other
	// sources count from the reference date. Stored in source.editions.
	Editions bool
}

// All is the catalogue of sources. IDs are stable and stored in the database.
var All = []Source{
	{1, "receita", "Receita Federal — CNPJ", 30 * day, 45 * day, true, true, true, true},
	{2, "tse", "TSE — candidaturas e contas", 7 * day, 30 * day, false, true, false, false},
	{3, "camara", "Câmara dos Deputados", 7 * day, 14 * day, false, true, false, false},
	{4, "senado", "Senado Federal", 7 * day, 14 * day, false, true, false, false},
	{5, "cvm", "CVM — companhias abertas", 7 * day, 30 * day, true, true, false, false},
}

// ByKey returns the source with the given key.
func ByKey(key string) (Source, bool) {
	for _, s := range All {
		if s.Key == key {
			return s, true
		}
	}
	return Source{}, false
}

// Sync inserts or updates the rows of All in the source table, intervals included.
func Sync(ctx context.Context, p *pgxpool.Pool) error {
	for _, s := range All {
		_, err := p.Exec(ctx, `
			INSERT INTO source (id, key, name, publish_every, stale_after, snapshot, editions)
			VALUES ($1, $2, $3, make_interval(secs => $4), make_interval(secs => $5), $6, $7)
			ON CONFLICT (id) DO UPDATE SET
				key = EXCLUDED.key, name = EXCLUDED.name,
				publish_every = EXCLUDED.publish_every, stale_after = EXCLUDED.stale_after,
				snapshot = EXCLUDED.snapshot, editions = EXCLUDED.editions`,
			s.ID, s.Key, s.Name, s.PublishEvery.Seconds(), s.StaleAfter.Seconds(), s.Snapshot, s.Editions)
		if err != nil {
			return fmt.Errorf("sources: sync %s: %w", s.Key, err)
		}
	}
	return nil
}
