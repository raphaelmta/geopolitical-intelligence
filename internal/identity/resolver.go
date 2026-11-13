package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultCommonNameThreshold is used when Resolver.CommonNameThreshold is zero.
//
// Why 100: the published fragment is CPF digits 4-9 and the 9th digit is the
// issuing fiscal region, so two same-name people of one region share a
// fragment with probability about 1e-5, not 1e-6. A name held by k people
// then carries about k^2/2 * 1e-5 expected false merges: 0.05 at k = 100,
// 5 at k = 1000. Lowering the threshold later does NOT undo merges already
// made, so the value must be right before the first national Resolve.
const DefaultCommonNameThreshold = 100

// namesPerBatch bounds the work of one Resolve transaction. Names are
// independent of each other (a group never spans two names), so committing
// per batch is safe: every batch leaves its names fully resolved.
// A variable only so tests can force several batches.
var namesPerBatch = 20000

// afterBatch, when set (tests only), runs after each committed batch.
var afterBatch func()

// beforeCollection, when set (tests only), runs before a collection is
// resolved; an error fails that collection.
var beforeCollection func(collectionID int64) error

// maxAttempts bounds the retries of a transaction that lost a deadlock or a
// serialization conflict (for example against a concurrent ingest).
const maxAttempts = 5

// lockKey is the transaction-level advisory lock taken by every transaction
// that moves person records between entities (Resolve batches, Merge, Split,
// Decide), so those changes never interleave.
const lockKey int64 = 0x6964656e74697479 // "identity"

// resolverUser is decided_by for automatic decisions.
const resolverUser = "resolver"

// validFragment is the SQL predicate for a usable CPF fragment of the
// person_record row with the given alias. Empty, malformed and filler
// fragments never match.
func validFragment(alias string) string {
	return fmt.Sprintf(`(%[1]s.cpf_fragment ~ '^[0-9]{6}$' AND %[1]s.cpf_fragment <> '000000')`, alias)
}

// Resolver decides which person records describe the same human being.
//
// Rules, conservative by design (a wrong merge invents a connection; a missed
// merge only loses a lead; when in doubt, propose):
//   - A group is the records sharing a non-empty normalized name and a valid
//     CPF fragment, from any source. Nothing else ever merges automatically.
//   - n is the number of distinct fragments seen with that name, over all
//     records. If n <= CommonNameThreshold the group merges with confidence
//     max(0.5, 1 - max(n-1, 1)/1e5); otherwise it becomes a proposal for an
//     analyst. The bound: about 1e5 effective fragments (the fragment's last
//     digit is the issuing region), n-1 other fragments already taken by
//     namesakes; it is capped below 1 even for n = 1, so an automatic merge
//     never reads as certain (an analyst's merge is 1). Name and fragment
//     are evidence, not proof.
//   - Merges are never undone by Resolve: lowering CommonNameThreshold later
//     (or a name becoming common) leaves earlier automatic merges in place;
//     only an analyst Split separates them.
//   - Records locked by an analyst decision are never moved by Resolve, and
//     an automatic merge never extends an entity an analyst built: if the
//     target holds a locked record or a record of another key, the group is
//     proposed instead.
//   - A key an analyst Split found on both sides is always proposed.
//   - A rejected proposal's key is never merged or proposed again; new
//     records with a confirmed proposal's key join the confirmed entity,
//     as long as that entity holds only records of that key.
type Resolver struct {
	Pool *pgxpool.Pool
	// CommonNameThreshold is the largest n that still merges automatically.
	// Zero means DefaultCommonNameThreshold.
	CommonNameThreshold int
}

// Result counts what one Resolve changed (summed over the earlier
// collections it recovered and the requested one).
type Result struct {
	// AutoMerged is the number of records moved to another entity.
	AutoMerged int64
	// Proposed is the number of proposals created, refreshed or reopened.
	Proposed int64
	// AutoMergedGroups is the number of name+fragment groups in which some
	// record moved.
	AutoMergedGroups int64
	// ProposalsCreated is the number of proposals created (not refreshed).
	ProposalsCreated int64
	// NamesByFragments counts the touched names by n, their number of
	// distinct valid fragments: 1, 2-10, 11-100, 101-1000, more than 1000.
	// Numbers only (no name): the distribution the national load reports.
	NamesByFragments [5]int64
	// Recovered is the number of earlier collections resolved first.
	Recovered int
}

func (r *Result) add(o Result) {
	r.AutoMerged += o.AutoMerged
	r.Proposed += o.Proposed
	r.AutoMergedGroups += o.AutoMergedGroups
	r.ProposalsCreated += o.ProposalsCreated
	for i := range r.NamesByFragments {
		r.NamesByFragments[i] += o.NamesByFragments[i]
	}
}

// Candidate is a person entity that may be the same person as another one.
type Candidate struct {
	EntityID int64
	Name     string
	Records  int
	// SharedFragment: some record of the candidate has the same name and the
	// same CPF fragment as a record of the entity (it was proposed, rejected
	// or split, never silently ignored).
	SharedFragment bool
}

// Proposal is a group waiting for an analyst.
type Proposal struct {
	ID          int64
	GroupKey    string
	RecordIDs   []int64
	Reason      string
	Connections int
}

func (r *Resolver) threshold() (int, error) {
	switch t := r.CommonNameThreshold; {
	case t == 0:
		return DefaultCommonNameThreshold, nil
	case t < 0:
		return 0, fmt.Errorf("identity: common name threshold %d is negative", t)
	default:
		return t, nil
	}
}

// Resolve groups the person records of the names that collectionID touched.
//
// A collection touches the name_norm of every person record whose
// first_collection_id is that collection. That is enough: a group can only
// change when a new record joins it, and a new record is first seen by
// exactly one collection. Re-running Resolve changes nothing.
//
// Before that, every earlier finished (done or failed) collection that has
// person records but never completed a Resolve is resolved, oldest first, so
// a Resolve that failed midway, or the records of a run whose worker was
// killed (failed by the dead-run check, collection.FailDead), are completed
// by the next one. How many were recovered is logged and in Result.Recovered.
// The Result adds up all of them.
func (r *Resolver) Resolve(ctx context.Context, collectionID int64) (Result, error) {
	threshold, err := r.threshold()
	if err != nil {
		return Result{}, err
	}
	rows, err := r.Pool.Query(ctx, `SELECT c.id FROM collection c
		WHERE c.id < $1 AND c.status IN ('done', 'failed')
			AND NOT EXISTS (SELECT 1 FROM identity_resolve_done d WHERE d.collection_id = c.id)
			AND EXISTS (SELECT 1 FROM person_record p WHERE p.first_collection_id = c.id)
		ORDER BY c.id`, collectionID)
	if err != nil {
		return Result{}, fmt.Errorf("identity: find unresolved collections: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return Result{}, fmt.Errorf("identity: find unresolved collections: %w", err)
	}
	var total Result
	if len(ids) > 0 {
		slog.Info("identity: resolving earlier unresolved collections first", "collection", collectionID, "earlier", len(ids))
	}
	// An earlier collection that keeps failing must not block every later
	// resolve: its error is logged and it stays unmarked for the next try.
	for _, id := range ids {
		res, err := r.resolveCollection(ctx, id, threshold)
		total.add(res)
		if err == nil {
			total.Recovered++
		}
		if err != nil {
			if ctx.Err() != nil {
				return total, fmt.Errorf("identity: resolve earlier collection %d: %w", id, err)
			}
			slog.Error("identity: could not resolve an earlier collection; continuing", "collection", id, "error", err)
		}
	}
	res, err := r.resolveCollection(ctx, collectionID, threshold)
	total.add(res)
	if err != nil {
		return total, fmt.Errorf("identity: resolve collection %d: %w", collectionID, err)
	}
	if len(ids) > 0 {
		slog.Info("identity: earlier collections recovered", "collection", collectionID,
			"recovered", total.Recovered, "of", len(ids))
	}
	return total, nil
}

func (r *Resolver) resolveCollection(ctx context.Context, collectionID int64, threshold int) (Result, error) {
	var total Result
	if beforeCollection != nil {
		if err := beforeCollection(collectionID); err != nil {
			return total, err
		}
	}
	cursor := ""
	for {
		var (
			res  Result
			next string
		)
		err := retryTx(ctx, r.Pool, func(tx pgx.Tx) error {
			var err error
			res, next, err = resolveBatch(ctx, tx, collectionID, cursor, threshold)
			return err
		})
		if err != nil {
			return total, err
		}
		total.add(res)
		if afterBatch != nil {
			afterBatch()
		}
		if next == "" {
			break
		}
		cursor = next
	}
	// Consistency sweep, then the completion marker, in one transaction. The
	// sweep runs once per collection: a collection already marked was swept,
	// and its relationships take their entities from the records since then.
	err := retryTx(ctx, r.Pool, func(tx pgx.Tx) error {
		if err := LockTx(ctx, tx); err != nil {
			return err
		}
		var done bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity_resolve_done WHERE collection_id = $1)`,
			collectionID).Scan(&done)
		if err != nil || done {
			return err
		}
		// One pass over the collection's relationships; only rows whose
		// pointer differs from its record's current entity are updated.
		_, err = tx.Exec(ctx, `UPDATE relationship rel SET src_entity_id = x.src, dst_entity_id = x.dst
			FROM (
				SELECT r.id, coalesce(ps.entity_id, r.src_entity_id) AS src, coalesce(pd.entity_id, r.dst_entity_id) AS dst
				FROM relationship r
				LEFT JOIN person_record ps ON ps.id = r.src_record_id
				LEFT JOIN person_record pd ON pd.id = r.dst_record_id
				WHERE r.first_collection_id = $1
					AND (ps.entity_id <> r.src_entity_id OR pd.entity_id <> r.dst_entity_id)
			) x
			WHERE rel.id = x.id`, collectionID)
		if err != nil {
			return fmt.Errorf("sweep pointers: %w", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO identity_resolve_done (collection_id) VALUES ($1)`, collectionID)
		if err != nil {
			return fmt.Errorf("mark resolved: %w", err)
		}
		return nil
	})
	return total, err
}

// resolveBatch resolves the next namesPerBatch touched names after cursor and
// returns the last name it handled, or "" when there were none left.
func resolveBatch(ctx context.Context, tx pgx.Tx, collectionID int64, cursor string, threshold int) (Result, string, error) {
	var res Result
	if err := LockTx(ctx, tx); err != nil {
		return res, "", err
	}
	// Names are walked in keyset order; '' sorts first, so empty names are skipped.
	// CREATE TABLE AS cannot be prepared, so parameterized ones use the simple
	// protocol (pgx quotes the arguments client-side).
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE res_names ON COMMIT DROP AS
		SELECT DISTINCT name_norm FROM person_record
		WHERE first_collection_id = $1 AND name_norm > $2
		ORDER BY name_norm LIMIT $3`, pgx.QueryExecModeSimpleProtocol, collectionID, cursor, namesPerBatch); err != nil {
		return res, "", fmt.Errorf("touched names: %w", err)
	}
	var names int
	var last string
	if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(max(name_norm), '') FROM res_names`).Scan(&names, &last); err != nil {
		return res, "", fmt.Errorf("count names: %w", err)
	}
	if names == 0 {
		return res, "", nil
	}

	steps := []struct {
		name, sql string
		args      []any
	}{
		// Every record with a touched name and a valid fragment, from all
		// sources and collections, including records not yet linked to an
		// entity: n counts all of them; only linked ones form groups.
		{name: "records", sql: `CREATE TEMP TABLE res_rec ON COMMIT DROP AS
			SELECT p.id, p.entity_id, p.name_norm, p.cpf_fragment,
				p.locked_by_decision_id IS NOT NULL AS locked, p.identity_confidence
			FROM res_names n JOIN person_record p ON p.name_norm = n.name_norm
			WHERE ` + validFragment("p")},
		{name: "analyze records", sql: `ANALYZE res_rec`},
		// One row per group of two or more linked records; singletons need no work.
		{name: "groups", sql: `CREATE TEMP TABLE res_grp ON COMMIT DROP AS
			WITH k AS (
				SELECT name_norm, count(DISTINCT cpf_fragment) AS n FROM res_rec GROUP BY name_norm
			), g AS (
				SELECT name_norm, cpf_fragment,
					count(*) FILTER (WHERE NOT locked) AS unlocked,
					count(DISTINCT entity_id) FILTER (WHERE NOT locked) AS unlocked_entities,
					min(id) FILTER (WHERE NOT locked) AS first_unlocked
				FROM res_rec WHERE entity_id IS NOT NULL
				GROUP BY name_norm, cpf_fragment
				HAVING count(*) > 1
			)
			SELECT g.name_norm, g.cpf_fragment, k.n, g.unlocked, g.unlocked_entities, g.first_unlocked,
				g.name_norm || '|' || g.cpf_fragment AS group_key,
				greatest(0.5, 1 - greatest(k.n - 1, 1)::float8 / 100000)::real AS confidence,
				pr.id AS proposal_id, pr.status, pr.decision_id, pr.record_ids AS proposal_records,
				a.group_key IS NOT NULL AS ambiguous,
				NULL::bigint AS target, false AS impure, ''::text AS action
			FROM g JOIN k USING (name_norm)
			LEFT JOIN identity_proposal pr ON pr.group_key = g.name_norm || '|' || g.cpf_fragment
			LEFT JOIN identity_ambiguous_key a ON a.group_key = g.name_norm || '|' || g.cpf_fragment`},
		// Target of a confirmed key: the current entity of the lowest record
		// still locked by the confirming decision.
		{name: "confirmed targets", sql: `UPDATE res_grp g SET target = (
				SELECT x.entity_id FROM person_record x
				WHERE x.id = ANY (g.proposal_records) AND x.locked_by_decision_id = g.decision_id
				ORDER BY x.id LIMIT 1)
			WHERE g.status = 'confirmed' AND g.unlocked > 0`},
		// Any other target: the entity of the lowest-id unlocked record.
		{name: "targets", sql: `UPDATE res_grp g SET target = f.entity_id
			FROM res_rec f
			WHERE f.id = g.first_unlocked AND g.status IS DISTINCT FROM 'confirmed' AND g.unlocked > 1`},
		{name: "analyze groups", sql: `ANALYZE res_grp`},
		// An automatic merge never extends an entity built by an analyst: the
		// target must hold only records of this key, none of them locked
		// (for a confirmed key, its own locked records are allowed).
		{name: "impure targets", sql: `UPDATE res_grp g SET impure = true
			WHERE g.target IS NOT NULL AND EXISTS (
				SELECT 1 FROM person_record x
				WHERE x.entity_id = g.target
					AND (x.name_norm <> g.name_norm OR x.cpf_fragment <> g.cpf_fragment
						OR (x.locked_by_decision_id IS NOT NULL AND g.status IS DISTINCT FROM 'confirmed')))`},
		{name: "actions", sql: `UPDATE res_grp SET action = CASE
				WHEN status = 'rejected' THEN 'skip'
				-- A confirmed proposal cannot be proposed again. New records join
				-- the confirmed entity only if it is clean and no analyst Split
				-- has since found the key on both sides; otherwise they stay apart.
				WHEN status = 'confirmed' THEN
					CASE WHEN target IS NULL OR impure OR ambiguous THEN 'skip' ELSE 'confirmed' END
				WHEN ambiguous OR impure OR n > $1 THEN 'propose'
				ELSE 'auto' END`, args: []any{threshold}},
		// Unlocked members of groups that merge. Only groups where some member
		// would change (entity or confidence) are kept, so a re-run writes nothing.
		{name: "moves", sql: `CREATE TEMP TABLE res_move ON COMMIT DROP AS
			SELECT * FROM (
				SELECT r.id, r.entity_id AS old_entity, g.target, g.confidence, r.identity_confidence AS old_confidence,
					g.group_key, g.action, g.n, g.proposal_id, r.entity_id <> g.target AS moved,
					bool_or(r.entity_id <> g.target OR r.identity_confidence <> g.confidence)
						OVER (PARTITION BY g.group_key) AS group_changed
				FROM res_rec r JOIN res_grp g USING (name_norm, cpf_fragment)
				WHERE NOT r.locked AND r.entity_id IS NOT NULL AND g.action IN ('auto', 'confirmed')
			) m WHERE group_changed`},
		{name: "decisions", sql: `INSERT INTO identity_decision (kind, action, record_ids, entity_id, confidence, reason, decided_by)
			SELECT 'auto', 'merge', array_agg(id ORDER BY id), min(target), min(confidence),
				CASE WHEN min(action) = 'confirmed'
					THEN format('same name and CPF fragment as proposal %s, confirmed by an analyst; %s distinct fragments for this name',
						min(proposal_id), min(n))
					ELSE format('same name and CPF fragment; %s distinct fragment(s) for this name', min(n)) END,
				'` + resolverUser + `'
			FROM res_move GROUP BY group_key ORDER BY group_key`},
		// Row locks in id order, the order AssertRelationship uses, so a
		// concurrent ingest waits instead of deadlocking. Besides the records
		// that change, this covers every record still in an entity a record
		// leaves: relaxLoneRecords may update those afterwards.
		{name: "lock records", sql: `SELECT count(*) FROM (
			SELECT p.id FROM person_record p JOIN (
				SELECT id FROM res_move WHERE moved OR old_confidence <> confidence
				UNION
				SELECT q.id FROM person_record q WHERE q.entity_id IN (SELECT old_entity FROM res_move WHERE moved)
			) s ON s.id = p.id
			ORDER BY p.id FOR UPDATE OF p) l`},
		{name: "merge", sql: `UPDATE person_record p SET entity_id = m.target, identity_confidence = m.confidence
			FROM res_move m
			WHERE p.id = m.id AND (m.moved OR m.old_confidence <> m.confidence)`},
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return res, "", fmt.Errorf("%s: %w", s.name, err)
		}
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE moved), count(DISTINCT group_key) FILTER (WHERE moved) FROM res_move`).
		Scan(&res.AutoMerged, &res.AutoMergedGroups); err != nil {
		return res, "", fmt.Errorf("count moves: %w", err)
	}
	b := &res.NamesByFragments
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE n = 1), count(*) FILTER (WHERE n BETWEEN 2 AND 10),
			count(*) FILTER (WHERE n BETWEEN 11 AND 100), count(*) FILTER (WHERE n BETWEEN 101 AND 1000),
			count(*) FILTER (WHERE n > 1000)
		FROM (SELECT count(DISTINCT cpf_fragment) AS n FROM res_rec GROUP BY name_norm) k`).
		Scan(&b[0], &b[1], &b[2], &b[3], &b[4]); err != nil {
		return res, "", fmt.Errorf("fragment distribution: %w", err)
	}
	if err := repoint(ctx, tx, `SELECT id FROM res_move`); err != nil {
		return res, "", err
	}
	if err := relaxLoneRecords(ctx, tx, `SELECT old_entity FROM res_move WHERE moved`); err != nil {
		return res, "", err
	}

	// One proposal per group whose unlocked records are spread over two or
	// more entities. Confirmed and rejected proposals are never touched; an
	// obsolete one is reopened.
	rows, err := tx.Query(ctx, `
		WITH members AS (
			SELECT g.group_key, g.n, g.ambiguous, g.impure, r.id
			FROM res_grp g JOIN res_rec r USING (name_norm, cpf_fragment)
			WHERE g.action = 'propose' AND g.unlocked > 1 AND g.unlocked_entities > 1
				AND NOT r.locked AND r.entity_id IS NOT NULL
		), links AS (
			SELECT m.group_key, rel.id FROM members m JOIN relationship rel ON rel.src_record_id = m.id
			UNION
			SELECT m.group_key, rel.id FROM members m JOIN relationship rel ON rel.dst_record_id = m.id
		), conn AS (
			SELECT group_key, count(*) AS c FROM links GROUP BY group_key
		), agg AS (
			SELECT group_key, min(n) AS n, bool_or(ambiguous) AS ambiguous, bool_or(impure) AS impure,
				array_agg(id ORDER BY id) AS ids
			FROM members GROUP BY group_key
		)
		INSERT INTO identity_proposal (group_key, record_ids, reason, connections)
		SELECT a.group_key, a.ids,
			CASE WHEN a.ambiguous THEN 'an analyst split records with this name and CPF fragment apart before'
				WHEN a.impure THEN 'the records would join an entity an analyst built'
				ELSE format('%s distinct CPF fragments share this name (automatic merge up to %s)', a.n, $1::int) END,
			coalesce(c.c, 0)
		FROM agg a LEFT JOIN conn c USING (group_key)
		ORDER BY a.group_key
		ON CONFLICT (group_key) DO UPDATE
			SET record_ids = EXCLUDED.record_ids, reason = EXCLUDED.reason, connections = EXCLUDED.connections,
				status = 'pending', decided_by = NULL, decided_at = NULL
			WHERE identity_proposal.status = 'obsolete'
				OR (identity_proposal.status = 'pending'
					AND (identity_proposal.record_ids, identity_proposal.reason, identity_proposal.connections)
						IS DISTINCT FROM (EXCLUDED.record_ids, EXCLUDED.reason, EXCLUDED.connections))
		RETURNING (xmax = 0)`, threshold)
	if err != nil {
		return res, "", fmt.Errorf("proposals: %w", err)
	}
	created, err := pgx.CollectRows(rows, pgx.RowTo[bool])
	if err != nil {
		return res, "", fmt.Errorf("proposals: %w", err)
	}
	res.Proposed = int64(len(created))
	for _, c := range created {
		if c {
			res.ProposalsCreated++
		}
	}

	// Pending proposals with nothing left to decide: the group merged, or
	// fewer than two unlocked records remain apart.
	_, err = tx.Exec(ctx, `UPDATE identity_proposal p
		SET status = 'obsolete', decided_by = '`+resolverUser+`', decided_at = now()
		FROM res_grp g
		WHERE p.group_key = g.group_key AND p.status = 'pending'
			AND (g.action = 'auto' OR (g.action = 'propose' AND NOT (g.unlocked > 1 AND g.unlocked_entities > 1)))`)
	if err != nil {
		return res, "", fmt.Errorf("obsolete proposals: %w", err)
	}
	return res, last, nil
}

// LockTx takes the identity lock (lockKey) for the rest of tx. Every
// transaction that moves person records between entities takes it, and so
// does every writer that reads person_record.entity_id to store it elsewhere
// (the Receita loader's relationship chunks): under the lock that read is the
// record's current entity until commit.
func LockTx(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("identity lock: %w", err)
	}
	return nil
}

// repoint makes the relationships that cite the records selected by idsSQL
// point at those records' current entities. Only rows whose pointer differs
// are updated (each update pays a row trigger). The natural key of a
// relationship uses the record at each end that has one, so re-pointing
// never makes two claims collide.
func repoint(ctx context.Context, tx pgx.Tx, idsSQL string, args ...any) error {
	for _, side := range []string{"src", "dst"} {
		_, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE relationship rel SET %[1]s_entity_id = p.entity_id
			FROM person_record p
			WHERE p.id IN (%[2]s) AND rel.%[1]s_record_id = p.id AND rel.%[1]s_entity_id <> p.entity_id`, side, idsSQL), args...)
		if err != nil {
			return fmt.Errorf("re-point %s: %w", side, err)
		}
	}
	return nil
}

// relaxLoneRecords gives confidence 1 to a record left alone in one of the
// entities selected by entitiesSQL (records were moved out of them).
func relaxLoneRecords(ctx context.Context, tx pgx.Tx, entitiesSQL string, args ...any) error {
	_, err := tx.Exec(ctx, `UPDATE person_record p SET identity_confidence = 1
		WHERE p.entity_id IN (`+entitiesSQL+`) AND p.identity_confidence <> 1
			AND NOT EXISTS (SELECT 1 FROM person_record q WHERE q.entity_id = p.entity_id AND q.id <> p.id)`, args...)
	if err != nil {
		return fmt.Errorf("relax lone records: %w", err)
	}
	return nil
}

// possibleSameScan caps how many records of the entity's names PossibleSame
// reads to find name-only candidates. Records sharing name and fragment are
// always read (one person's key is small); for a very common name, the
// name-only candidates come from the first possibleSameScan records in index
// order, not from all of them.
const possibleSameScan = 5000

// PossibleSame lists other person entities with a record of the same
// normalized name as a record of entityID, computed on the fly and never
// merged. Candidates sharing a CPF fragment come first, then the ones with
// more records. Entities left without records by a merge are not listed;
// empty names match nothing. limit <= 0 means 50.
func (r *Resolver) PossibleSame(ctx context.Context, entityID int64, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.Pool.Query(ctx, `
		WITH mine AS (
			SELECT DISTINCT p.name_norm, CASE WHEN `+validFragment("p")+` THEN p.cpf_fragment END AS frag
			FROM person_record p JOIN entity e ON e.id = p.entity_id
			WHERE p.entity_id = $1 AND e.kind = 'person' AND p.name_norm <> ''
		), scan AS (
			SELECT p.entity_id, true AS shared
			FROM mine m JOIN person_record p ON p.name_norm = m.name_norm AND p.cpf_fragment = m.frag
			WHERE p.entity_id <> $1
			UNION ALL
			(SELECT p.entity_id, false
			FROM person_record p
			WHERE p.name_norm IN (SELECT name_norm FROM mine) AND p.entity_id <> $1
			LIMIT $3)
		), cand AS (
			SELECT entity_id, bool_or(shared) AS shared FROM scan GROUP BY entity_id
		), counts AS (
			SELECT x.entity_id, count(*) AS records
			FROM person_record x WHERE x.entity_id IN (SELECT entity_id FROM cand)
			GROUP BY x.entity_id
		)
		SELECT c.entity_id, e.name, k.records, c.shared
		FROM cand c JOIN counts k USING (entity_id) JOIN entity e ON e.id = c.entity_id
		WHERE e.kind = 'person'
		ORDER BY c.shared DESC, k.records DESC, c.entity_id
		LIMIT $2`, entityID, limit, possibleSameScan)
	if err != nil {
		return nil, fmt.Errorf("identity: possible same: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Candidate, error) {
		var c Candidate
		err := row.Scan(&c.EntityID, &c.Name, &c.Records, &c.SharedFragment)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("identity: possible same: %w", err)
	}
	return out, nil
}

// Pending returns the pending proposals, most connected first.
func (r *Resolver) Pending(ctx context.Context, limit int) ([]Proposal, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.Pool.Query(ctx, `SELECT id, group_key, record_ids, reason, connections
		FROM identity_proposal WHERE status = 'pending' ORDER BY connections DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("identity: pending proposals: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Proposal])
	if err != nil {
		return nil, fmt.Errorf("identity: pending proposals: %w", err)
	}
	return out, nil
}

// inTx runs fn in a transaction, committing when it returns nil.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// retryTx is inTx, retried a bounded number of times when the transaction
// lost a deadlock or a serialization conflict.
func retryTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	for attempt := 1; ; attempt++ {
		err := inTx(ctx, pool, fn)
		var pgErr *pgconn.PgError
		if err == nil || attempt == maxAttempts || !errors.As(err, &pgErr) ||
			(pgErr.Code != "40001" && pgErr.Code != "40P01") {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
		}
	}
}
