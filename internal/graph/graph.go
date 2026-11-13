// Package graph answers read-only queries over the claims graph: the
// neighbors of an entity, the shortest paths between two entities and the
// control group of a company. It never writes.
package graph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
)

// Errors returned by the queries.
var (
	ErrNotFound   = errors.New("graph: entity not found")
	ErrBadCursor  = errors.New("graph: invalid cursor")
	ErrBadOptions = errors.New("graph: invalid options")
)

// EntityRef identifies an entity in results.
type EntityRef struct {
	ID         int64             `json:"id"`
	Kind       claims.EntityKind `json:"kind"`
	Name       string            `json:"name"`
	NaturalKey string            `json:"natural_key"`
}

// Edge is one relationship claim with its provenance.
type Edge struct {
	RelationshipID int64            `json:"relationship_id"`
	Type           claims.RelType   `json:"type"`
	Src            EntityRef        `json:"src"`
	Dst            EntityRef        `json:"dst"`
	Attrs          map[string]any   `json:"attrs"`
	Kind           claims.ClaimKind `json:"kind"`
	// Confidence is null for facts.
	Confidence *float64 `json:"confidence"`
	// IdentityConfidence is the lowest identity confidence among the person
	// records the claim cites; nil when it cites none.
	IdentityConfidence *float64   `json:"identity_confidence,omitempty"`
	ValidFrom          *time.Time `json:"valid_from,omitempty"`
	ValidToAfter       *time.Time `json:"valid_to_after,omitempty"`
	ValidToBefore      *time.Time `json:"valid_to_before,omitempty"`
	// SourceKey and CollectionID are where the claim was first read.
	SourceKey    string `json:"source_key"`
	CollectionID int64  `json:"collection_id"`
	// Stale: the source's last done full-scope collection is older than its
	// StaleAfter (or the source has no done full-scope collection at all),
	// counted from its observed date for an edition source, else from its
	// reference date (SourceStaleSQL).
	Stale bool `json:"stale"`
	// Closed: the claim is closed (closed_collection_id is set): the source
	// stopped listing it or declared it ended.
	Closed bool `json:"closed"`
	// IdentityBasis is what the identity of the claim's person ends rests on:
	// IdentityNone, IdentityAuto or IdentityAnalyst. Results stored before it
	// existed decode it as "".
	IdentityBasis string `json:"identity_basis"`
}

// Identity bases of an edge (Edge.IdentityBasis). The basis of each person
// end is that of its record's entity; the edge takes the strongest claim of
// inference among its ends: analyst over auto over none.
const (
	// IdentityNone: no person record is involved, or every involved person
	// entity consists of exactly one record.
	IdentityNone = "none"
	// IdentityAuto: an involved entity has more than one record, joined
	// automatically (no record of it is locked by an analyst decision).
	IdentityAuto = "auto"
	// IdentityAnalyst: an involved entity has more than one record and at
	// least one of them was placed by an analyst decision (merge or
	// confirmation: locked_by_decision_id is set).
	IdentityAnalyst = "analyst"
)

// Options selects the edges a query walks.
//
// By default only open edges (closed_collection_id IS NULL) are used.
// IncludeClosed adds the closed ones. At selects the edges valid on that
// date, open or closed: valid_from is unknown or not after At, and the row is
// open, or its closing interval is unknown, or At is before valid_to_before.
// A closed row whose closing interval ends after At is still valid at At, so
// At implies closed rows are considered whatever IncludeClosed says.
// RelTypes, when not empty, keeps only those relationship types.
// ClosedOnly keeps only closed edges (closed_collection_id set); it implies
// IncludeClosed, and with At it keeps the closed edges valid on that date.
type Options struct {
	RelTypes      []claims.RelType
	At            *time.Time
	IncludeClosed bool
	ClosedOnly    bool
}

// PathOptions configures Paths. Zero values mean the defaults: MaxDepth 6
// (also the maximum; larger values are clamped), HubDegree 500, MaxPaths 20.
type PathOptions struct {
	Options
	MaxDepth, HubDegree, MaxPaths int
}

// Hop is one step of a path. Forward: the path walks the edge from Src to Dst.
type Hop struct {
	Edge    Edge `json:"edge"`
	Forward bool `json:"forward"`
}

// PathsResult holds the shortest paths found. StoppedHubs are intermediate
// entities not expanded because their degree exceeds HubDegree. Truncated:
// more paths exist than MaxPaths, or the search hit its memory bound.
type PathsResult struct {
	Paths       [][]Hop   `json:"paths"`
	StoppedHubs []HubStop `json:"stopped_hubs"`
	Truncated   bool      `json:"truncated"`
}

// HubStop is a hub the path search did not expand, with its real degree.
type HubStop struct {
	Entity EntityRef `json:"entity"`
	Degree int       `json:"degree"`
}

// ControlMode says which edges carry control in ControlGroup.
type ControlMode string

// Control modes.
const (
	// Documented follows only controls, shareholder_of above 50 percent, and
	// partner_of into a company that has exactly one open partner.
	Documented ControlMode = "documented"
	// CorporateLinks follows every partner_of and shareholder_of;
	// administrator_of edges are reported but not followed.
	CorporateLinks ControlMode = "corporate_links"
)

// ControlOptions configures ControlGroup. MaxPartners 0 means 50.
type ControlOptions struct {
	Mode        ControlMode
	MaxPartners int
}

// Member is a company of the group. Depth is the number of control hops from
// the nearest final controller; Via the relationship ids of that chain.
type Member struct {
	Entity EntityRef `json:"entity"`
	Depth  int       `json:"depth"`
	Via    []int64   `json:"via"`
}

// Stop reasons.
const (
	StopFund            = "fund"
	StopForeign         = "foreign"
	StopTooManyPartners = "too_many_partners"
	// StopTooLarge is reported on the root when the group passed maxMembers
	// and the search stopped.
	StopTooLarge = "too_large"
)

// Stop is an entity where the control search stopped, and why.
type Stop struct {
	Entity EntityRef `json:"entity"`
	Reason string    `json:"reason"`
}

// ControlResult is the control group of Root: its final controllers, the
// member companies, every edge used (and administrator edges in
// corporate_links mode) and where the search stopped.
type ControlResult struct {
	Root        EntityRef   `json:"root"`
	Controllers []EntityRef `json:"controllers"`
	Members     []Member    `json:"members"`
	Edges       []Edge      `json:"edges"`
	Stops       []Stop      `json:"stops"`
}

// Graph runs the queries on a pool.
type Graph struct {
	pool *pgxpool.Pool
	// Memory bounds; tests lower them (export_test.go).
	maxVisited, maxMembers int
}

// New returns a Graph on the pool.
func New(p *pgxpool.Pool) *Graph {
	return &Graph{pool: p, maxVisited: defaultMaxVisited, maxMembers: defaultMaxMembers}
}

// read runs f in a read-only, repeatable-read transaction, so a query that
// takes several statements sees one snapshot.
func (g *Graph) read(ctx context.Context, f func(tx pgx.Tx) error) error {
	tx, err := g.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("graph: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; nothing to keep
	// The spike measured with JIT off; JIT compilation only adds latency to
	// these short index lookups, even when large id arrays inflate the cost.
	if _, err := tx.Exec(ctx, `SET LOCAL jit = off`); err != nil {
		return fmt.Errorf("graph: session settings: %w", err)
	}
	return f(tx)
}

// args collects positional query parameters.
type args []any

func (a *args) add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}

// filter returns the SQL predicate on relationship r for o. Only fixed SQL
// fragments are concatenated; every value is a parameter.
func (o Options) filter(a *args) string {
	var parts []string
	if len(o.RelTypes) > 0 {
		types := make([]string, len(o.RelTypes))
		for i, t := range o.RelTypes {
			types[i] = string(t)
		}
		parts = append(parts, "r.rel_type = ANY("+a.add(types)+"::text[])")
	}
	switch {
	case o.At != nil:
		d := a.add(*o.At) + "::date"
		parts = append(parts, "(r.valid_from IS NULL OR r.valid_from <= "+d+")",
			"(r.closed_collection_id IS NULL OR r.valid_to_before IS NULL OR "+d+" < r.valid_to_before)")
	case !o.IncludeClosed && !o.ClosedOnly:
		parts = append(parts, "r.closed_collection_id IS NULL")
	}
	if o.ClosedOnly {
		parts = append(parts, "r.closed_collection_id IS NOT NULL")
	}
	if len(parts) == 0 {
		return "TRUE"
	}
	return strings.Join(parts, " AND ")
}

// exists returns ErrNotFound unless every id is an entity.
func exists(ctx context.Context, tx pgx.Tx, ids ...int64) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM entity WHERE id = ANY($1)`, ids).Scan(&n); err != nil {
		return fmt.Errorf("graph: find entity: %w", err)
	}
	distinct := slices.Compact(slices.Sorted(slices.Values(ids)))
	if n != len(distinct) {
		return ErrNotFound
	}
	return nil
}

// sourceState is a source's key and whether it is stale now.
type sourceState struct {
	key   string
	stale bool
}

// SourceStaleSQL returns a boolean SQL expression over a source row aliased
// s: true when the source's newest done full-scope collection (by reference
// date) is older than s.stale_after at now (an SQL expression, such as now()
// or a timestamptz parameter), or when there is none. The age counts from
// the collection's observed date for a source published as editions
// (source.editions), from its reference date otherwise: the rule of
// internal/freshness. Only full-scope collections count: a slice refreshes a
// few companies, not the source. collection holds a few rows per source.
func SourceStaleSQL(now string) string {
	return `coalesce((SELECT CASE WHEN s.editions THEN c.observed_date ELSE c.reference_date END
			FROM collection c
			WHERE c.source_id = s.id AND c.status = 'done' AND (c.scope ->> 'full')::boolean IS TRUE
			ORDER BY c.reference_date DESC, c.id DESC LIMIT 1)
		+ s.stale_after < ` + now + `, true)`
}

// loadSources reads every source (a handful of rows) with its staleness.
func loadSources(ctx context.Context, tx pgx.Tx) (map[int16]sourceState, error) {
	rows, err := tx.Query(ctx, `SELECT s.id, s.key, `+SourceStaleSQL("now()")+` FROM source s`)
	if err != nil {
		return nil, fmt.Errorf("graph: load sources: %w", err)
	}
	out := map[int16]sourceState{}
	var (
		id int16
		st sourceState
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &st.key, &st.stale}, func() error {
		out[id] = st
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("graph: load sources: %w", err)
	}
	return out, nil
}

// edgeSelect reads full edges for relationship ids. Confidences are stored
// as real; the numeric round trip returns them as written (0.99, not
// 0.9900000095).
const edgeSelect = `
	SELECT r.id, r.rel_type, r.attrs, r.kind, r.confidence::numeric::float8,
		r.valid_from, r.valid_to_after, r.valid_to_before, r.first_collection_id, c.source_id,
		se.id, se.kind, se.name, se.natural_key, de.id, de.kind, de.name, de.natural_key,
		(SELECT min(pr.identity_confidence)::numeric::float8 FROM person_record pr
			WHERE pr.id IN (r.src_record_id, r.dst_record_id)),
		r.closed_collection_id IS NOT NULL, r.src_record_id, r.dst_record_id
	FROM relationship r
	JOIN entity se ON se.id = r.src_entity_id
	JOIN entity de ON de.id = r.dst_entity_id
	JOIN collection c ON c.id = r.first_collection_id
	WHERE r.id = ANY($1)
	ORDER BY r.id`

// identityBasisSQL gives, for each person record of $1, the basis of its
// entity: how many records the entity has and whether an analyst decision
// placed any of them. One aggregate per page of edges, set-based:
// person_record_pkey for the records, person_record_entity_id for the
// entities' records (TestIdentityBasisPlanUsesIndexes).
const identityBasisSQL = `
	WITH rec AS (
		SELECT p.id, p.entity_id FROM person_record p WHERE p.id = ANY($1) AND p.entity_id IS NOT NULL
	), ent AS (
		SELECT q.entity_id, count(*) AS n, bool_or(q.locked_by_decision_id IS NOT NULL) AS analyst
		FROM person_record q WHERE q.entity_id IN (SELECT entity_id FROM rec) GROUP BY q.entity_id
	)
	SELECT rec.id, ent.n > 1, ent.analyst FROM rec JOIN ent USING (entity_id)`

// basisRank orders the bases from the least to the most inferential.
var basisRank = map[string]int{IdentityNone: 0, IdentityAuto: 1, IdentityAnalyst: 2}

// loadEdges returns the edges with these relationship ids, in id order.
func loadEdges(ctx context.Context, tx pgx.Tx, srcs map[int16]sourceState, relIDs []int64) ([]Edge, error) {
	if len(relIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, edgeSelect, relIDs)
	if err != nil {
		return nil, fmt.Errorf("graph: load edges: %w", err)
	}
	var (
		out     []Edge
		records [][2]*int64 // the person records of each edge, src and dst
		all     []int64
	)
	for rows.Next() {
		var (
			e              Edge
			sourceID       int16
			srcKey, dstKey *string
			srcRec, dstRec *int64
		)
		if err := rows.Scan(&e.RelationshipID, &e.Type, &e.Attrs, &e.Kind, &e.Confidence,
			&e.ValidFrom, &e.ValidToAfter, &e.ValidToBefore, &e.CollectionID, &sourceID,
			&e.Src.ID, &e.Src.Kind, &e.Src.Name, &srcKey, &e.Dst.ID, &e.Dst.Kind, &e.Dst.Name, &dstKey,
			&e.IdentityConfidence, &e.Closed, &srcRec, &dstRec); err != nil {
			rows.Close()
			return nil, fmt.Errorf("graph: scan edge: %w", err)
		}
		e.Src.NaturalKey, e.Dst.NaturalKey = deref(srcKey), deref(dstKey)
		st := srcs[sourceID]
		e.SourceKey, e.Stale = st.key, st.stale
		e.IdentityBasis = IdentityNone
		out = append(out, e)
		records = append(records, [2]*int64{srcRec, dstRec})
		for _, r := range []*int64{srcRec, dstRec} {
			if r != nil {
				all = append(all, *r)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graph: load edges: %w", err)
	}
	if len(all) == 0 {
		return out, nil
	}
	slices.Sort(all)
	basis, err := recordBases(ctx, tx, slices.Compact(all))
	if err != nil {
		return nil, err
	}
	for i := range out {
		for _, r := range records[i] {
			if r == nil {
				continue
			}
			if b := basis[*r]; basisRank[b] > basisRank[out[i].IdentityBasis] {
				out[i].IdentityBasis = b
			}
		}
	}
	return out, nil
}

// recordBases returns the identity basis of each record's entity (records
// without an entity are absent: IdentityNone).
func recordBases(ctx context.Context, tx pgx.Tx, recordIDs []int64) (map[int64]string, error) {
	rows, err := tx.Query(ctx, identityBasisSQL, recordIDs)
	if err != nil {
		return nil, fmt.Errorf("graph: identity basis: %w", err)
	}
	out := map[int64]string{}
	var (
		id              int64
		joined, analyst bool
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &joined, &analyst}, func() error {
		switch {
		case !joined:
			out[id] = IdentityNone
		case analyst:
			out[id] = IdentityAnalyst
		default:
			out[id] = IdentityAuto
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("graph: identity basis: %w", err)
	}
	return out, nil
}

// loadRefs returns the entities with these ids.
func loadRefs(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]EntityRef, error) {
	out := map[int64]EntityRef{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, kind, name, natural_key FROM entity WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("graph: load entities: %w", err)
	}
	var (
		r   EntityRef
		key *string
	)
	_, err = pgx.ForEachRow(rows, []any{&r.ID, &r.Kind, &r.Name, &key}, func() error {
		r.NaturalKey = deref(key)
		out[r.ID] = r
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("graph: load entities: %w", err)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// batch is the largest id array sent in one = ANY($1) query, as in the spike.
const batch = 100_000

// chunks splits ids into slices of at most batch ids.
func chunks(ids []int64) [][]int64 {
	var out [][]int64
	for len(ids) > batch {
		out = append(out, ids[:batch])
		ids = ids[batch:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

// reporter calls progress, if any, with values that never go down.
type reporter struct {
	f    func(float64)
	last float64
}

func (r *reporter) report(v float64) {
	if r.f == nil {
		return
	}
	v = min(max(v, r.last), 1)
	r.last = v
	r.f(v)
}
