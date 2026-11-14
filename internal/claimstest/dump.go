// Package claimstest is the equivalence test of the load optimisation
// (docs/performance/2026-load-optimization-design.md, task 1): a canonical,
// id-independent dump of the claims state, golden files to compare it with,
// a comparison of the search tables with a from-scratch rebuild, and the
// collection path of the worker for tests (Pipeline). Test code only: no
// production package imports it.
package claimstest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Section names of the canonical dump.
const (
	SecCollection        = "collection"
	SecCollectionFile    = "collection_file"
	SecCollectionFileURL = "collection_file_url"
	SecEntity            = "entity"
	SecEmptyPersons      = "person_entity_without_records"
	SecPersonRecord      = "person_record"
	SecRelationship      = "relationship"
	SecCompanyVersion    = "company_version"
	SecDecision          = "identity_decision"
	SecProposal          = "identity_proposal"
	SecAmbiguousKey      = "identity_ambiguous_key"
	SecResolveDone       = "identity_resolve_done"
	relationshipFirstCol = 11 // field index of the first collection in a relationship line
)

// LastSeen is the SQL expression for the collection that last saw the row
// alias of relationship or company_version (a collection id, or NULL). It is
// the only place the dump reads last_seen_collection_id: a design that
// stores "last seen" differently (task 9, effective last seen) changes this
// function, not the dump.
func LastSeen(alias string) string {
	return alias + ".last_seen_collection_id"
}

// canonTables are temporary tables (dropped at commit) mapping surrogate ids
// to natural identities, in the order they are created:
//   - c_rec: a person record's key, source|context_key|name_norm|cpf_fragment;
//   - c_ent: an entity's identity: kind:natural_key, or for a person the
//     sorted keys of its records, person[k1;k2]; a person entity without
//     records is "person-without-records:<name_norm>" (empty = true). A
//     non-person entity without natural key (none is written today) would be
//     kind~name_norm~name, which two such entities with the same name share:
//     add a natural attribute here if a connector starts writing them;
//   - c_col: a collection's identity, source@reference_date#n, n numbering
//     the collections of one source and date in creation order;
//   - c_dec: an identity decision's identity, kind/action/[sorted record keys].
var canonTables = []string{
	`SET LOCAL DateStyle = 'ISO, YMD'`,
	`SET LOCAL extra_float_digits = 1`,
	`CREATE TEMP TABLE c_rec ON COMMIT DROP AS
		SELECT p.id, s.key || '|' || p.context_key || '|' || p.name_norm || '|' || p.cpf_fragment AS k
		FROM person_record p JOIN source s ON s.id = p.source_id`,
	`CREATE TEMP TABLE c_ent ON COMMIT DROP AS
		SELECT e.id,
			CASE WHEN r.keys IS NOT NULL THEN 'person[' || r.keys || ']'
				WHEN e.natural_key IS NOT NULL THEN e.kind || ':' || e.natural_key
				WHEN e.kind = 'person' THEN 'person-without-records:' || e.name_norm
				ELSE e.kind || '~' || e.name_norm || '~' || e.name END AS k,
			e.kind = 'person' AND r.keys IS NULL AS empty
		FROM entity e
		LEFT JOIN (SELECT p.entity_id, string_agg(c.k, ';' ORDER BY c.k COLLATE "C") AS keys
			FROM person_record p JOIN c_rec c ON c.id = p.id
			WHERE p.entity_id IS NOT NULL GROUP BY p.entity_id) r ON r.entity_id = e.id`,
	`CREATE TEMP TABLE c_col ON COMMIT DROP AS
		SELECT c.id, s.key || '@' || c.reference_date::text || '#'
			|| row_number() OVER (PARTITION BY c.source_id, c.reference_date ORDER BY c.id) AS k
		FROM collection c JOIN source s ON s.id = c.source_id`,
	`CREATE TEMP TABLE c_dec ON COMMIT DROP AS
		SELECT d.id, d.kind || '/' || d.action || '/[' || coalesce((
			SELECT string_agg(c.k, ';' ORDER BY c.k COLLATE "C")
			FROM unnest(d.record_ids) AS x(id) JOIN c_rec c ON c.id = x.id), '') || ']' AS k
		FROM identity_decision d`,
	`CREATE INDEX ON c_rec (id)`,
	`CREATE INDEX ON c_ent (id)`,
	`CREATE INDEX ON c_col (id)`,
	`CREATE INDEX ON c_dec (id)`,
}

// v renders a nullable SQL expression as text, NULL as "~".
func v(expr string) string { return "coalesce((" + expr + ")::text, '~')" }

// FieldSep separates the fields of a dump line (record keys use '|').
const FieldSep = "\t"

// line joins SQL expressions with FieldSep after rendering each with v.
func line(exprs ...string) string {
	parts := make([]string, len(exprs))
	for i, e := range exprs {
		parts[i] = v(e)
	}
	return strings.Join(parts, " || E'\\t' || ")
}

// recordKeys renders an array of person_record ids as their sorted keys.
func recordKeys(arr string) string {
	return `'[' || coalesce((SELECT string_agg(c.k, ';' ORDER BY c.k COLLATE "C")
		FROM unnest(` + arr + `) AS x(id) JOIN c_rec c ON c.id = x.id), '') || ']'`
}

// sectionQueries return one text line per row; lines are sorted in Go
// (byte order), so neither physical order nor the collation matters. Every
// reference to another row goes through c_rec, c_ent, c_col or c_dec.
var sectionQueries = map[string]string{
	// Columns of collection left out, on purpose:
	//   - started_at, finished_at: timestamps of the run;
	//   - stats: the loader's counters, which a faster path may legitimately
	//     change (rows seen or re-pointed per step) without changing claims;
	//   - error: its text names internal steps and buckets (it would change
	//     with task 2's chunks); status says that a collection failed;
	//   - observed_date: for Receita it is fixed by Pipeline (ref +
	//     ObservedAfter) and visible in every closure's valid_to_before; for
	//     TSE it is the day the test runs (raised to the reference date), not
	//     a property of the code.
	SecCollection: `SELECT ` + line("c.k", "x.status", "x.scope") + `
		FROM collection x JOIN c_col c ON c.id = x.id`,
	// sha256 and size_bytes are left out: they describe the fixture's zip
	// bytes (archive/zip output, which a Go release may change), not what
	// the loader did.
	SecCollectionFile: `SELECT ` + line("c.k", "f.name", "f.raw_key") + `
		FROM collection_file f JOIN c_col c ON c.id = f.collection_id`,
	// source_url (migration 0018) in a section of its own, added after the
	// first goldens were recorded (so collection_file kept its text).
	SecCollectionFileURL: `SELECT ` + line("c.k", "f.name", "f.source_url") + `
		FROM collection_file f JOIN c_col c ON c.id = f.collection_id`,
	SecEntity: `SELECT ` + line("ce.k", "e.kind", "e.natural_key", "e.name", "e.name_norm") + `
		FROM entity e JOIN c_ent ce ON ce.id = e.id WHERE NOT ce.empty`,
	// Person entities left without records by identity merges, as a count
	// and their names only: the section a design that stops creating them
	// (task 10) changes, and nothing else.
	SecEmptyPersons: `SELECT 'count ' || count(*) FROM c_ent WHERE empty
		UNION ALL
		SELECT ` + line("e.name", "e.name_norm") + ` FROM entity e JOIN c_ent ce ON ce.id = e.id WHERE ce.empty`,
	SecPersonRecord: `SELECT ` + line("cr.k", "p.name", "ce.k", "p.identity_confidence", "cd.k", "cc.k", "p.record_ref") + `
		FROM person_record p JOIN c_rec cr ON cr.id = p.id
		LEFT JOIN c_ent ce ON ce.id = p.entity_id
		LEFT JOIN c_dec cd ON cd.id = p.locked_by_decision_id
		JOIN c_col cc ON cc.id = p.first_collection_id`,
	// Field order is relied on by MaskFirstCollectionOf.
	SecRelationship: `SELECT ` + line("r.rel_type", "se.k", "de.k", "sr.k", "dr.k", "r.attrs", "r.kind", "r.confidence",
		"r.valid_from", "r.valid_to_after", "r.valid_to_before", "fc.k", "r.record_ref", "ls.k", "cl.k") + `
		FROM relationship r
		JOIN c_ent se ON se.id = r.src_entity_id
		JOIN c_ent de ON de.id = r.dst_entity_id
		LEFT JOIN c_rec sr ON sr.id = r.src_record_id
		LEFT JOIN c_rec dr ON dr.id = r.dst_record_id
		JOIN c_col fc ON fc.id = r.first_collection_id
		LEFT JOIN c_col ls ON ls.id = ` + LastSeen("r") + `
		LEFT JOIN c_col cl ON cl.id = r.closed_collection_id`,
	SecCompanyVersion: `SELECT ` + line("e.kind || ':' || e.natural_key", "v.name", "v.legal_nature", "v.size", "v.uf", "v.city",
		"v.cnae", "v.status", "v.capital", "fc.k", "ls.k", "cl.k", "v.record_ref") + `
		FROM company_version v
		JOIN entity e ON e.id = v.entity_id
		JOIN c_col fc ON fc.id = v.first_collection_id
		LEFT JOIN c_col ls ON ls.id = ` + LastSeen("v") + `
		LEFT JOIN c_col cl ON cl.id = v.closed_collection_id`,
	// A multiset: equal lines are kept (an analyst may take the same decision twice).
	SecDecision: `SELECT ` + line("d.kind", "d.action", recordKeys("d.record_ids"), "ce.k", "d.confidence", "d.reason", "d.decided_by") + `
		FROM identity_decision d LEFT JOIN c_ent ce ON ce.id = d.entity_id`,
	SecProposal: `SELECT ` + line("p.group_key", recordKeys("p.record_ids"), "p.reason", "p.status", "p.connections",
		"p.decided_by", "cd.k") + `
		FROM identity_proposal p LEFT JOIN c_dec cd ON cd.id = p.decision_id`,
	SecAmbiguousKey: `SELECT ` + line("a.group_key", "cd.k") + `
		FROM identity_ambiguous_key a JOIN c_dec cd ON cd.id = a.decision_id`,
	SecResolveDone: `SELECT ` + line("c.k") + ` FROM identity_resolve_done d JOIN c_col c ON c.id = d.collection_id`,
}

// proposalRef finds the surrogate proposal ids the resolver and Decide write
// into a decision's reason ("proposal 12"); they are replaced by the
// proposal's group key.
var proposalRef = regexp.MustCompile(`proposal ([0-9]+)`)

// CanonicalDump reads the claims state behind pool in one REPEATABLE READ
// transaction and returns it as sections of sorted text lines (one section per
// table, see sectionQueries) and the SHA-256 of the whole (Render). Surrogate
// ids, timestamps and the physical order of rows do not appear in it.
// Collections appear by source, reference date and status, never by
// statistics, error text or dates of the run.
func CanonicalDump(ctx context.Context, pool *pgxpool.Pool) (hash string, sections map[string]string, err error) {
	sections = map[string]string{}
	err = withCanon(ctx, pool, func(tx pgx.Tx) error {
		groupKeys := map[string]string{}
		rows, err := tx.Query(ctx, `SELECT id::text, group_key FROM identity_proposal`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, key string
			if err := rows.Scan(&id, &key); err != nil {
				return err
			}
			groupKeys[id] = key
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for name, q := range sectionQueries {
			lines, err := queryLines(ctx, tx, q)
			if err != nil {
				return fmt.Errorf("section %s: %w", name, err)
			}
			if name == SecDecision {
				for i, l := range lines {
					lines[i] = proposalRef.ReplaceAllStringFunc(l, func(m string) string {
						return "proposal {" + groupKeys[proposalRef.FindStringSubmatch(m)[1]] + "}"
					})
				}
			}
			sort.Strings(lines)
			sections[name] = joinLines(lines)
		}
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("claimstest: canonical dump: %w", err)
	}
	return Hash(sections), sections, nil
}

// withCanon runs fn in a REPEATABLE READ transaction holding the canonTables;
// the transaction is rolled back.
func withCanon(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // nothing is kept
	for _, q := range canonTables {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", firstLine(q), err)
		}
	}
	return fn(tx)
}

func queryLines(ctx context.Context, tx pgx.Tx, q string) ([]string, error) {
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// joinLines renders sorted lines as a section's text (one line each, a final
// newline when not empty).
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// splitLines is the inverse of joinLines.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// Render is the text of a dump: every section in name order, each under a
// "== name ==" header.
func Render(sections map[string]string) string {
	var b strings.Builder
	for _, name := range sectionNames(sections) {
		fmt.Fprintf(&b, "== %s ==\n%s", name, sections[name])
	}
	return b.String()
}

// Hash is the SHA-256 of Render(sections).
func Hash(sections map[string]string) string {
	return sha(Render(sections))
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func sectionNames(sections map[string]string) []string {
	names := make([]string, 0, len(sections))
	for n := range sections {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
