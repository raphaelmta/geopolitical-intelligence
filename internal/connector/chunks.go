package connector

import "fmt"

// RangeChunks returns a query that cuts the keys of a staged load into at
// most chunks ranges of the key order, for loaders that write their
// relationships in one short transaction per chunk. Its rows are (key,
// bucket): one per distinct key of keys, a query with a single text column
// that lists a key once per staged row of that key (its weight).
//
// A key's bucket is floor(rows of the keys before it × chunks / all rows):
//   - every row of one key is in one bucket, and a key's bucket is never
//     lower than the bucket of a key before it, so a chunk is a short range
//     of the key order (and of every id handed out in that order);
//   - buckets hold about equal numbers of rows: a bucket's keys all start in
//     one window of total/chunks rows, so it holds at most
//     ceil(total/chunks) + (rows of its last key) − 1 rows. A key heavier
//     than a window fills its bucket alone and the buckets it spans stay
//     empty; keys without rows do not exist here and cost nothing;
//   - it is a function of the staged multiset of keys only (not of the row
//     order or of earlier loads): the same staging always gives the same
//     chunks.
//
// Bucket numbers are in [0, chunks) and need not be contiguous.
func RangeChunks(keys string, chunks int) string {
	// Integer arithmetic (bigint: rows × chunks stays far below 2^63), so the
	// division floors.
	return fmt.Sprintf(`SELECT k.key, (((sum(count(*)) OVER (ORDER BY k.key))::bigint - count(*)) * %d
			/ (sum(count(*)) OVER ())::bigint)::int AS bucket
		FROM (%s) k(key)
		GROUP BY k.key`, chunks, keys)
}
