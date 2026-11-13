package receita

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/connector"
	"geoint/internal/identity"
)

// checkpoint, when set (tests only), runs after each committed step of the
// set-based phase ("companies", "persons", "relationship chunk <n>"); an
// error stops the load there, as a crash would.
var checkpointHook func(point string) error

func checkpoint(point string) error {
	if checkpointHook == nil {
		return nil
	}
	return checkpointHook(point)
}

// relBuckets is the number of relationship chunks: about 27 k Socios rows
// (seconds of work) per chunk at national scale. Must be a power of two.
const relBuckets = 1024

// workMem is set for the set-based transactions (sorts and hashes over tens
// of millions of staging rows).
const workMem = "256MB"

// Set-based phase. The statements reproduce the single-row semantics of
// internal/claims.Store on whole staging tables:
//   - entity: unique (kind, natural_key); an existing entity keeps its name;
//   - company_version: one open version per company; same data advances
//     last_seen, changed data closes the open one and opens a new one;
//   - person_record: unique (source_id, context_key, name_norm, cpf_fragment);
//     a new record gets its own new person entity; existing ones are not rewritten;
//   - relationship: the open-row natural key (rel_type, record or -entity at
//     each end, role, valid_from); a re-seen row only advances last_seen
//     (and follows its records' current entities).
// Every UPDATE touches only rows that change (each pays a row trigger).
//
// Inserts find new keys with an anti-join and carry no ON CONFLICT clause:
// staging holds one row per key, the load lock keeps Receita loads apart,
// and in the medium trial ON CONFLICT made bulk inserts about four times
// slower (speculative insertion per row). A concurrent writer of the same key
// (another connector) makes the statement fail with a unique violation, the
// collection is marked failed and the next one redoes the work idempotently.

// scope statements: the start bases, one expansion per depth level, the filter.
const (
	sqlScopeStart = `INSERT INTO receita_stg_scope (cnpj_basico) SELECT DISTINCT unnest($1::text[])`
	// One level: company partners of the selected companies, and the
	// companies the selected ones are partners of.
	sqlScopeExpand = `INSERT INTO receita_stg_scope (cnpj_basico)
		SELECT s.partner_base FROM receita_stg_socio s JOIN receita_stg_scope sc ON sc.cnpj_basico = s.cnpj_basico
		WHERE s.partner_type = '1'
		UNION
		SELECT s.cnpj_basico FROM receita_stg_socio s JOIN receita_stg_scope sc ON sc.cnpj_basico = s.partner_base
		WHERE s.partner_type = '1'
		ON CONFLICT DO NOTHING`
	sqlScopeEmpresa = `DELETE FROM receita_stg_empresa e
		WHERE NOT EXISTS (SELECT 1 FROM receita_stg_scope sc WHERE sc.cnpj_basico = e.cnpj_basico)`
	sqlScopeEstab = `DELETE FROM receita_stg_estab e
		WHERE NOT EXISTS (SELECT 1 FROM receita_stg_scope sc WHERE sc.cnpj_basico = e.cnpj_basico)`
	// Rows of selected companies; a company partner only when it is selected too.
	sqlScopeSocio = `DELETE FROM receita_stg_socio s
		WHERE NOT EXISTS (SELECT 1 FROM receita_stg_scope sc WHERE sc.cnpj_basico = s.cnpj_basico)
		   OR (s.partner_type = '1' AND NOT EXISTS (SELECT 1 FROM receita_stg_scope sc WHERE sc.cnpj_basico = s.partner_base))`
)

// Companies.
const (
	// Empresas deduplicated by cnpj_basico (first row wins), joined with the
	// head office of Estabelecimentos.
	sqlCompanySrc = `INSERT INTO receita_stg_company_src
			(cnpj_basico, name, name_norm, has_version, legal_nature, size, uf, city, cnae, status, capital, file_no, line)
		SELECT DISTINCT ON (e.cnpj_basico) e.cnpj_basico, e.name, e.name_norm, true, e.legal_nature, e.size,
			coalesce(m.uf, ''), coalesce(m.city, ''), coalesce(m.cnae, ''), coalesce(m.status, ''),
			e.capital::numeric(18, 2), e.file_no, e.line
		FROM receita_stg_empresa e
		LEFT JOIN receita_stg_estab m ON m.cnpj_basico = e.cnpj_basico
		ORDER BY e.cnpj_basico, e.file_no, e.line, m.file_no, m.line`
	// Companies seen only in Socios: a company partner missing from Empresas
	// (named from the partner row) or a company whose partners are listed but
	// that has no Empresas row (no name). No company_version for them.
	sqlCompanyOrphans = `INSERT INTO receita_stg_company_src
			(cnpj_basico, name, name_norm, has_version, legal_nature, size, uf, city, cnae, status)
		SELECT DISTINCT ON (o.cnpj_basico) o.cnpj_basico, o.name, o.name_norm, false, '', '', '', '', '', ''
		FROM (
			SELECT partner_base AS cnpj_basico, name, name_norm, 0 AS prio, file_no, line
			FROM receita_stg_socio WHERE partner_type = '1'
			UNION ALL
			SELECT cnpj_basico, '', '', 1, file_no, line FROM receita_stg_socio
		) o
		WHERE NOT EXISTS (SELECT 1 FROM receita_stg_empresa e WHERE e.cnpj_basico = o.cnpj_basico)
		ORDER BY o.cnpj_basico, o.prio, o.file_no, o.line`
	sqlCompanyEntities = `INSERT INTO entity (kind, natural_key, name, name_norm)
		SELECT 'company', s.cnpj_basico, s.name, s.name_norm FROM receita_stg_company_src s
		WHERE NOT EXISTS (SELECT 1 FROM entity e WHERE e.kind = 'company' AND e.natural_key = s.cnpj_basico)
		ORDER BY s.cnpj_basico`
	sqlCompanyIDs = `INSERT INTO receita_stg_company
			(cnpj_basico, entity_id, has_version, name, legal_nature, size, uf, city, cnae, status, capital, file_no, line)
		SELECT s.cnpj_basico, e.id, s.has_version, s.name, s.legal_nature, s.size, s.uf, s.city, s.cnae, s.status,
			s.capital, s.file_no, s.line
		FROM receita_stg_company_src s
		JOIN entity e ON e.kind = 'company' AND e.natural_key = s.cnpj_basico`
	sqlVersionClose = `UPDATE company_version v SET closed_collection_id = $1
		FROM receita_stg_company s
		WHERE s.has_version AND v.entity_id = s.entity_id AND v.closed_collection_id IS NULL
		  AND (v.name, v.legal_nature, v.size, v.uf, v.city, v.cnae, v.status, v.capital)
		      IS DISTINCT FROM (s.name, s.legal_nature, s.size, s.uf, s.city, s.cnae, s.status, s.capital)`
	// After sqlVersionClose every open version of a loaded company equals the
	// new data.
	sqlVersionSeen = `UPDATE company_version v SET last_seen_collection_id = $1
		FROM receita_stg_company s
		WHERE s.has_version AND v.entity_id = s.entity_id AND v.closed_collection_id IS NULL
		  AND (v.last_seen_collection_id IS NULL OR v.last_seen_collection_id < $1)`
	sqlVersionInsert = `INSERT INTO company_version (entity_id, name, legal_nature, size, uf, city, cnae, status, capital,
			first_collection_id, record_ref, last_seen_collection_id)
		SELECT s.entity_id, s.name, s.legal_nature, s.size, s.uf, s.city, s.cnae, s.status, s.capital,
			$1, f.name || ':' || s.line, $1
		FROM receita_stg_company s JOIN receita_stg_file f ON f.file_no = s.file_no
		WHERE s.has_version
		  AND NOT EXISTS (SELECT 1 FROM company_version v WHERE v.entity_id = s.entity_id AND v.closed_collection_id IS NULL)`
)

// Persons and foreign partners.
const (
	// One row per record key: person partners and representatives, both in
	// the company's context. A partner row wins over a representative.
	sqlPersons = `INSERT INTO receita_stg_person (context_key, name, name_norm, cpf_fragment, file_no, line)
		SELECT DISTINCT ON (p.context_key, p.name_norm, p.cpf_fragment)
			p.context_key, p.name, p.name_norm, p.cpf_fragment, p.file_no, p.line
		FROM (
			SELECT cnpj_basico AS context_key, name, name_norm, cpf_fragment, 0 AS prio, file_no, line
			FROM receita_stg_socio WHERE partner_type = '2'
			UNION ALL
			SELECT cnpj_basico, rep_name, rep_name_norm, rep_fragment, 1, file_no, line
			FROM receita_stg_socio WHERE has_rep
		) p
		ORDER BY p.context_key, p.name_norm, p.cpf_fragment, p.prio, p.file_no, p.line`
	// New keys get an entity id from the entity sequence ($2 is its oid).
	sqlPersonNew = `INSERT INTO receita_stg_person_new (entity_id, context_key, name, name_norm, cpf_fragment, file_no, line)
		SELECT nextval($2::oid::regclass), p.context_key, p.name, p.name_norm, p.cpf_fragment, p.file_no, p.line
		FROM receita_stg_person p
		WHERE NOT EXISTS (SELECT 1 FROM person_record r WHERE r.source_id = $1 AND r.context_key = p.context_key
			AND r.name_norm = p.name_norm AND r.cpf_fragment = p.cpf_fragment)`
	sqlPersonEntities = `INSERT INTO entity (id, kind, name, name_norm)
		SELECT entity_id, 'person', name, name_norm FROM receita_stg_person_new`
	// Inserted already linked: no follow-up UPDATE (and its trigger) per record.
	sqlPersonRecords = `INSERT INTO person_record (source_id, context_key, name, name_norm, cpf_fragment, entity_id,
			identity_confidence, first_collection_id, record_ref)
		SELECT $1, n.context_key, n.name, n.name_norm, n.cpf_fragment, n.entity_id, 1, $2, f.name || ':' || n.line
		FROM receita_stg_person_new n JOIN receita_stg_file f ON f.file_no = n.file_no`
	sqlForeignEntities = `INSERT INTO entity (kind, natural_key, name, name_norm)
		SELECT DISTINCT ON (s.foreign_key) 'foreign', s.foreign_key, s.name, s.name_norm
		FROM receita_stg_socio s
		WHERE s.partner_type = '3'
		  AND NOT EXISTS (SELECT 1 FROM entity e WHERE e.kind = 'foreign' AND e.natural_key = s.foreign_key)
		ORDER BY s.foreign_key, s.file_no, s.line`
)

// Relationships. They are written in short per-bucket transactions so the
// identity lock is held for seconds, not for the whole phase:
//   - sqlPartners (one transaction, no identity lock) resolves, for every
//     Socios row, the ids identity resolution never moves: the company, the
//     company or foreign partner entity, and the person record ids of the
//     partner and of the representative. Rows are stored sorted by bucket
//     (a hash of cnpj_basico, so all claims of one company share a bucket).
//   - per bucket (relChunk): take the identity lock, read the records'
//     current entities, stage one row per natural key and write them.
//
// Every chunk is idempotent (anti-joins on the natural key), so a load that
// fails between chunks is repaired by the next collection.
const (
	sqlPartners = `INSERT INTO receita_stg_partner (bucket, company_id, partner_entity_id, partner_record_id, rep_record_id,
			qual, rep_qual, valid_from, file_no, line)
		SELECT hashtext(s.cnpj_basico) & $2, c.entity_id,
			CASE s.partner_type WHEN '1' THEN pc.entity_id WHEN '3' THEN fe.id END,
			pr.id, rr.id, s.qual, s.rep_qual, s.valid_from, s.file_no, s.line
		FROM receita_stg_socio s
		JOIN receita_stg_company c ON c.cnpj_basico = s.cnpj_basico
		LEFT JOIN receita_stg_company pc ON s.partner_type = '1' AND pc.cnpj_basico = s.partner_base
		LEFT JOIN entity fe ON s.partner_type = '3' AND fe.kind = 'foreign' AND fe.natural_key = s.foreign_key
		LEFT JOIN person_record pr ON s.partner_type = '2' AND pr.source_id = $1 AND pr.context_key = s.cnpj_basico
			AND pr.name_norm = s.name_norm AND pr.cpf_fragment = s.cpf_fragment
		LEFT JOIN person_record rr ON s.has_rep AND rr.source_id = $1 AND rr.context_key = s.cnpj_basico
			AND rr.name_norm = s.rep_name_norm AND rr.cpf_fragment = s.rep_fragment
		ORDER BY 1`
	sqlBuckets = `SELECT DISTINCT bucket FROM receita_stg_partner ORDER BY 1`
	// Up to three claims per Socios row of bucket $2, one row per natural key
	// (first row wins): partner_of (partner -> company, role = qualification,
	// valid_from = entry date), administrator_of (same, for administration
	// qualifications, $1) and legal_rep_of (representative -> partner, role =
	// representative's qualification, no date). A person end takes the
	// record's current entity, read here under the identity lock.
	sqlRels = `INSERT INTO receita_stg_rel
			(rel_type, src_entity_id, dst_entity_id, src_record_id, dst_record_id, role, valid_from, file_no, line)
		SELECT DISTINCT ON (r.rel_type, coalesce(r.src_record_id, -r.src_entity_id),
				coalesce(r.dst_record_id, -r.dst_entity_id), r.role, r.valid_from)
			r.rel_type, r.src_entity_id, r.dst_entity_id, r.src_record_id, r.dst_record_id, r.role, r.valid_from, r.file_no, r.line
		FROM (
			WITH p AS (
				SELECT p.qual, p.valid_from, p.rep_qual, p.file_no, p.line, p.company_id,
					coalesce(p.partner_entity_id, pr.entity_id) AS partner_entity, p.partner_record_id AS partner_record,
					rr.entity_id AS rep_entity, p.rep_record_id AS rep_record
				FROM receita_stg_partner p
				LEFT JOIN person_record pr ON pr.id = p.partner_record_id
				LEFT JOIN person_record rr ON rr.id = p.rep_record_id
				WHERE p.bucket = $2
			)
			SELECT 'partner_of' AS rel_type, partner_entity AS src_entity_id, company_id AS dst_entity_id,
				partner_record AS src_record_id, NULL::bigint AS dst_record_id, qual AS role, valid_from, file_no, line
			FROM p WHERE partner_entity IS NOT NULL
			UNION ALL
			SELECT 'administrator_of', partner_entity, company_id, partner_record, NULL, qual, valid_from, file_no, line
			FROM p WHERE partner_entity IS NOT NULL AND qual = ANY ($1::text[])
			UNION ALL
			SELECT 'legal_rep_of', rep_entity, partner_entity, rep_record, partner_record, rep_qual, NULL, file_no, line
			FROM p WHERE rep_entity IS NOT NULL AND partner_entity IS NOT NULL
				AND rep_record IS DISTINCT FROM partner_record
		) r
		ORDER BY r.rel_type, coalesce(r.src_record_id, -r.src_entity_id), coalesce(r.dst_record_id, -r.dst_entity_id),
			r.role, r.valid_from, r.file_no, r.line`
	// Re-seen claims: advance last_seen and follow the records' current
	// entities. Rows already at this collection with the right entities are
	// left alone.
	sqlRelSeen = `UPDATE relationship r
		SET last_seen_collection_id = GREATEST(r.last_seen_collection_id, $1),
			src_entity_id = s.src_entity_id, dst_entity_id = s.dst_entity_id
		FROM receita_stg_rel s
		WHERE r.closed_collection_id IS NULL AND r.rel_type = s.rel_type
		  AND coalesce(r.src_record_id, -r.src_entity_id) = coalesce(s.src_record_id, -s.src_entity_id)
		  AND coalesce(r.dst_record_id, -r.dst_entity_id) = coalesce(s.dst_record_id, -s.dst_entity_id)
		  AND (r.attrs ->> 'role') IS NOT DISTINCT FROM s.role
		  AND r.valid_from IS NOT DISTINCT FROM s.valid_from
		  AND (r.last_seen_collection_id IS NULL OR r.last_seen_collection_id < $1
		       OR r.src_entity_id <> s.src_entity_id OR r.dst_entity_id <> s.dst_entity_id)`
	sqlRelInsert = `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, src_record_id, dst_record_id,
			attrs, kind, valid_from, first_collection_id, record_ref, last_seen_collection_id)
		SELECT s.rel_type, s.src_entity_id, s.dst_entity_id, s.src_record_id, s.dst_record_id,
			jsonb_build_object('role', s.role), 'fact', s.valid_from, $1, f.name || ':' || s.line, $1
		FROM receita_stg_rel s JOIN receita_stg_file f ON f.file_no = s.file_no
		WHERE NOT EXISTS (SELECT 1 FROM relationship r
			WHERE r.closed_collection_id IS NULL AND r.rel_type = s.rel_type
			  AND coalesce(r.src_record_id, -r.src_entity_id) = coalesce(s.src_record_id, -s.src_entity_id)
			  AND coalesce(r.dst_record_id, -r.dst_entity_id) = coalesce(s.dst_record_id, -s.dst_entity_id)
			  AND (r.attrs ->> 'role') IS NOT DISTINCT FROM s.role
			  AND r.valid_from IS NOT DISTINCT FROM s.valid_from)`
)

// step is one statement of the set-based phase; stat, when set, receives
// its row count.
type step struct {
	name, sql string
	args      []any
	stat      string
}

// upsert runs the set-based phase in transactions (companies, persons,
// partners, then one per relationship bucket), logging each statement with its row count and
// time. A failure rolls back the current transaction; the earlier ones are
// complete on their own and the next collection redoes the rest idempotently.
func upsert(ctx context.Context, pool *pgxpool.Pool, colID int64, scope []string, depth int, stats connector.Stats) error {
	if err := run(ctx, pool, "analyze staging", nil, []step{
		{name: "analyze receita_stg_empresa", sql: `ANALYZE receita_stg_empresa`},
		{name: "analyze receita_stg_estab", sql: `ANALYZE receita_stg_estab`},
		{name: "analyze receita_stg_socio", sql: `ANALYZE receita_stg_socio`},
	}); err != nil {
		return err
	}
	if scope != nil {
		if err := applyScope(ctx, pool, scope, depth); err != nil {
			return err
		}
	}
	if err := run(ctx, pool, "companies", stats, []step{
		{name: "company_src", sql: sqlCompanySrc},
		{name: "company_orphans", sql: sqlCompanyOrphans, stat: "companies_without_empresas_row"},
		{name: "analyze company_src", sql: `ANALYZE receita_stg_company_src`},
		{name: "entity company", sql: sqlCompanyEntities, stat: "entities_company_inserted"},
		{name: "company ids", sql: sqlCompanyIDs},
		{name: "analyze company", sql: `ANALYZE receita_stg_company`},
		{name: "company_version close", sql: sqlVersionClose, args: []any{colID}, stat: "company_versions_closed"},
		{name: "company_version seen", sql: sqlVersionSeen, args: []any{colID}, stat: "company_versions_seen"},
		{name: "company_version insert", sql: sqlVersionInsert, args: []any{colID}, stat: "company_versions_inserted"},
	}); err != nil {
		return err
	}
	if err := checkpoint("companies"); err != nil {
		return err
	}
	var entitySeq uint32
	if err := pool.QueryRow(ctx, `SELECT pg_get_serial_sequence('entity', 'id')::regclass::oid`).Scan(&entitySeq); err != nil {
		return fmt.Errorf("receita: entity sequence: %w", err)
	}
	if err := run(ctx, pool, "persons", stats, []step{
		{name: "persons", sql: sqlPersons},
		{name: "analyze person", sql: `ANALYZE receita_stg_person`},
		{name: "person_new", sql: sqlPersonNew, args: []any{sourceID, entitySeq}},
		{name: "entity person", sql: sqlPersonEntities, stat: "entities_person_inserted"},
		{name: "person_record insert", sql: sqlPersonRecords, args: []any{sourceID, colID}, stat: "person_records_inserted"},
		{name: "entity foreign", sql: sqlForeignEntities, stat: "entities_foreign_inserted"},
	}); err != nil {
		return err
	}
	if err := checkpoint("persons"); err != nil {
		return err
	}
	if err := run(ctx, pool, "partners", nil, []step{
		{name: "partners", sql: sqlPartners, args: []any{sourceID, relBuckets - 1}},
		{name: "analyze partner", sql: `ANALYZE receita_stg_partner`},
	}); err != nil {
		return err
	}
	if err := relationships(ctx, pool, colID, stats); err != nil {
		return err
	}
	return analyzeClaims(ctx, pool)
}

// claimTables are the big tables a Receita load writes.
var claimTables = []string{"entity", "company_version", "person_record", "relationship"}

// analyzeClaims refreshes the planner statistics of claimTables after the
// bulk phases, each ANALYZE in its own statement outside any long
// transaction, so Resolve, CloseUnseen and the search rebuild plan against
// the new row counts instead of whatever autovacuum has caught up with. It
// samples (300 x default_statistics_target rows per table): seconds, not a
// scan. A failure is only logged: statistics are an optimisation.
func analyzeClaims(ctx context.Context, pool *pgxpool.Pool) error {
	start := time.Now()
	for _, t := range claimTables {
		if _, err := pool.Exec(ctx, `ANALYZE `+t); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("receita: analyze %s: %w", t, err)
			}
			slog.Warn("receita: analyze failed", "table", t, "error", err)
		}
	}
	slog.Info("receita: load phase done", "phase", "analyze", "tables", len(claimTables), "elapsed", since(start))
	return nil
}

// relationships writes the relationships bucket by bucket, each bucket in its
// own short transaction that starts by taking the identity lock.
func relationships(ctx context.Context, pool *pgxpool.Pool, colID int64, stats connector.Stats) error {
	start := time.Now()
	rows, err := pool.Query(ctx, sqlBuckets)
	if err != nil {
		return fmt.Errorf("receita: relationship buckets: %w", err)
	}
	buckets, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return fmt.Errorf("receita: relationship buckets: %w", err)
	}
	var longest time.Duration
	longestBucket := int32(-1)
	for i, b := range buckets {
		t := time.Now()
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if err := identity.LockTx(ctx, tx); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SET LOCAL work_mem = '`+workMem+`'`); err != nil {
				return err
			}
			for _, s := range []step{
				{name: "truncate rel", sql: `TRUNCATE receita_stg_rel`},
				{name: "relationships", sql: sqlRels, args: []any{adminQualifications, b}, stat: "relationships_asserted"},
				// Fresh statistics for this chunk's rows (as TSE does): the
				// UPDATE and INSERT below join it with relationship; a few
				// milliseconds on ~27 k rows.
				{name: "analyze rel", sql: `ANALYZE receita_stg_rel`},
				{name: "relationship seen", sql: sqlRelSeen, args: []any{colID}, stat: "relationships_seen"},
				{name: "relationship insert", sql: sqlRelInsert, args: []any{colID}, stat: "relationships_inserted"},
			} {
				tag, err := tx.Exec(ctx, s.sql, s.args...)
				if err != nil {
					return fmt.Errorf("%s: %w", s.name, err)
				}
				if s.stat != "" {
					stats[s.stat] += tag.RowsAffected()
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("receita: relationships bucket %d: %w", b, err)
		}
		if d := time.Since(t); d > longest {
			longest, longestBucket = d, b
		}
		if (i+1)%64 == 0 || i+1 == len(buckets) {
			slog.Info("receita: relationship chunks", "done", i+1, "of", len(buckets),
				"relationships_inserted", stats["relationships_inserted"], "relationships_seen", stats["relationships_seen"],
				"elapsed", since(start))
		}
		if err := checkpoint(fmt.Sprintf("relationship chunk %d", i+1)); err != nil {
			return err
		}
	}
	slog.Info("receita: load phase done", "phase", "relationships", "chunks", len(buckets),
		"longest_chunk", longest.Round(time.Millisecond).String(), "longest_bucket", longestBucket, "elapsed", since(start))
	return nil
}

// applyScope keeps in staging only the companies reachable from the start
// bases within depth levels of company-to-company partnership.
func applyScope(ctx context.Context, pool *pgxpool.Pool, bases []string, depth int) error {
	steps := []step{{name: "scope start", sql: sqlScopeStart, args: []any{bases}}}
	for i := range depth {
		steps = append(steps, step{name: fmt.Sprintf("scope level %d", i+1), sql: sqlScopeExpand})
	}
	steps = append(steps,
		step{name: "scope filter empresas", sql: sqlScopeEmpresa},
		step{name: "scope filter estabelecimentos", sql: sqlScopeEstab},
		step{name: "scope filter socios", sql: sqlScopeSocio},
		step{name: "analyze receita_stg_socio", sql: `ANALYZE receita_stg_socio`},
	)
	return run(ctx, pool, "scope", nil, steps)
}

// run executes steps in one transaction.
func run(ctx context.Context, pool *pgxpool.Pool, phase string, stats connector.Stats, steps []step) error {
	start := time.Now()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL work_mem = '`+workMem+`'`); err != nil {
			return err
		}
		for _, s := range steps {
			t := time.Now()
			tag, err := tx.Exec(ctx, s.sql, s.args...)
			if err != nil {
				return fmt.Errorf("%s: %w", s.name, err)
			}
			if s.stat != "" && stats != nil {
				stats[s.stat] += tag.RowsAffected()
			}
			slog.Info("receita: load step", "phase", phase, "step", s.name, "rows", tag.RowsAffected(), "elapsed", since(t))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("receita: %s: %w", phase, err)
	}
	slog.Info("receita: load phase done", "phase", phase, "elapsed", since(start))
	return nil
}
