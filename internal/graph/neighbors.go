package graph

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Page sizes of Neighbors.
const (
	DefaultLimit = 200
	MaxLimit     = 1000
)

// Neighbors returns a page of the edges touching entity id that pass o, in
// relationship id order. limit 0 means DefaultLimit; larger than MaxLimit is
// clamped. cursor is "" for the first page, else the next value of the
// previous page; next is "" on the last page. An unknown id gives ErrNotFound
// and a cursor that was not returned by Neighbors gives ErrBadCursor.
func (g *Graph) Neighbors(ctx context.Context, id int64, o Options, cursor string, limit int) (edges []Edge, next string, err error) {
	switch {
	case limit < 0:
		return nil, "", fmt.Errorf("%w: negative limit", ErrBadOptions)
	case limit == 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		limit = MaxLimit
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	err = g.read(ctx, func(tx pgx.Tx) error {
		if err := exists(ctx, tx, id); err != nil {
			return err
		}
		srcs, err := loadSources(ctx, tx)
		if err != nil {
			return err
		}
		ids, err := neighborPage(ctx, tx, id, o, after, limit+1)
		if err != nil {
			return err
		}
		if len(ids) > limit {
			ids = ids[:limit]
			next = encodeCursor(ids[len(ids)-1])
		}
		edges, err = loadEdges(ctx, tx, srcs, ids)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return edges, next, nil
}

// neighborPage returns up to n relationship ids after the cursor, in order.
// Each direction is an ordered range of (entity, id); a self-loop is read
// from the source side only.
func neighborPage(ctx context.Context, tx pgx.Tx, id int64, o Options, after int64, n int) ([]int64, error) {
	a := args{id, after, n}
	f := o.filter(&a)
	rows, err := tx.Query(ctx, `
		SELECT id FROM (
			(SELECT r.id FROM relationship r
			 WHERE r.src_entity_id = $1 AND r.id > $2 AND `+f+` ORDER BY r.id LIMIT $3)
			UNION ALL
			(SELECT r.id FROM relationship r
			 WHERE r.dst_entity_id = $1 AND r.src_entity_id <> $1 AND r.id > $2 AND `+f+` ORDER BY r.id LIMIT $3)
		) u ORDER BY id LIMIT $3`, a...)
	if err != nil {
		return nil, fmt.Errorf("graph: neighbors: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("graph: neighbors: %w", err)
	}
	return ids, nil
}

// The cursor is the last relationship id of the page, base64 of its decimal form.
func encodeCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func decodeCursor(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, ErrBadCursor
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrBadCursor
	}
	return id, nil
}
