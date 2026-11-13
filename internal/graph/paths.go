package graph

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Defaults and bounds of Paths.
const (
	DefaultMaxDepth  = 6
	DefaultHubDegree = 500
	DefaultMaxPaths  = 20
	// defaultMaxVisited bounds the memory of one search: entities held by
	// both sides plus the links of the level being read. A search that
	// passes it stops at once and reports Truncated with no paths.
	defaultMaxVisited = 2_000_000
)

func (o PathOptions) withDefaults() (PathOptions, error) {
	if o.MaxDepth < 0 || o.HubDegree < 0 || o.MaxPaths < 0 {
		return o, fmt.Errorf("%w: negative path option", ErrBadOptions)
	}
	if o.MaxDepth == 0 || o.MaxDepth > DefaultMaxDepth {
		o.MaxDepth = DefaultMaxDepth
	}
	if o.HubDegree == 0 {
		o.HubDegree = DefaultHubDegree
	}
	if o.MaxPaths == 0 {
		o.MaxPaths = DefaultMaxPaths
	}
	return o, nil
}

// Paths returns every shortest path between a and b (up to MaxPaths) of at
// most MaxDepth hops, treating edges as undirected. It is a bidirectional
// breadth-first search, the strategy the spike chose (PATHS_STRATEGY = bfs):
// each step expands one whole level of the smaller frontier with batched
// = ANY($1) queries. An intermediate entity whose degree (edges passing o)
// exceeds HubDegree is a hub: it is neither expanded nor a meeting point,
// and is listed in StoppedHubs when it blocked an expansion or a meeting.
// Degrees are checked lazily, just before expanding a frontier and at a
// meeting, so the last (largest) level needs none. a and b are always
// expanded. A path is a sequence of entities: between two entities
// joined by several edges it uses the one with the lowest relationship id.
// a == b gives an empty result. progress, when set, gets increasing values
// ending at 1.
func (g *Graph) Paths(ctx context.Context, a, b int64, o PathOptions, progress func(float64)) (PathsResult, error) {
	o, err := o.withDefaults()
	if err != nil {
		return PathsResult{}, err
	}
	var res PathsResult
	rep := &reporter{f: progress}
	err = g.read(ctx, func(tx pgx.Tx) error {
		if err := exists(ctx, tx, a, b); err != nil {
			return err
		}
		if a == b {
			return nil
		}
		s := &pathSearch{tx: tx, o: o, maxVisited: g.maxVisited, hubs: map[int64]int{},
			checked: map[int64]bool{a: true, b: true}}
		steps, truncated, err := s.run(ctx, newSide(a), newSide(b), rep)
		if err != nil {
			return err
		}
		res, err = s.result(ctx, steps)
		res.Truncated = res.Truncated || truncated
		return err
	})
	if err != nil {
		return PathsResult{}, err
	}
	rep.report(1)
	return res, nil
}

// step is one hop of a path, walked from one entity to the next over rel.
type step struct{ from, to, rel int64 }

// side is one direction of the bidirectional search.
type side struct {
	root     int64
	depth    int
	dist     map[int64]int
	parents  map[int64][]step // steps into each entity from the previous level, by from
	frontier []int64          // entities at distance depth, sorted
}

func newSide(root int64) *side {
	return &side{root: root, dist: map[int64]int{root: 0}, parents: map[int64][]step{}, frontier: []int64{root}}
}

type pathSearch struct {
	tx         pgx.Tx
	o          PathOptions
	maxVisited int
	hubs       map[int64]int  // stopped hubs and their real degree
	checked    map[int64]bool // entities whose degree was checked (endpoints included)
}

// errVisitCap stops reading a level when the search passes maxVisited.
var errVisitCap = errors.New("graph: path search visited too many entities")

// run searches level by level until the sides meet, one runs out, or the
// depth is spent. It returns the shortest paths as steps. truncated with no
// paths: the search passed maxVisited (partial results would not be the
// shortest paths, so none are returned).
func (s *pathSearch) run(ctx context.Context, sa, sb *side, rep *reporter) (paths [][]step, truncated bool, err error) {
	for sa.depth+sb.depth < s.o.MaxDepth && len(sa.frontier) > 0 && len(sb.frontier) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		x, y := sa, sb
		if len(sb.frontier) < len(sa.frontier) {
			x, y = sb, sa
		}
		err := s.expand(ctx, x, y)
		if errors.Is(err, errVisitCap) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		rep.report(float64(sa.depth+sb.depth) / float64(s.o.MaxDepth))
		meets, err := s.meets(ctx, x, y)
		if err != nil {
			return nil, false, err
		}
		if len(meets) > 0 {
			paths, truncated := s.join(sa, sb, meets)
			return paths, truncated, nil
		}
	}
	return nil, false, nil
}

// meets returns the entities of x's new level that y reached, leaving out
// hubs: a hub is never a meeting point. A hub found here is dropped from
// both sides.
func (s *pathSearch) meets(ctx context.Context, x, y *side) ([]int64, error) {
	var cand []int64
	for _, v := range x.frontier {
		if _, ok := y.dist[v]; ok {
			cand = append(cand, v)
		}
	}
	hubs, err := s.stopHubs(ctx, cand, x, y)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(cand, func(v int64) bool { _, hub := hubs[v]; return hub }), nil
}

// stopHubs checks the entities of ids not checked yet, records the hubs
// among them and removes them from both sides. Endpoints are never hubs.
func (s *pathSearch) stopHubs(ctx context.Context, ids []int64, sides ...*side) (map[int64]int, error) {
	var unchecked []int64
	for _, v := range ids {
		if !s.checked[v] {
			unchecked = append(unchecked, v)
		}
	}
	hubs, err := s.findHubs(ctx, unchecked)
	if err != nil {
		return nil, err
	}
	for _, v := range unchecked {
		s.checked[v] = true
	}
	for v, deg := range hubs {
		s.hubs[v] = deg
		for _, sd := range sides {
			if _, ok := sd.dist[v]; ok {
				delete(sd.dist, v)
				delete(sd.parents, v)
				sd.frontier = slices.DeleteFunc(sd.frontier, func(u int64) bool { return u == v })
			}
		}
	}
	return hubs, nil
}

// expand moves x one level out. Hubs in x's frontier are checked just
// before, recorded and not expanded (the endpoints are always expanded).
// It returns errVisitCap, without finishing the level, when the entities
// held by both sides pass maxVisited.
func (s *pathSearch) expand(ctx context.Context, x, y *side) error {
	if _, err := s.stopHubs(ctx, x.frontier, x, y); err != nil {
		return err
	}
	inFrontier := make(map[int64]bool, len(x.frontier))
	for _, v := range x.frontier {
		inFrontier[v] = true
	}
	next := map[int64]map[int64]int64{} // entity -> previous entity -> lowest relationship id
	links := 0                          // entries in next, the memory this level holds
	consider := func(u, v, rel int64) {
		if !inFrontier[u] {
			return
		}
		if _, seen := x.dist[v]; seen {
			return
		}
		if _, hub := s.hubs[v]; hub {
			return
		}
		prev := next[v]
		if prev == nil {
			prev = map[int64]int64{}
			next[v] = prev
		}
		old, ok := prev[u]
		if !ok {
			links++
		}
		if !ok || rel < old {
			prev[u] = rel
		}
	}
	for _, chunk := range chunks(x.frontier) {
		a := args{chunk}
		f := s.o.filter(&a)
		rows, err := s.tx.Query(ctx, `
			SELECT r.id, r.src_entity_id, r.dst_entity_id FROM relationship r WHERE r.src_entity_id = ANY($1) AND `+f+`
			UNION ALL
			SELECT r.id, r.src_entity_id, r.dst_entity_id FROM relationship r WHERE r.dst_entity_id = ANY($1) AND `+f, a...)
		if err != nil {
			return fmt.Errorf("graph: expand: %w", err)
		}
		var rel, src, dst int64
		_, err = pgx.ForEachRow(rows, []any{&rel, &src, &dst}, func() error {
			consider(src, dst, rel)
			consider(dst, src, rel)
			if len(x.dist)+len(y.dist)+links > s.maxVisited {
				return errVisitCap
			}
			return nil
		})
		if errors.Is(err, errVisitCap) {
			return err
		}
		if err != nil {
			return fmt.Errorf("graph: expand: %w", err)
		}
	}

	x.depth++
	x.frontier = slices.Sorted(maps.Keys(next))
	for _, v := range x.frontier {
		x.dist[v] = x.depth
		prevs := slices.Sorted(maps.Keys(next[v]))
		steps := make([]step, len(prevs))
		for i, u := range prevs {
			steps[i] = step{from: u, to: v, rel: next[v][u]}
		}
		x.parents[v] = steps
	}
	return nil
}

// findHubs returns the entities among ids whose degree exceeds HubDegree,
// with their real degree. The first pass counts at most HubDegree+1 edges per
// direction; only the hubs are then counted in full.
func (s *pathSearch) findHubs(ctx context.Context, ids []int64) (map[int64]int, error) {
	hubs := map[int64]int{}
	for _, chunk := range chunks(ids) {
		a := args{chunk, s.o.HubDegree + 1, s.o.HubDegree}
		f := s.o.filter(&a)
		rows, err := s.tx.Query(ctx, `
			SELECT n.id FROM unnest($1::bigint[]) AS n(id)
			WHERE (SELECT count(*) FROM (SELECT 1 FROM relationship r
					WHERE r.src_entity_id = n.id AND `+f+` LIMIT $2) s)
				+ (SELECT count(*) FROM (SELECT 1 FROM relationship r
					WHERE r.dst_entity_id = n.id AND r.src_entity_id <> n.id AND `+f+` LIMIT $2) d) > $3`, a...)
		if err != nil {
			return nil, fmt.Errorf("graph: degree: %w", err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return nil, fmt.Errorf("graph: degree: %w", err)
		}
		for _, v := range found {
			hubs[v] = 0
		}
	}
	if len(hubs) == 0 {
		return hubs, nil
	}
	a := args{slices.Collect(maps.Keys(hubs))}
	f := s.o.filter(&a)
	rows, err := s.tx.Query(ctx, `
		SELECT n.id,
			(SELECT count(*) FROM relationship r WHERE r.src_entity_id = n.id AND `+f+`)
			+ (SELECT count(*) FROM relationship r WHERE r.dst_entity_id = n.id AND r.src_entity_id <> n.id AND `+f+`)
		FROM unnest($1::bigint[]) AS n(id)`, a...)
	if err != nil {
		return nil, fmt.Errorf("graph: hub degree: %w", err)
	}
	var (
		id  int64
		deg int
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &deg}, func() error {
		hubs[id] = deg
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("graph: hub degree: %w", err)
	}
	return hubs, nil
}

// chains calls emit with each shortest chain of steps from sd.root to v,
// root first, until emit returns false.
func (sd *side) chains(v int64, emit func([]step) bool) bool {
	if v == sd.root {
		return emit(nil)
	}
	for _, st := range sd.parents[v] {
		if !sd.chains(st.from, func(c []step) bool { return emit(append(slices.Clip(c), st)) }) {
			return false
		}
	}
	return true
}

// join builds the paths a→m→b through each meeting entity m, at most
// MaxPaths of them; truncated when there were more.
func (s *pathSearch) join(sa, sb *side, meets []int64) ([][]step, bool) {
	limit := s.o.MaxPaths + 1
	var paths [][]step
	for _, m := range meets {
		var tails [][]step // m → b
		sb.chains(m, func(c []step) bool {
			tail := make([]step, len(c))
			for i, st := range c {
				tail[len(c)-1-i] = step{from: st.to, to: st.from, rel: st.rel}
			}
			tails = append(tails, tail)
			return len(tails) < limit
		})
		sa.chains(m, func(head []step) bool {
			for _, tail := range tails {
				paths = append(paths, append(slices.Clip(head), tail...))
				if len(paths) == limit {
					return false
				}
			}
			return true
		})
		if len(paths) == limit {
			return paths[:s.o.MaxPaths], true
		}
	}
	return paths, false
}

// result loads the edges of the paths and the stopped hubs.
func (s *pathSearch) result(ctx context.Context, paths [][]step) (PathsResult, error) {
	var res PathsResult
	srcs, err := loadSources(ctx, s.tx)
	if err != nil {
		return res, err
	}
	var relIDs []int64
	for _, p := range paths {
		for _, st := range p {
			relIDs = append(relIDs, st.rel)
		}
	}
	slices.Sort(relIDs)
	edges, err := loadEdges(ctx, s.tx, srcs, slices.Compact(relIDs))
	if err != nil {
		return res, err
	}
	byID := make(map[int64]Edge, len(edges))
	for _, e := range edges {
		byID[e.RelationshipID] = e
	}
	for _, p := range paths {
		hops := make([]Hop, len(p))
		for i, st := range p {
			e := byID[st.rel]
			hops[i] = Hop{Edge: e, Forward: e.Src.ID == st.from}
		}
		res.Paths = append(res.Paths, hops)
	}
	hubIDs := slices.Sorted(maps.Keys(s.hubs))
	refs, err := loadRefs(ctx, s.tx, hubIDs)
	if err != nil {
		return res, err
	}
	for _, id := range hubIDs {
		res.StoppedHubs = append(res.StoppedHubs, HubStop{Entity: refs[id], Degree: s.hubs[id]})
	}
	return res, nil
}
