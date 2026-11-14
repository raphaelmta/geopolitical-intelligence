package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Stats describes a rebuild. Entities counts the searchable entities (those
// with a non-empty normalized name) by entity kind.
type Stats struct {
	Entities       map[string]int64
	FrequentTokens int
	Duration       time.Duration
	// Unchanged: RebuildIfDirty found that the live tables already are what
	// a rebuild would produce, recorded the request as covered and built
	// nothing (Entities and FrequentTokens are then not filled in).
	Unchanged bool
	// Reused: the build took name_norm and tokens of the entities already in
	// the live tables from them instead of computing them again.
	Reused bool
}

// rebuildLockKey is the session advisory lock that serializes rebuilds across
// processes.
const rebuildLockKey int64 = 0x7365617263683031 // "search01"

// The swap waits for running searches with a short lock_timeout and is
// retried with backoff (about a minute in all by default), so a long reader
// delays the swap without making every new search queue behind it for long.
const (
	defaultSwapLockTimeout = 2 * time.Second
	defaultSwapAttempts    = 10
	defaultSwapBackoff     = 500 * time.Millisecond
	maxSwapBackoff         = 5 * time.Second
)

// Rebuild settings, set on the rebuild's own session and reset afterwards.
// Memory as in SEARCH_STRATEGY 4 (measured on a 31 GB server): they are upper
// bounds per sort or index build, used only as far as the data needs.
const rebuildSettings = `SET statement_timeout = 0;
SET lock_timeout = 0;
SET work_mem = '1GB';
SET maintenance_work_mem = '2GB'`

// The build (SEARCH_STRATEGY 4 to 6). Every identifier is a constant.
//
// The ranked table and the three search tables exist in two forms. "Fresh"
// computes name_norm and tokens of every entity. "Reuse" takes them from the
// live tables for the entities already there (LEFT JOIN on entity_id) and
// computes them for the others only. Reuse is exact because both are
// functions of the entity's name alone and an entity id has the same name
// forever (entity rows are immutable, guard_entity_update of migration 0019,
// and never deleted, migration 0001); it is used only while the live tables
// were built with the same functions and statements (search_state.
// build_version equal to buildVersion).
const (
	dropBuildTables = `DROP TABLE IF EXISTS search_degree, search_ranked, search_company_new, search_person_new,
		search_other_new, search_frequent_token_new`

	// grau = open relationships touching the entity, all sources; a
	// relationship of an entity with itself counts once.
	createDegree = `CREATE UNLOGGED TABLE search_degree AS
		SELECT v.entity_id, count(*)::integer AS grau
		FROM relationship r
		CROSS JOIN LATERAL (VALUES (r.src_entity_id),
			(CASE WHEN r.dst_entity_id <> r.src_entity_id THEN r.dst_entity_id END)) AS v(entity_id)
		WHERE r.closed_collection_id IS NULL AND v.entity_id IS NOT NULL
		GROUP BY v.entity_id`

	// The entities that may be searchable. Person entities without any
	// person record (left empty by identity merges) are left out; kinds
	// without a code (none today) get kind NULL and are left out later.
	candidatesCTE = `cand AS (
			SELECT e.id, e.name,
				(CASE e.kind WHEN 'company' THEN 'C' WHEN 'person' THEN 'P' WHEN 'foreign' THEN 'E'
					WHEN 'party' THEN 'A' WHEN 'public_body' THEN 'O' END)::"char" AS kind
			FROM entity e
			WHERE e.kind <> 'person' OR EXISTS (SELECT 1 FROM person_record p WHERE p.entity_id = e.id))`

	// The live tables as one relation.
	liveCTE = `live AS (
			SELECT entity_id, 'C'::"char" AS kind, name_norm, grau FROM search_company
			UNION ALL SELECT entity_id, kind, name_norm, grau FROM search_person
			UNION ALL SELECT entity_id, kind, name_norm, grau FROM search_other)`

	rankedHead = `CREATE UNLOGGED TABLE search_ranked AS
		WITH ` + candidatesCTE + `, ` + liveCTE + `,
		n AS (`
	rankedTail = `)
		SELECT row_number() OVER (ORDER BY coalesce(d.grau, 0) DESC, n.name_norm, n.id) AS search_rank,
			n.id AS entity_id, n.kind, n.name_norm, coalesce(d.grau, 0) AS grau
		FROM n LEFT JOIN search_degree d ON d.entity_id = n.id
		WHERE n.name_norm IS NOT NULL AND n.kind IS NOT NULL`
	createRankedFresh = rankedHead + `
			SELECT c.id, c.kind, search_norm(c.name) COLLATE "C" AS name_norm FROM cand c` + rankedTail
	createRankedReuse = rankedHead + `
			SELECT c.id, c.kind, coalesce(l.name_norm, search_norm(c.name) COLLATE "C") AS name_norm
			FROM cand c LEFT JOIN live l ON l.entity_id = c.id` + rankedTail

	// changedSQL is false only when the live tables hold exactly the rows a
	// build from the current data would rank: the same searchable entities,
	// each with the same kind and the same grau. With the same build_version
	// (so the same name_norm and tokens for an entity id) these are all the
	// inputs of the build: search_rank orders by (grau, name_norm, id), the
	// kind picks the table, the frequent tokens count the tokens. It
	// compares data, not a log of writes: whatever wrote entity,
	// person_record or relationship, and whether or not the collection that
	// did reached its search refresh, a difference shows here. search_norm
	// is computed only for the entities missing from the live tables (an
	// entity there has a non-NULL name_norm, and its name cannot change).
	changedSQL = `WITH ` + candidatesCTE + `, ` + liveCTE + `
		SELECT EXISTS (
			SELECT 1 FROM cand c FULL JOIN live l ON l.entity_id = c.id
			LEFT JOIN search_degree d ON d.entity_id = c.id
			WHERE CASE
				WHEN l.entity_id IS NULL THEN c.kind IS NOT NULL AND search_norm(c.name) IS NOT NULL
				WHEN c.id IS NULL THEN true
				ELSE l.kind IS DISTINCT FROM c.kind OR l.grau IS DISTINCT FROM coalesce(d.grau, 0)
				END)`

	countRanked = `SELECT kind::text, count(*) FROM search_ranked GROUP BY kind`

	// ORDER BY search_rank is mandatory: queries rely on the physical order.
	createCompanyFresh = `CREATE TABLE search_company_new AS
		SELECT search_rank, entity_id, name_norm, grau, search_tokens(name_norm) AS tokens
		FROM search_ranked WHERE kind = 'C' ORDER BY search_rank`
	createPersonFresh = `CREATE TABLE search_person_new AS
		SELECT search_rank, entity_id, kind, name_norm, grau, search_tokens(name_norm) AS tokens
		FROM search_ranked WHERE kind = 'P' ORDER BY search_rank`
	createOtherFresh = `CREATE TABLE search_other_new AS
		SELECT search_rank, entity_id, kind, name_norm, grau, search_tokens(name_norm) AS tokens
		FROM search_ranked WHERE kind IN ('E', 'A', 'O') ORDER BY search_rank`
	createCompanyReuse = `CREATE TABLE search_company_new AS
		SELECT r.search_rank, r.entity_id, r.name_norm, r.grau, coalesce(o.tokens, search_tokens(r.name_norm)) AS tokens
		FROM search_ranked r LEFT JOIN search_company o ON o.entity_id = r.entity_id
		WHERE r.kind = 'C' ORDER BY r.search_rank`
	createPersonReuse = `CREATE TABLE search_person_new AS
		SELECT r.search_rank, r.entity_id, r.kind, r.name_norm, r.grau, coalesce(o.tokens, search_tokens(r.name_norm)) AS tokens
		FROM search_ranked r LEFT JOIN search_person o ON o.entity_id = r.entity_id
		WHERE r.kind = 'P' ORDER BY r.search_rank`
	createOtherReuse = `CREATE TABLE search_other_new AS
		SELECT r.search_rank, r.entity_id, r.kind, r.name_norm, r.grau, coalesce(o.tokens, search_tokens(r.name_norm)) AS tokens
		FROM search_ranked r LEFT JOIN search_other o ON o.entity_id = r.entity_id
		WHERE r.kind IN ('E', 'A', 'O') ORDER BY r.search_rank`

	// versionSQL is the build version: a digest of everything name_norm and
	// tokens depend on — the definitions of search_norm, search_words and
	// search_tokens as the catalogue holds them now (a migration that
	// replaces one changes it by itself), the unaccent dictionary (extension
	// version, options; its rules file ships with the server, hence the
	// server version) — and $1, the digest of the build statements of this
	// file (buildStatementsDigest).
	versionSQL = `SELECT md5(concat_ws(E'\n', $1::text, current_setting('server_version_num'),
		(SELECT extversion FROM pg_extension WHERE extname = 'unaccent'),
		(SELECT d.dictinitoption FROM pg_ts_dict d WHERE d.oid = 'public.unaccent'::regdictionary),
		pg_get_functiondef('search_norm(text)'::regprocedure),
		pg_get_functiondef('search_words(text)'::regprocedure),
		pg_get_functiondef('search_tokens(text)'::regprocedure)))`

	// What the live tables were built with.
	builtWithSQL = `SELECT generation, build_version, frequent_threshold FROM search_state`

	// A skipped rebuild covers the request exactly as a swap would, without
	// touching the generation (nothing to reload) or the tables.
	coveredSQL = `UPDATE search_state SET rebuilt_from = $1, finished_collections = $2`

	createFrequentNew = `CREATE TABLE search_frequent_token_new (token text NOT NULL, ndoc bigint)`
	// The threshold applies to the sum over all search tables.
	fillFrequentNew = `INSERT INTO search_frequent_token_new (token, ndoc)
		SELECT word, sum(ndoc)::bigint
		FROM (SELECT word, ndoc FROM ts_stat('SELECT tokens FROM search_company_new')
			UNION ALL SELECT word, ndoc FROM ts_stat('SELECT tokens FROM search_person_new')
			UNION ALL SELECT word, ndoc FROM ts_stat('SELECT tokens FROM search_other_new')) s
		GROUP BY word HAVING sum(ndoc) >= $1`

	// Dirty: a rebuild was requested after the one the current tables were
	// built from; or more collections have finished (done or failed: a
	// final, one-way status) than when that rebuild started reading, which
	// recovers a lost mark without trusting the clock; or the tables were
	// never built while entities exist. Also returns the request and the
	// finished count a rebuild starting now covers.
	dirtySQL = `WITH f AS (SELECT count(*) AS n FROM collection WHERE status <> 'running')
		SELECT coalesce(s.requested_at > coalesce(s.rebuilt_from, '-infinity'), false)
			OR f.n > coalesce(s.finished_collections, 0)
			OR (s.generation = 0 AND EXISTS (SELECT 1 FROM entity)),
		s.requested_at, f.n
		FROM search_state s, f`

	// The mark always grows, even if the clock goes back, so a request is
	// never taken for one an earlier rebuild already covered.
	markDirtySQL = `UPDATE search_state SET requested_at = greatest(clock_timestamp(),
		coalesce(requested_at, '-infinity') + interval '1 microsecond',
		coalesce(rebuilt_from, '-infinity') + interval '1 microsecond')`
)

var (
	buildIndexes = []string{
		`DROP TABLE search_ranked, search_degree`,
		`ALTER TABLE search_company_new ADD CONSTRAINT search_company_new_pkey PRIMARY KEY (search_rank)`,
		`ALTER TABLE search_person_new ADD CONSTRAINT search_person_new_pkey PRIMARY KEY (search_rank)`,
		`ALTER TABLE search_other_new ADD CONSTRAINT search_other_new_pkey PRIMARY KEY (search_rank)`,
		`CREATE INDEX search_company_new_name ON search_company_new (name_norm)`,
		`CREATE INDEX search_person_new_name ON search_person_new (name_norm)`,
		`CREATE INDEX search_other_new_name ON search_other_new (name_norm)`,
		`CREATE INDEX search_company_new_tokens ON search_company_new USING gin (tokens)`,
		`CREATE INDEX search_person_new_tokens ON search_person_new USING gin (tokens)`,
		`CREATE INDEX search_other_new_tokens ON search_other_new USING gin (tokens)`,
		// VACUUM cannot run in a transaction block: one statement each.
		`VACUUM ANALYZE search_company_new`,
		`VACUUM ANALYZE search_person_new`,
		`VACUUM ANALYZE search_other_new`,
		createFrequentNew,
	}
	afterFrequent = []string{
		`ALTER TABLE search_frequent_token_new ADD CONSTRAINT search_frequent_token_new_pkey PRIMARY KEY (token)`,
		`ANALYZE search_frequent_token_new`,
	}
	// SEARCH_STRATEGY 7, plus search_other and the state update, in one
	// transaction: the only moment that blocks searches. The LOCK takes the
	// live tables in the order a query reads them (frequent words, then
	// company, person, other), so a query that already holds one of them
	// never deadlocks with the swap.
	swapStatements = []string{
		`LOCK TABLE search_frequent_token, search_company, search_person, search_other IN ACCESS EXCLUSIVE MODE`,
		`DROP TABLE search_company, search_person, search_other, search_frequent_token`,
		`ALTER TABLE search_company_new RENAME TO search_company`,
		`ALTER TABLE search_person_new RENAME TO search_person`,
		`ALTER TABLE search_other_new RENAME TO search_other`,
		`ALTER TABLE search_frequent_token_new RENAME TO search_frequent_token`,
		`ALTER INDEX search_company_new_pkey RENAME TO search_company_pkey`,
		`ALTER INDEX search_person_new_pkey RENAME TO search_person_pkey`,
		`ALTER INDEX search_other_new_pkey RENAME TO search_other_pkey`,
		`ALTER INDEX search_frequent_token_new_pkey RENAME TO search_frequent_token_pkey`,
		`ALTER INDEX search_company_new_name RENAME TO search_company_name`,
		`ALTER INDEX search_person_new_name RENAME TO search_person_name`,
		`ALTER INDEX search_other_new_name RENAME TO search_other_name`,
		`ALTER INDEX search_company_new_tokens RENAME TO search_company_tokens`,
		`ALTER INDEX search_person_new_tokens RENAME TO search_person_tokens`,
		`ALTER INDEX search_other_new_tokens RENAME TO search_other_tokens`,
	}
)

// buildStatementsDigest is the part of the build version that this file
// decides: a digest of the statements that compute name_norm, tokens, the
// rank and the tables. Editing any of them changes the build version, so the
// first rebuild with the new code computes everything (and is never skipped)
// instead of reusing values produced by the old statements.
var buildStatementsDigest = func() string {
	h := sha256.New()
	for _, sql := range []string{createDegree, createRankedFresh, createRankedReuse, changedSQL,
		createCompanyFresh, createPersonFresh, createOtherFresh,
		createCompanyReuse, createPersonReuse, createOtherReuse, fillFrequentNew} {
		h.Write([]byte(sql))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}()

// kindNames maps the "char" codes of the search tables back to entity kinds.
var kindNames = map[string]string{}

func init() {
	for k, r := range routes {
		kindNames[r.code] = string(k)
	}
}

// markDirtyTimeout bounds MarkDirty, which runs even when the caller's
// context is already cancelled.
const markDirtyTimeout = 5 * time.Second

// MarkDirty records durably that the search tables must be rebuilt (new data
// was committed). Call it before RebuildIfDirty: if that rebuild is
// interrupted, the mark stays and a later RebuildIfDirty (at worker start or
// after the next collection) picks it up. It is written even if ctx was just
// cancelled (shutdown right after a collection), within 5 s.
func (s *Service) MarkDirty(ctx context.Context) error {
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markDirtyTimeout)
	defer cancel()
	if _, err := s.pool.Exec(mctx, markDirtySQL); err != nil {
		return fmt.Errorf("search: mark dirty: %w", err)
	}
	return nil
}

// Dirty reports whether a rebuild was requested after the one the current
// tables come from, or the tables were never built while entities exist.
func (s *Service) Dirty(ctx context.Context) (bool, error) {
	var dirty bool
	var requested *time.Time
	var finished int64
	if err := s.pool.QueryRow(ctx, dirtySQL).Scan(&dirty, &requested, &finished); err != nil {
		return false, fmt.Errorf("search: dirty: %w", err)
	}
	return dirty, nil
}

// Refresh is what runs after a collection: MarkDirty, then RebuildIfDirty.
// If the mark cannot be written, that is logged as its own error and the
// tables are rebuilt unconditionally; a lost mark is still recovered at the
// next worker start, because a collection finished after the last rebuild
// makes the state dirty. RefreshCollection does the same and names the
// collection in that log line.
func (s *Service) Refresh(ctx context.Context) (Stats, bool, error) {
	return s.refresh(ctx, nil)
}

// RefreshCollection is Refresh after collection collectionID.
func (s *Service) RefreshCollection(ctx context.Context, collectionID int64) (Stats, bool, error) {
	return s.refresh(ctx, &collectionID)
}

func (s *Service) refresh(ctx context.Context, collectionID *int64) (Stats, bool, error) {
	if err := s.MarkDirty(ctx); err != nil {
		attrs := []any{"error", err}
		if collectionID != nil {
			attrs = append(attrs, "collection", *collectionID)
		}
		slog.Error("search: could not mark dirty", attrs...)
		st, err := s.Rebuild(ctx)
		return st, err == nil, err
	}
	return s.RebuildIfDirty(ctx)
}

// RebuildIfDirty rebuilds when Dirty, deciding after it got the rebuild
// lock: a caller whose request was covered by a rebuild that started after
// it, while it waited, does nothing. It reports whether it rebuilt.
//
// When dirty, it first checks whether the data changed anything the search
// tables hold (changedSQL): when nothing did, and the live tables were built
// with the current build version and frequent-token threshold, it records
// the request as covered without building, swapping or bumping the
// generation, and returns rebuilt = false with Stats.Unchanged set.
func (s *Service) RebuildIfDirty(ctx context.Context) (Stats, bool, error) {
	return s.rebuild(ctx, true)
}

// Rebuild builds the search tables from entity, person_record and
// relationship and swaps them in (SEARCH_STRATEGY 4 to 7), whether or not
// they are dirty, and never skips. It works on empty tables too. The result
// is row for row what a build from empty tables gives; name_norm and tokens
// of the entities already indexed are taken from the live tables when they
// were built with the current build version (Stats.Reused).
//
// One rebuild runs at a time across processes: a second caller waits for the
// first to finish. While building, Rebuild only reads entity, relationship,
// person_record and the live search tables and writes its own build tables;
// searches keep using the current tables until the swap, a short transaction
// that renames them. If the swap cannot get its locks (a long-running search)
// it is retried with backoff; if it never can, the built tables are dropped,
// an error is returned and the state stays dirty. The rebuild runs on one
// dedicated connection whose session settings are reset before it goes back
// to the pool.
func (s *Service) Rebuild(ctx context.Context) (Stats, error) {
	st, _, err := s.rebuild(ctx, false)
	return st, err
}

func (s *Service) rebuild(ctx context.Context, onlyIfDirty bool) (st Stats, rebuilt bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return Stats{}, false, fmt.Errorf("search: rebuild: acquire: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, rebuildLockKey); err != nil {
		// The lock may or may not have been taken: closing the session
		// releases it either way.
		discard(conn)
		return Stats{}, false, fmt.Errorf("search: rebuild: lock: %w", err)
	}
	defer func() {
		// Session settings and the lock must not outlive the rebuild on a
		// pooled connection; when that cannot be ensured, drop the connection.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err != nil {
			// Do not leave a half-built copy behind. When the connection
			// is gone (pgx closes it on cancellation) the lock went with it
			// and another rebuild may own the build tables now: they are
			// left for the next rebuild, which drops leftovers first.
			if _, derr := conn.Exec(rctx, dropBuildTables); derr != nil {
				err = errors.Join(err, fmt.Errorf("drop the half-built tables: %w", derr))
			}
		}
		if _, uerr := conn.Exec(rctx, `RESET ALL; SELECT pg_advisory_unlock_all()`); uerr != nil {
			discard(conn)
			return
		}
		conn.Release()
	}()
	if s.afterLock != nil {
		s.afterLock()
	}

	// The request this rebuild covers is read before the build starts, so
	// every collection that marked the state dirty up to now is in the data
	// it reads; a mark made later leaves the state dirty.
	var dirty bool
	var requested *time.Time
	var finished int64
	if err = conn.QueryRow(ctx, dirtySQL).Scan(&dirty, &requested, &finished); err != nil {
		return Stats{}, false, fmt.Errorf("search: rebuild: state: %w", err)
	}
	if onlyIfDirty && !dirty {
		return Stats{}, false, nil
	}

	start := time.Now()
	var version string
	st, version, err = s.build(ctx, conn, onlyIfDirty)
	if err == nil && st.Unchanged {
		if _, err = conn.Exec(ctx, coveredSQL, requested, finished); err != nil {
			return Stats{}, false, fmt.Errorf("search: rebuild: record an unchanged state: %w", err)
		}
		st.Duration = time.Since(start)
		return st, false, nil
	}
	if err == nil {
		err = s.swap(ctx, conn, requested, finished, version)
	}
	if err != nil {
		return Stats{}, false, fmt.Errorf("search: rebuild: %w", err)
	}
	s.invalidate()
	st.Duration = time.Since(start)
	return st, true, nil
}

// discard closes a connection instead of returning it to the pool.
func discard(conn *pgxpool.Conn) {
	c := conn.Hijack()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

// build builds the new tables and returns the build version they were built
// with. With mayskip, it first checks whether anything changed and returns
// Stats.Unchanged without building when nothing did.
//
// The degree table, the check and the ranked table — every read of entity,
// person_record and relationship — run in one REPEATABLE READ transaction:
// the tables are built from one snapshot, and the check judges that same
// snapshot. The live search tables it also reads change only at a swap,
// which needs the rebuild lock this build holds.
func (s *Service) build(ctx context.Context, conn *pgxpool.Conn, mayskip bool) (Stats, string, error) {
	st := Stats{Entities: map[string]int64{}}
	for _, k := range kindNames {
		st.Entities[k] = 0
	}
	exec := func(sql string, args ...any) error {
		_, err := conn.Exec(ctx, sql, args...)
		return err
	}
	// Leftovers of an interrupted rebuild are dropped first.
	for _, sql := range []string{rebuildSettings, dropBuildTables} {
		if err := exec(sql); err != nil {
			return st, "", err
		}
	}
	var version string
	if err := conn.QueryRow(ctx, versionSQL, buildStatementsDigest).Scan(&version); err != nil {
		return st, "", fmt.Errorf("build version: %w", err)
	}
	var generation int64
	var builtWith *string
	var threshold *int64
	if err := conn.QueryRow(ctx, builtWithSQL).Scan(&generation, &builtWith, &threshold); err != nil {
		return st, "", err
	}
	// The live tables hold values computed exactly as this build would
	// compute them only when they were built with this version.
	st.Reused = builtWith != nil && *builtWith == version
	mayskip = mayskip && st.Reused && generation > 0 && threshold != nil && *threshold == s.frequentThreshold

	err := pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, createDegree); err != nil {
			return err
		}
		if mayskip {
			var changed bool
			if err := tx.QueryRow(ctx, changedSQL).Scan(&changed); err != nil {
				return fmt.Errorf("change check: %w", err)
			}
			if !changed {
				st.Unchanged = true
				_, err := tx.Exec(ctx, `DROP TABLE search_degree`)
				return err
			}
		}
		ranked := createRankedFresh
		if st.Reused {
			ranked = createRankedReuse
		}
		_, err := tx.Exec(ctx, ranked)
		return err
	})
	if err != nil || st.Unchanged {
		return st, version, err
	}

	rows, err := conn.Query(ctx, countRanked)
	if err != nil {
		return st, "", err
	}
	for rows.Next() {
		var code string
		var n int64
		if err := rows.Scan(&code, &n); err != nil {
			rows.Close()
			return st, "", err
		}
		st.Entities[kindNames[code]] = n
	}
	if err := rows.Err(); err != nil {
		return st, "", err
	}
	tables := []string{createCompanyFresh, createPersonFresh, createOtherFresh}
	if st.Reused {
		tables = []string{createCompanyReuse, createPersonReuse, createOtherReuse}
	}
	for _, sql := range append(tables, buildIndexes...) {
		if err := exec(sql); err != nil {
			return st, "", err
		}
	}
	tag, err := conn.Exec(ctx, fillFrequentNew, s.frequentThreshold)
	if err != nil {
		return st, "", err
	}
	st.FrequentTokens = int(tag.RowsAffected())
	for _, sql := range afterFrequent {
		if err := exec(sql); err != nil {
			return st, "", err
		}
	}
	return st, version, nil
}

// errVersionChanged: the functions the build version covers were replaced
// while the tables were being built (a migration ran meanwhile), so the new
// tables may mix values of both definitions. They are not swapped in; the
// state stays dirty and the next rebuild computes everything.
var errVersionChanged = errors.New("the search functions changed during the build")

// swap renames the new tables in, retrying when its locks time out. requested
// is the request the build covers, stored as rebuilt_from; finished, the
// number of finished collections read before the build read any data, is
// stored as finished_collections; version, the build version the tables were
// built with, as build_version, with the frequent-token threshold.
func (s *Service) swap(ctx context.Context, conn *pgxpool.Conn, requested *time.Time, finished int64, version string) error {
	backoff := s.swapBackoff
	var err error
	for attempt := 1; attempt <= s.swapAttempts; attempt++ {
		if s.beforeSwap != nil {
			s.beforeSwap(attempt)
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`,
				fmt.Sprintf("%dms", s.swapLockTimeout.Milliseconds())); err != nil {
				return err
			}
			var now string
			if err := tx.QueryRow(ctx, versionSQL, buildStatementsDigest).Scan(&now); err != nil {
				return err
			}
			if now != version {
				return errVersionChanged
			}
			for _, sql := range swapStatements {
				if _, err := tx.Exec(ctx, sql); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `UPDATE search_state
				SET generation = generation + 1, rebuilt_at = now(), rebuilt_from = $1, finished_collections = $2,
					build_version = $3, frequent_threshold = $4`,
				requested, finished, version, s.frequentThreshold)
			return err
		})
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "55P03" || attempt == s.swapAttempts {
			break // done, or not a lock timeout (55P03 lock_not_available), or out of attempts
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxSwapBackoff)
	}
	if err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	return nil
}
