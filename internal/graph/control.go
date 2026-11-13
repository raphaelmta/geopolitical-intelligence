package graph

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"geoint/internal/claims"
)

const (
	// DefaultMaxPartners is the MaxPartners of ControlOptions when zero.
	DefaultMaxPartners = 50
	// defaultMaxMembers bounds a control group: past it the search stops with
	// a too_large Stop on the root. The climb is checked against the entities
	// climbed, the descent against the members; either may overshoot by the
	// level being read when the bound is crossed.
	defaultMaxMembers = 50_000
	// fundLegalNature is the Receita legal nature of investment funds
	// ("Clube/Fundo de Investimento", docs/sources/receita.md).
	fundLegalNature = "2224"
)

func (o ControlOptions) withDefaults() (ControlOptions, error) {
	if o.Mode != Documented && o.Mode != CorporateLinks {
		return o, fmt.Errorf("%w: control mode %q", ErrBadOptions, o.Mode)
	}
	if o.MaxPartners < 0 {
		return o, fmt.Errorf("%w: negative MaxPartners", ErrBadOptions)
	}
	if o.MaxPartners == 0 {
		o.MaxPartners = DefaultMaxPartners
	}
	return o, nil
}

// ControlGroup returns the control group of a company over open edges. It
// climbs from the company through its controllers until none is left (the
// final controllers), then descends from each final controller through the
// entities they control. Which edges carry control depends on o.Mode. Both
// ways the search stops at an investment fund, a foreign entity or a company
// with more than MaxPartners open partners, listing it in Stops; the root
// itself is always expanded. Cycles are cut: an entity is visited once.
// Entities climbed through that no final controller reaches (a cycle at the
// top) seed the descent as members of depth 0, farthest from the root first,
// so depths and Via chains lead down to the root. When the size bound
// (too_large) was hit while climbing, the result is the root alone: no
// controllers, the root as the only member, no edges, and the too_large stop
// on the root as the only stop. A root that is not a company gives ErrBadOptions. progress, when set, gets
// increasing values ending at 1.
func (g *Graph) ControlGroup(ctx context.Context, companyID int64, o ControlOptions, progress func(float64)) (ControlResult, error) {
	o, err := o.withDefaults()
	if err != nil {
		return ControlResult{}, err
	}
	var res ControlResult
	rep := &reporter{f: progress}
	err = g.read(ctx, func(tx pgx.Tx) error {
		if err := exists(ctx, tx, companyID); err != nil {
			return err
		}
		c := &controlSearch{tx: tx, o: o, root: companyID, maxMembers: g.maxMembers, info: map[int64]nodeInfo{},
			stops: map[int64]string{}, edgeIDs: map[int64]bool{}, members: map[int64]Member{}}
		if err := c.load(ctx, []int64{companyID}); err != nil {
			return err
		}
		if k := c.info[companyID].ref.Kind; k != claims.KindCompany {
			return fmt.Errorf("%w: control group root %d is a %s, not a company", ErrBadOptions, companyID, k)
		}
		tops, climbed, err := c.climb(ctx)
		if err != nil {
			return err
		}
		rep.report(0.5)
		if c.tooLarge {
			// The climb was abandoned: keep nothing of it, so no edge or
			// stop points at an entity the result does not list.
			tops = nil
			clear(c.edgeIDs)
			clear(c.stops)
			c.members[companyID] = Member{Entity: c.info[companyID].ref}
		} else if err := c.descendAll(ctx, tops, climbed); err != nil {
			return err
		}
		rep.report(0.9)
		res, err = c.result(ctx, tops)
		return err
	})
	if err != nil {
		return ControlResult{}, err
	}
	rep.report(1)
	return res, nil
}

// nodeInfo is what the stop rules need to know about an entity.
type nodeInfo struct {
	ref         EntityRef
	legalNature string // of the open company version; "" when none
	partners    int    // distinct entities with an open partner_of into it
}

type controlEdge struct {
	id, src, dst int64
	typ          claims.RelType
	attrs        map[string]any
}

type controlSearch struct {
	tx         pgx.Tx
	o          ControlOptions
	root       int64
	info       map[int64]nodeInfo
	stops      map[int64]string
	maxMembers int
	edgeIDs    map[int64]bool
	members    map[int64]Member
	tooLarge   bool
}

// load reads the entities of ids not loaded yet.
func (c *controlSearch) load(ctx context.Context, ids []int64) error {
	var missing []int64
	for _, id := range ids {
		if _, ok := c.info[id]; !ok {
			missing = append(missing, id)
		}
	}
	slices.Sort(missing)
	for _, chunk := range chunks(slices.Compact(missing)) {
		rows, err := c.tx.Query(ctx, `
			SELECT e.id, e.kind, e.name, e.natural_key, coalesce(cv.legal_nature, ''),
				(SELECT count(DISTINCT r.src_entity_id) FROM relationship r
				 WHERE r.dst_entity_id = e.id AND r.rel_type = 'partner_of' AND r.closed_collection_id IS NULL)
			FROM entity e
			LEFT JOIN company_version cv ON cv.entity_id = e.id AND cv.closed_collection_id IS NULL
			WHERE e.id = ANY($1)`, chunk)
		if err != nil {
			return fmt.Errorf("graph: control entities: %w", err)
		}
		var (
			n   nodeInfo
			key *string
		)
		_, err = pgx.ForEachRow(rows, []any{&n.ref.ID, &n.ref.Kind, &n.ref.Name, &key, &n.legalNature, &n.partners}, func() error {
			n.ref.NaturalKey = deref(key)
			c.info[n.ref.ID] = n
			return nil
		})
		if err != nil {
			return fmt.Errorf("graph: control entities: %w", err)
		}
	}
	return nil
}

// stopReason says why the search must not go through id ("" when it may).
func (c *controlSearch) stopReason(id int64) string {
	if id == c.root {
		return ""
	}
	n := c.info[id]
	switch {
	case n.ref.Kind == claims.KindForeign:
		return StopForeign
	case n.ref.Kind == claims.KindCompany && n.legalNature == fundLegalNature:
		return StopFund
	case n.partners > c.o.MaxPartners:
		return StopTooManyPartners
	}
	return ""
}

// controlEdges returns the open control edges into (up) or out of the
// entities ids that carry control in this mode, in id order, with the
// entities at their other end loaded.
func (c *controlSearch) controlEdges(ctx context.Context, ids []int64, up bool) ([]controlEdge, error) {
	// corporate_links is a superset of documented: it follows controls too.
	types := []string{string(claims.PartnerOf), string(claims.ShareholderOf), string(claims.Controls)}
	query := `SELECT r.id, r.src_entity_id, r.dst_entity_id, r.rel_type, r.attrs FROM relationship r
		WHERE r.src_entity_id = ANY($1) AND r.rel_type = ANY($2) AND r.closed_collection_id IS NULL ORDER BY r.id`
	if up {
		query = `SELECT r.id, r.src_entity_id, r.dst_entity_id, r.rel_type, r.attrs FROM relationship r
		WHERE r.dst_entity_id = ANY($1) AND r.rel_type = ANY($2) AND r.closed_collection_id IS NULL ORDER BY r.id`
	}
	var all []controlEdge
	for _, chunk := range chunks(ids) {
		rows, err := c.tx.Query(ctx, query, chunk, types)
		if err != nil {
			return nil, fmt.Errorf("graph: control edges: %w", err)
		}
		var e controlEdge
		_, err = pgx.ForEachRow(rows, []any{&e.id, &e.src, &e.dst, &e.typ, &e.attrs}, func() error {
			all = append(all, e)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("graph: control edges: %w", err)
		}
	}
	slices.SortFunc(all, func(a, b controlEdge) int { return cmp.Compare(a.id, b.id) })
	ends := make([]int64, 0, 2*len(all))
	for _, e := range all {
		ends = append(ends, e.src, e.dst)
	}
	if err := c.load(ctx, ends); err != nil {
		return nil, err
	}
	out := all[:0]
	for _, e := range all {
		if c.carriesControl(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

// carriesControl applies the mode's rule to one edge.
func (c *controlSearch) carriesControl(e controlEdge) bool {
	if c.o.Mode == CorporateLinks {
		return true
	}
	switch e.typ {
	case claims.Controls:
		return true
	case claims.ShareholderOf:
		p, ok := percent(e.attrs)
		return ok && p > 50
	case claims.PartnerOf:
		return c.info[e.dst].partners == 1
	}
	return false
}

// percent reads attrs.percent: a JSON number or a numeric string (decimal
// point or comma). Anything else is no percentage.
func percent(attrs map[string]any) (float64, bool) {
	var f float64
	switch v := attrs["percent"].(type) {
	case float64:
		f = v
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.Replace(strings.TrimSpace(v), ",", ".", 1), 64); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// full records whether a group of this size is past the bound and reports it.
func (c *controlSearch) full(size int) bool {
	if size > c.maxMembers {
		c.tooLarge = true
	}
	return c.tooLarge
}

// climb walks up from the root. It returns the final controllers (climbed
// entities with no controller the search may pass) and every climbed entity
// in climb order, the root first.
func (c *controlSearch) climb(ctx context.Context) (tops, order []int64, err error) {
	climbed := map[int64]bool{c.root: true}
	order = []int64{c.root}
	controlled := map[int64]bool{}
	frontier := []int64{c.root}
	for len(frontier) > 0 && !c.full(len(climbed)) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		edges, err := c.controlEdges(ctx, frontier, true)
		if err != nil {
			return nil, nil, err
		}
		var next []int64
		for _, e := range edges {
			c.edgeIDs[e.id] = true
			if r := c.stopReason(e.src); r != "" {
				c.stops[e.src] = r
				continue
			}
			controlled[e.dst] = true
			if !climbed[e.src] {
				climbed[e.src] = true
				next = append(next, e.src)
			}
		}
		slices.Sort(next)
		order = append(order, next...)
		frontier = next
	}
	// The bound also holds when the last level read crossed it (the loop
	// condition is not evaluated once the frontier is empty).
	c.full(len(climbed))
	for _, id := range order {
		if !controlled[id] {
			tops = append(tops, id)
		}
	}
	slices.Sort(tops)
	return tops, order, nil
}

// descendAll descends from the final controllers, then from climbed
// entities no controller reached (members of depth 0), farthest first.
func (c *controlSearch) descendAll(ctx context.Context, tops, climbed []int64) error {
	reached := map[int64][]int64{} // entity -> relationship ids from its final controller
	for _, t := range tops {
		reached[t] = nil
		if t == c.root {
			c.members[t] = Member{Entity: c.info[t].ref}
		}
	}
	if err := c.descend(ctx, tops, reached); err != nil {
		return err
	}
	for _, id := range slices.Backward(climbed) {
		if _, ok := reached[id]; ok || c.tooLarge {
			continue
		}
		reached[id] = nil
		c.members[id] = Member{Entity: c.info[id].ref}
		if err := c.descend(ctx, []int64{id}, reached); err != nil {
			return err
		}
	}
	return nil
}

// descend walks down level by level from level, adding members.
func (c *controlSearch) descend(ctx context.Context, level []int64, reached map[int64][]int64) error {
	for len(level) > 0 && !c.full(len(c.members)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		edges, err := c.controlEdges(ctx, level, false)
		if err != nil {
			return err
		}
		var next []int64
		for _, e := range edges {
			c.edgeIDs[e.id] = true
			if r := c.stopReason(e.dst); r != "" {
				c.stops[e.dst] = r
				continue
			}
			if _, ok := reached[e.dst]; ok {
				continue
			}
			via := append(slices.Clip(reached[e.src]), e.id)
			reached[e.dst] = via
			c.members[e.dst] = Member{Entity: c.info[e.dst].ref, Depth: len(via), Via: via}
			next = append(next, e.dst)
		}
		slices.Sort(next)
		level = next
	}
	return nil
}

// result assembles the group: administrator edges (corporate_links), the
// edges used, controllers, members and stops.
func (c *controlSearch) result(ctx context.Context, tops []int64) (ControlResult, error) {
	res := ControlResult{Root: c.info[c.root].ref}
	if c.o.Mode == CorporateLinks && !(c.tooLarge && len(tops) == 0) {
		group := slices.Collect(maps.Keys(c.members))
		group = append(group, tops...)
		for _, chunk := range chunks(group) {
			rows, err := c.tx.Query(ctx, `SELECT r.id FROM relationship r WHERE r.dst_entity_id = ANY($1)
				AND r.rel_type = $2 AND r.closed_collection_id IS NULL`, chunk, string(claims.AdministratorOf))
			if err != nil {
				return res, fmt.Errorf("graph: administrators: %w", err)
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil {
				return res, fmt.Errorf("graph: administrators: %w", err)
			}
			for _, id := range ids {
				c.edgeIDs[id] = true
			}
		}
	}
	srcs, err := loadSources(ctx, c.tx)
	if err != nil {
		return res, err
	}
	if res.Edges, err = loadEdges(ctx, c.tx, srcs, slices.Sorted(maps.Keys(c.edgeIDs))); err != nil {
		return res, err
	}
	for _, t := range tops {
		if t != c.root {
			res.Controllers = append(res.Controllers, c.info[t].ref)
		}
	}
	res.Members = slices.SortedFunc(maps.Values(c.members), func(a, b Member) int {
		return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.Entity.ID, b.Entity.ID))
	})
	for _, id := range slices.Sorted(maps.Keys(c.stops)) {
		res.Stops = append(res.Stops, Stop{Entity: c.info[id].ref, Reason: c.stops[id]})
	}
	if c.tooLarge {
		res.Stops = append(res.Stops, Stop{Entity: res.Root, Reason: StopTooLarge})
	}
	return res, nil
}
