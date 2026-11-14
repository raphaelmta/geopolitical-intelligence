package tse

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

// relBuckets is the number of relationship chunks; a municipal year (about
// 550 k candidacies and a few million receipts) gives a few thousand claims
// per chunk.
const relBuckets = 256

// workMem is set for the set-based transactions.
const workMem = "256MB"

// Set-based phase, with the semantics of internal/claims.Store and of the
// Receita loader (internal/connector/receita/upsert.go):
//   - entity: unique (kind, natural_key); an existing entity keeps its name
//     (first seen wins);
//   - person_record: unique (source_id, context_key, name_norm,
//     cpf_fragment); a new record gets its own new person entity; existing
//     ones are not rewritten. A candidacy (context cand:<year>:<SQ>) has ONE
//     record: once it exists it is reused even when a later publication
//     changes the name or the CPF (the first publication wins);
//   - relationship: the open-row natural key (rel_type, record or -entity
//     at each end, role, valid_from); a re-seen row only advances last_seen
//     (and follows its records' current entities). attrs are immutable.
//   - a receipt (year, SQ_RECEITA, candidacy) is one donation: the donor
//     comes from its first line; a receipt already loaded with another donor
//     or date is left as it is (receipt_conflict).
//
// Inserts find new keys with anti-joins and carry no ON CONFLICT (staging
// holds one row per key and the load lock keeps TSE loads apart). The TSE is
// not a snapshot source: nothing is ever closed.

const (
	// A receipt is loaded only when its candidacy is in this load.
	receiptHasCandidate = `EXISTS (SELECT 1 FROM tse_stg_candidate c WHERE c.year = r.year AND c.sq = r.sq)`
	// The first line of a donation's receipt (donor name, party name).
	firstLine = `JOIN tse_stg_receipt r ON r.file_no = d.file_no AND r.line = d.line`
)

// Candidacies, receipts and parties.
const (
	// One row per candidacy (year, SQ_CANDIDATO): the data of the row of the
	// last round (final result), the date of the first round.
	sqlCandidates = `INSERT INTO tse_stg_candidate (year, sq, context_key, name, name_norm, cpf_fragment, ballot_name,
			office_code, office, uf, city, city_name, party, result, valid_from, file_no, line)
		SELECT DISTINCT ON (c.year, c.sq) c.year, c.sq, 'cand:' || c.year || ':' || c.sq, c.name, c.name_norm, c.cpf_fragment,
			c.ballot_name, c.office_code, c.office, c.uf, c.city, c.city_name, c.party, c.result,
			min(c.election_date) OVER (PARTITION BY c.year, c.sq), c.file_no, c.line
		FROM tse_stg_cand c
		ORDER BY c.year, c.sq, c.turno DESC, c.file_no, c.line`
	// A party donor published with its number but no sigla gets the sigla the
	// candidacies of the same year give that number, when only one does.
	sqlPartyNumbers = `UPDATE tse_stg_receipt r SET donor_party = m.party, donor_key = m.party
		FROM (SELECT year, party_number, min(party) AS party FROM tse_stg_cand
			WHERE party_number <> '' AND party <> '' GROUP BY year, party_number HAVING count(DISTINCT party) = 1) m
		WHERE r.donor_kind = 'party' AND r.donor_party = '' AND r.year = m.year AND r.donor_party_number = m.party_number`
	// One row per receipt (year, SQ_RECEITA, candidacy) of a loaded
	// candidacy: the donor, origin and record_ref from its first line, the
	// amount summed exactly (numeric) over all its lines.
	sqlDonations = `INSERT INTO tse_stg_donation (year, receipt_id, sq, donor_kind, donor_key, donor_name_norm, donor_fragment,
			donor_base, donor_party, donor_sq, amount, receipt_date, origin, origin_code, items, file_no, line)
		SELECT f.year, f.receipt_id, f.sq, f.donor_kind, f.donor_key, f.donor_name_norm, f.donor_fragment,
			f.donor_base, f.donor_party, f.donor_sq, a.amount, a.receipt_date, f.origin, f.origin_code, a.items, f.file_no, f.line
		FROM (
			SELECT DISTINCT ON (r.year, r.receipt_id, r.sq) r.* FROM tse_stg_receipt r
			WHERE ` + receiptHasCandidate + `
			ORDER BY r.year, r.receipt_id, r.sq, r.file_no, r.line
		) f
		JOIN (
			SELECT r.year, r.receipt_id, r.sq, sum(r.amount::numeric) AS amount, min(r.receipt_date) AS receipt_date,
				count(*) AS items
			FROM tse_stg_receipt r WHERE ` + receiptHasCandidate + `
			GROUP BY r.year, r.receipt_id, r.sq
		) a ON a.year = f.year AND a.receipt_id = f.receipt_id AND a.sq = f.sq`
	// Parties of the candidacies first, then of the party donors; the first
	// row wins the name.
	sqlParties = `INSERT INTO entity (kind, natural_key, name, name_norm)
		SELECT DISTINCT ON (p.party) 'party', p.party, p.name, p.name_norm
		FROM (
			SELECT party, party_name AS name, party_name_norm AS name_norm, 0 AS prio, file_no, line
			FROM tse_stg_cand WHERE party <> ''
			UNION ALL
			SELECT d.donor_party, r.donor_party_name, r.donor_party_name_norm, 1, d.file_no, d.line
			FROM tse_stg_donation d ` + firstLine + `
			WHERE d.donor_kind = 'party' AND d.donor_party <> ''
		) p
		WHERE NOT EXISTS (SELECT 1 FROM entity e WHERE e.kind = 'party' AND e.natural_key = p.party)
		ORDER BY p.party, p.prio, p.file_no, p.line`
)

// Person records. The two record steps share sqlPersonNew, sqlPersonEntities
// and sqlPersonRecords over tse_stg_person.
const (
	// Candidacies whose context has no record yet (a known candidacy keeps
	// its record whatever the new publication says).
	sqlCandPersons = `INSERT INTO tse_stg_person (context_key, name, name_norm, cpf_fragment, file_no, line)
		SELECT c.context_key, c.name, c.name_norm, c.cpf_fragment, c.file_no, c.line
		FROM tse_stg_candidate c
		WHERE NOT EXISTS (SELECT 1 FROM person_record r WHERE r.source_id = $1 AND r.context_key = c.context_key)`
	// Individual donors, in the context of the candidacy they gave to.
	sqlDonorPersons = `INSERT INTO tse_stg_person (context_key, name, name_norm, cpf_fragment, file_no, line)
		SELECT DISTINCT ON (p.context_key, p.name_norm, p.cpf_fragment)
			p.context_key, p.name, p.name_norm, p.cpf_fragment, p.file_no, p.line
		FROM (
			SELECT 'donor:' || d.year || ':' || d.sq AS context_key, r.donor_name AS name, d.donor_name_norm AS name_norm,
				d.donor_fragment AS cpf_fragment, d.file_no, d.line
			FROM tse_stg_donation d ` + firstLine + `
			WHERE d.donor_kind = 'person'
		) p
		ORDER BY p.context_key, p.name_norm, p.cpf_fragment, p.file_no, p.line`
	// New keys get an entity id from the entity sequence ($2 is its oid),
	// handed out in key order (PostgreSQL evaluates a volatile function of
	// the select list after the ORDER BY), and records are inserted in that
	// order: candidacy records ('cand:<year>:<SQ>') and donor records
	// ('donor:<year>:<SQ>') get ids in the order of the candidacies, the
	// order of the relationship chunks (claimBuckets).
	sqlPersonNew = `INSERT INTO tse_stg_person_new (entity_id, context_key, name, name_norm, cpf_fragment, file_no, line)
		SELECT nextval($2::oid::regclass), p.context_key, p.name, p.name_norm, p.cpf_fragment, p.file_no, p.line
		FROM tse_stg_person p
		WHERE NOT EXISTS (SELECT 1 FROM person_record r WHERE r.source_id = $1 AND r.context_key = p.context_key
			AND r.name_norm = p.name_norm AND r.cpf_fragment = p.cpf_fragment)
		ORDER BY p.context_key, p.name_norm, p.cpf_fragment`
	sqlPersonEntities = `INSERT INTO entity (id, kind, name, name_norm)
		SELECT entity_id, 'person', name, name_norm FROM tse_stg_person_new ORDER BY entity_id`
	sqlPersonRecords = `INSERT INTO person_record (source_id, context_key, name, name_norm, cpf_fragment, entity_id,
			identity_confidence, first_collection_id, record_ref)
		SELECT $1, n.context_key, n.name, n.name_norm, n.cpf_fragment, n.entity_id, 1, $2, f.name || ':' || n.line
		FROM tse_stg_person_new n JOIN tse_stg_file f ON f.file_no = n.file_no
		ORDER BY n.entity_id`
	sqlPersonClear = `TRUNCATE tse_stg_person, tse_stg_person_new`
)

// Receipts already loaded with another donor or date: an open donated_to to
// the same candidacy record with the same receipt id (role) whose source or
// valid_from differ from this publication. They are dropped from the load
// (counted as receipt_conflict) before any donor record is created, and the
// stored row is left untouched.
//
// The candidacy record is the one the claims use (candRecordContext: lowest
// id of the context). The relationship probe matches the partial index
// relationship_open_receipt (migration 0016) exactly: rel_type =
// 'donated_to', closed_collection_id IS NULL, dst_record_id, attrs ->> 'role'.
// It runs once per load, over the donations (not per bucket). This probe is
// the only user of relationship_open_receipt: the header of migration 0016
// also names the insert guard (sqlRelInsert), which no longer probes by
// receipt (it anti-joins on the natural key, relationship_natural_key); the
// shipped migration is left unchanged and this is the current account.
const sqlReceiptConflicts = `DELETE FROM tse_stg_donation d
		WHERE EXISTS (SELECT 1 FROM tse_stg_candidate c
		  JOIN LATERAL (` + candRecordContext + `) cr ON true
		  WHERE c.year = d.year AND c.sq = d.sq
		  AND EXISTS (SELECT 1 FROM relationship x
		  WHERE x.rel_type = 'donated_to' AND x.closed_collection_id IS NULL
			AND x.dst_record_id = cr.id AND (x.attrs ->> 'role') = d.receipt_id
		  AND NOT (x.valid_from IS NOT DISTINCT FROM d.receipt_date AND CASE d.donor_kind
			WHEN 'person' THEN EXISTS (SELECT 1 FROM person_record s WHERE s.id = x.src_record_id
				AND s.context_key = 'donor:' || d.year || ':' || d.sq
				AND s.name_norm = d.donor_name_norm AND s.cpf_fragment = d.donor_fragment)
			WHEN 'candidate' THEN EXISTS (SELECT 1 FROM person_record s WHERE s.id = x.src_record_id
				AND s.context_key = 'cand:' || d.year || ':' || d.donor_sq)
			WHEN 'company' THEN x.src_record_id IS NULL AND EXISTS (SELECT 1 FROM entity e
				WHERE e.id = x.src_entity_id AND e.kind = 'company' AND e.natural_key = d.donor_base)
			WHEN 'party' THEN x.src_record_id IS NULL AND EXISTS (SELECT 1 FROM entity e
				WHERE e.id = x.src_entity_id AND e.kind = 'party' AND e.natural_key = d.donor_party)
			ELSE false END)))`

// Counters computed in SQL.
const (
	sqlCandidatesWithoutParty = `SELECT count(*) FROM tse_stg_candidate WHERE party = ''`
	sqlUnknownCandidates      = `SELECT count(DISTINCT (r.year, r.receipt_id, r.sq)) FROM tse_stg_receipt r WHERE NOT ` + receiptHasCandidate
	sqlMixedDonors            = `SELECT count(*) FROM (SELECT 1 FROM tse_stg_receipt r WHERE ` + receiptHasCandidate + `
		GROUP BY r.year, r.receipt_id, r.sq HAVING count(DISTINCT (r.donor_kind, r.donor_key)) > 1) x`
	sqlUnknownParties = `SELECT count(*) FROM tse_stg_donation WHERE donor_kind = 'party' AND donor_party = ''`
	// Donations from another candidacy that are not loaded, per receipt
	// (tse_stg_donation has one row per receipt). The donor candidacy named
	// by SQ_CANDIDATO_DOADOR is either the receiving candidacy itself
	// (donor_candidate_self: a transfer between a candidate's own accounts,
	// never a self-loop) or absent from the year's candidacies file
	// (donor_candidate_not_in_file). A receipt with no donor SQ at all is
	// counted while parsing (donor_candidate_unpublished, per receipt too).
	sqlSelfDonors      = `SELECT count(*) FROM tse_stg_donation d WHERE d.donor_kind = 'candidate' AND d.donor_sq = d.sq`
	sqlDonorsNotInFile = `SELECT count(*) FROM tse_stg_donation d WHERE d.donor_kind = 'candidate' AND d.donor_sq <> d.sq
		AND NOT EXISTS (SELECT 1 FROM tse_stg_candidate c WHERE c.year = d.year AND c.sq = d.donor_sq)`
)

// Claims. A claim's bucket is the one of its candidacy: a range of
// candidacy context keys (connector.RangeChunks, weighted by the candidacy's
// claims: its candidate_for and its donations), so all claims of a
// candidacy share a bucket, buckets hold about equal numbers of claims, and a
// bucket reads and writes short ranges of the candidacy and donor records
// (their ids follow the same order, see sqlPersonNew) and of relationship's
// indexes. Both inserts compute the same buckets: they run in one
// transaction over the same staging. The ranges are recomputed from staging
// at every load; nothing depends on them across loads.
//
// Each insert is sorted by bucket so the BRIN index on tse_stg_claim.bucket
// makes each bucket's read a short range scan (in the 2024 trial, unsorted
// claims cost a full scan of 2 M rows per bucket).
var (
	claimBuckets = connector.RangeChunks(`SELECT context_key FROM tse_stg_candidate
		UNION ALL
		SELECT c.context_key FROM tse_stg_donation d JOIN tse_stg_candidate c ON c.year = d.year AND c.sq = d.sq`, relBuckets)
	// candidate_for: candidacy record -> party entity.
	sqlClaimsCandidate = `INSERT INTO tse_stg_claim (bucket, rel_type, src_entity_id, src_record_id, dst_entity_id, dst_record_id,
			role, valid_from, attrs, file_no, line)
		SELECT b.bucket, 'candidate_for', NULL, cr.id, p.id, NULL,
			c.year || ':' || c.office_code || ':' || c.uf || ':' || c.city, c.valid_from,
			jsonb_build_object('year', c.year, 'office', c.office, 'office_code', c.office_code, 'uf', c.uf,
				'city', c.city, 'city_name', c.city_name, 'result', c.result, 'ballot_name', c.ballot_name),
			c.file_no, c.line
		FROM tse_stg_candidate c
		JOIN (` + claimBuckets + `) b ON b.key = c.context_key
		JOIN LATERAL (` + candRecordContext + `) cr ON true
		JOIN entity p ON p.kind = 'party' AND p.natural_key = c.party
		ORDER BY b.bucket`
	// donated_to: donor (person record, company or party entity, or the
	// record of the donor candidacy) -> candidacy record.
	sqlClaimsDonation = `INSERT INTO tse_stg_claim (bucket, rel_type, src_entity_id, src_record_id, dst_entity_id, dst_record_id,
			role, valid_from, attrs, file_no, line)
		SELECT b.bucket, 'donated_to',
			CASE d.donor_kind WHEN 'company' THEN co.id WHEN 'party' THEN pa.id END,
			CASE d.donor_kind WHEN 'person' THEN pr.id WHEN 'candidate' THEN dr.id END,
			NULL, cr.id, d.receipt_id, d.receipt_date,
			jsonb_build_object('year', d.year, 'amount', d.amount::text, 'date', to_char(d.receipt_date, 'YYYY-MM-DD'),
				'origin', d.origin, 'origin_code', d.origin_code, 'receipt_id', d.receipt_id, 'items', d.items),
			d.file_no, d.line
		FROM tse_stg_donation d
		JOIN tse_stg_candidate c ON c.year = d.year AND c.sq = d.sq
		JOIN (` + claimBuckets + `) b ON b.key = c.context_key
		JOIN LATERAL (` + candRecordContext + `) cr ON true
		LEFT JOIN person_record pr ON d.donor_kind = 'person' AND pr.source_id = $1
			AND pr.context_key = 'donor:' || d.year || ':' || d.sq
			AND pr.name_norm = d.donor_name_norm AND pr.cpf_fragment = d.donor_fragment
		LEFT JOIN entity co ON d.donor_kind = 'company' AND co.kind = 'company' AND co.natural_key = d.donor_base
		LEFT JOIN entity pa ON d.donor_kind = 'party' AND pa.kind = 'party' AND pa.natural_key = d.donor_party
		LEFT JOIN tse_stg_candidate dc ON d.donor_kind = 'candidate' AND dc.year = d.year AND dc.sq = d.donor_sq AND dc.sq <> d.sq
		LEFT JOIN LATERAL (SELECT r.id FROM person_record r WHERE r.source_id = $1 AND r.context_key = dc.context_key
			ORDER BY r.id LIMIT 1) dr ON true
		WHERE coalesce(pr.id, co.id, pa.id, dr.id) IS NOT NULL
		ORDER BY b.bucket`
)

const (
	// companies of the donors (first line's name; Receita's name wins when
	// the company is already known).
	sqlCompanies = `INSERT INTO entity (kind, natural_key, name, name_norm)
		SELECT DISTINCT ON (d.donor_base) 'company', d.donor_base, r.donor_name, r.donor_name_norm
		FROM tse_stg_donation d ` + firstLine + `
		WHERE d.donor_kind = 'company'
		  AND NOT EXISTS (SELECT 1 FROM entity e WHERE e.kind = 'company' AND e.natural_key = d.donor_base)
		ORDER BY d.donor_base, d.file_no, d.line`
	// The record of a candidacy: the first one of its context (there is one
	// per context, since the first publication wins).
	candRecordContext = `SELECT r.id FROM person_record r WHERE r.source_id = $1 AND r.context_key = c.context_key ORDER BY r.id LIMIT 1`
)

// Relationships of one bucket, written under the identity lock: the records'
// current entities are read here.
const (
	sqlRels = `INSERT INTO tse_stg_rel (rel_type, src_entity_id, dst_entity_id, src_record_id, dst_record_id, role, valid_from,
			attrs, file_no, line)
		SELECT DISTINCT ON (r.rel_type, coalesce(r.src_record_id, -r.src_entity_id),
				coalesce(r.dst_record_id, -r.dst_entity_id), r.role, r.valid_from)
			r.rel_type, r.src_entity_id, r.dst_entity_id, r.src_record_id, r.dst_record_id, r.role, r.valid_from,
			r.attrs, r.file_no, r.line
		FROM (
			SELECT c.rel_type, coalesce(c.src_entity_id, sr.entity_id) AS src_entity_id,
				coalesce(c.dst_entity_id, dr.entity_id) AS dst_entity_id, c.src_record_id, c.dst_record_id,
				c.role, c.valid_from, c.attrs || jsonb_build_object('role', c.role) AS attrs, c.file_no, c.line
			FROM tse_stg_claim c
			LEFT JOIN person_record sr ON sr.id = c.src_record_id
			LEFT JOIN person_record dr ON dr.id = c.dst_record_id
			WHERE c.bucket = $1
		) r
		ORDER BY r.rel_type, coalesce(r.src_record_id, -r.src_entity_id), coalesce(r.dst_record_id, -r.dst_entity_id),
			r.role, r.valid_from, r.file_no, r.line`
	// role is never NULL in tse_stg_rel (NOT NULL), so plain equality on
	// attrs ->> 'role' matches the natural key and lets relationship_natural_key
	// use it as an index condition (IS NOT DISTINCT FROM cannot be one).
	sqlRelSeen = `UPDATE relationship r
		SET last_seen_collection_id = GREATEST(r.last_seen_collection_id, $1),
			src_entity_id = s.src_entity_id, dst_entity_id = s.dst_entity_id
		FROM tse_stg_rel s
		WHERE r.closed_collection_id IS NULL AND r.rel_type = s.rel_type
		  AND coalesce(r.src_record_id, -r.src_entity_id) = coalesce(s.src_record_id, -s.src_entity_id)
		  AND coalesce(r.dst_record_id, -r.dst_entity_id) = coalesce(s.dst_record_id, -s.dst_entity_id)
		  AND (r.attrs ->> 'role') = s.role
		  AND r.valid_from IS NOT DISTINCT FROM s.valid_from
		  AND (r.last_seen_collection_id IS NULL OR r.last_seen_collection_id < $1
		       OR r.src_entity_id <> s.src_entity_id OR r.dst_entity_id <> s.dst_entity_id)`
	// New claims. A donated_to whose receipt is already open for the
	// candidacy with another donor or date never gets here: the load drops
	// it once, before the buckets (sqlReceiptConflicts); the TSE load lock
	// keeps other TSE writers out meanwhile. No per-bucket receipt probe.
	sqlRelInsert = `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, src_record_id, dst_record_id,
			attrs, kind, valid_from, first_collection_id, record_ref, last_seen_collection_id)
		SELECT s.rel_type, s.src_entity_id, s.dst_entity_id, s.src_record_id, s.dst_record_id,
			s.attrs, 'fact', s.valid_from, $1, f.name || ':' || s.line, $1
		FROM tse_stg_rel s JOIN tse_stg_file f ON f.file_no = s.file_no
		WHERE NOT EXISTS (SELECT 1 FROM relationship r
			WHERE r.closed_collection_id IS NULL AND r.rel_type = s.rel_type
			  AND coalesce(r.src_record_id, -r.src_entity_id) = coalesce(s.src_record_id, -s.src_entity_id)
			  AND coalesce(r.dst_record_id, -r.dst_entity_id) = coalesce(s.dst_record_id, -s.dst_entity_id)
			  AND (r.attrs ->> 'role') = s.role
			  AND r.valid_from IS NOT DISTINCT FROM s.valid_from)`
)

// stagingHook, when set (tests only), runs at the named points of the
// set-based phase inside its transaction (steps without sql), and after the
// claims are written, so tests can read staging while it is populated.
var stagingHook func(ctx context.Context, q pgx.Tx, point string)

// step is one statement of the set-based phase; stat, when set, receives
// its row count. A step without sql is a stagingHook point.
type step struct {
	name, sql string
	args      []any
	stat      string
}

// personSteps turn the keys in tse_stg_person into new person records.
func personSteps(colID int64, entitySeq uint32) []step {
	return []step{
		{name: "analyze person", sql: `ANALYZE tse_stg_person`},
		{name: "person_new", sql: sqlPersonNew, args: []any{sourceID, entitySeq}},
		{name: "entity person", sql: sqlPersonEntities, stat: "entities_person_inserted"},
		{name: "person_record insert", sql: sqlPersonRecords, args: []any{sourceID, colID}, stat: "person_records_inserted"},
		{name: "person staging"}, // stagingHook point (tests)
		{name: "clear person staging", sql: sqlPersonClear},
	}
}

// upsert runs the set-based phase: candidacies, receipts and parties;
// candidacy records; receipt conflicts; donors; claims; then one short
// transaction per relationship bucket. A failure rolls back the current
// transaction; the earlier ones are complete on their own and the next
// collection redoes the rest idempotently.
func upsert(ctx context.Context, pool *pgxpool.Pool, colID int64, stats connector.Stats) error {
	var entitySeq uint32
	if err := pool.QueryRow(ctx, `SELECT pg_get_serial_sequence('entity', 'id')::regclass::oid`).Scan(&entitySeq); err != nil {
		return fmt.Errorf("tse: entity sequence: %w", err)
	}
	if err := run(ctx, pool, "candidacies and receipts", stats, []step{
		{name: "analyze tse_stg_cand", sql: `ANALYZE tse_stg_cand`},
		{name: "analyze tse_stg_receipt", sql: `ANALYZE tse_stg_receipt`},
		{name: "candidates", sql: sqlCandidates},
		{name: "analyze candidate", sql: `ANALYZE tse_stg_candidate`},
		{name: "party numbers", sql: sqlPartyNumbers, stat: "party_number_mapped"},
		{name: "donations", sql: sqlDonations, stat: "donations"},
		{name: "analyze donation", sql: `ANALYZE tse_stg_donation`},
		{name: "entity party", sql: sqlParties, stat: "entities_party_inserted"},
	}); err != nil {
		return err
	}
	if err := count(ctx, pool, stats, map[string]string{
		"candidates_without_party":    sqlCandidatesWithoutParty,
		"candidate_unknown":           sqlUnknownCandidates,
		"receipt_mixed_donor":         sqlMixedDonors,
		"party_unknown":               sqlUnknownParties,
		"donor_candidate_self":        sqlSelfDonors,
		"donor_candidate_not_in_file": sqlDonorsNotInFile,
	}); err != nil {
		return err
	}
	if err := run(ctx, pool, "candidacy records", stats, append([]step{
		{name: "candidacy persons", sql: sqlCandPersons, args: []any{sourceID}},
	}, personSteps(colID, entitySeq)...)); err != nil {
		return err
	}
	if err := run(ctx, pool, "donors", stats, append([]step{
		{name: "receipt conflicts", sql: sqlReceiptConflicts, args: []any{sourceID}, stat: "receipt_conflict"},
		{name: "entity company", sql: sqlCompanies, stat: "entities_company_inserted"},
		{name: "donor persons", sql: sqlDonorPersons},
	}, personSteps(colID, entitySeq)...)); err != nil {
		return err
	}
	if err := run(ctx, pool, "claims", stats, []step{
		{name: "claims candidate_for", sql: sqlClaimsCandidate, args: []any{sourceID}},
		{name: "claims donated_to", sql: sqlClaimsDonation, args: []any{sourceID}},
		{name: "analyze claim", sql: `ANALYZE tse_stg_claim`},
	}); err != nil {
		return err
	}
	if err := relationships(ctx, pool, colID, stats); err != nil {
		return err
	}
	return analyzeClaims(ctx, pool)
}

// claimTables are the big tables a TSE load writes.
var claimTables = []string{"entity", "person_record", "relationship"}

// analyzeClaims refreshes the planner statistics of claimTables after the
// bulk phases, one ANALYZE per statement outside any long transaction, so
// Resolve and the search rebuild plan against the new row counts. ANALYZE
// samples: seconds, not a scan. A failure is only logged.
func analyzeClaims(ctx context.Context, pool *pgxpool.Pool) error {
	for _, t := range claimTables {
		if _, err := pool.Exec(ctx, `ANALYZE `+t); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("tse: analyze %s: %w", t, err)
			}
			slog.Warn("tse: analyze failed", "table", t, "error", err)
		}
	}
	return nil
}

// count adds the result of each counting query to stats.
func count(ctx context.Context, pool *pgxpool.Pool, stats connector.Stats, queries map[string]string) error {
	for stat, q := range queries {
		var n int64
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			return fmt.Errorf("tse: count %s: %w", stat, err)
		}
		stats[stat] += n
	}
	return nil
}

// relationships writes the claims bucket by bucket, each bucket in its own
// short transaction that starts by taking the identity lock.
func relationships(ctx context.Context, pool *pgxpool.Pool, colID int64, stats connector.Stats) error {
	start := time.Now()
	rows, err := pool.Query(ctx, `SELECT DISTINCT bucket FROM tse_stg_claim ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("tse: relationship buckets: %w", err)
	}
	buckets, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return fmt.Errorf("tse: relationship buckets: %w", err)
	}
	var longest time.Duration
	spent := map[string]time.Duration{}
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
				{name: "truncate rel", sql: `TRUNCATE tse_stg_rel`},
				{name: "relationships", sql: sqlRels, args: []any{b}, stat: "relationships_asserted"},
				// Without statistics the planner takes the bucket's few
				// thousand rows for many and joins relationship with a
				// full scan per bucket instead of its indexes.
				{name: "analyze rel", sql: `ANALYZE tse_stg_rel`},
				{name: "relationship staging"}, // stagingHook point (tests)
				{name: "relationship seen", sql: sqlRelSeen, args: []any{colID}, stat: "relationships_seen"},
				{name: "relationship insert", sql: sqlRelInsert, args: []any{colID}, stat: "relationships_inserted"},
			} {
				t := time.Now()
				if s.sql == "" {
					if stagingHook != nil {
						stagingHook(ctx, tx, s.name)
					}
					continue
				}
				tag, err := tx.Exec(ctx, s.sql, s.args...)
				spent[s.name] += time.Since(t)
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
			return fmt.Errorf("tse: relationships bucket %d: %w", b, err)
		}
		longest = max(longest, time.Since(t))
		if (i+1)%64 == 0 || i+1 == len(buckets) {
			slog.Info("tse: relationship chunks", "done", i+1, "of", len(buckets),
				"relationships_inserted", stats["relationships_inserted"], "relationships_seen", stats["relationships_seen"],
				"elapsed", since(start))
		}
	}
	for _, name := range []string{"relationships", "analyze rel", "relationship seen", "relationship insert"} {
		slog.Info("tse: relationship step total", "step", name, "elapsed", spent[name].Round(time.Millisecond).String())
	}
	slog.Info("tse: load phase done", "phase", "relationships", "chunks", len(buckets),
		"longest_chunk", longest.Round(time.Millisecond).String(), "elapsed", since(start))
	return nil
}

// run executes steps in one transaction.
func run(ctx context.Context, pool *pgxpool.Pool, phase string, stats connector.Stats, steps []step) error {
	start := time.Now()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL work_mem = '`+workMem+`'`); err != nil {
			return err
		}
		for _, s := range steps {
			if s.sql == "" {
				if stagingHook != nil {
					stagingHook(ctx, tx, s.name)
				}
				continue
			}
			t := time.Now()
			tag, err := tx.Exec(ctx, s.sql, s.args...)
			if err != nil {
				return fmt.Errorf("%s: %w", s.name, err)
			}
			if s.stat != "" {
				stats[s.stat] += tag.RowsAffected()
			}
			slog.Info("tse: load step", "phase", phase, "step", s.name, "rows", tag.RowsAffected(), "elapsed", since(t))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("tse: %s: %w", phase, err)
	}
	slog.Info("tse: load phase done", "phase", phase, "elapsed", since(start))
	return nil
}
