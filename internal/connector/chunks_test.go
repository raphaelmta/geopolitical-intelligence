package connector_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/connector"
)

type keyBucket struct {
	Key    string
	Bucket int
}

// rangeChunks runs connector.RangeChunks over keys and returns its rows in
// key order (the database's order, the one the loaders use).
func rangeChunks(t *testing.T, pool *pgxpool.Pool, keys string, chunks int) []keyBucket {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT key, bucket FROM (`+connector.RangeChunks(keys, chunks)+`) c ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[keyBucket])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// checkRangeChunks checks the assignment of keys (one row per staged row,
// key as its only column) to at most chunks buckets and returns the size of
// each bucket in rows.
func checkRangeChunks(t *testing.T, pool *pgxpool.Pool, keys string, chunks int) map[int]int {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT key, count(*)::int FROM (`+keys+`) k(key) GROUP BY key`)
	if err != nil {
		t.Fatal(err)
	}
	weight, err := pgx.CollectRows(rows, pgx.RowToStructByPos[keyBucket]) // Bucket holds the weight
	if err != nil {
		t.Fatal(err)
	}
	w := map[string]int{}
	total := 0
	for _, r := range weight {
		w[r.Key] = r.Bucket
		total += r.Bucket
	}

	got := rangeChunks(t, pool, keys, chunks)
	// Every staged key in exactly one bucket.
	if len(got) != len(w) {
		t.Fatalf("%d keys assigned, want %d (one row per distinct key)", len(got), len(w))
	}
	seen := map[string]bool{}
	size := map[int]int{}
	last := map[int]string{}
	for i, r := range got {
		if seen[r.Key] {
			t.Fatalf("key %q assigned twice", r.Key)
		}
		seen[r.Key] = true
		if _, ok := w[r.Key]; !ok {
			t.Fatalf("key %q is not staged", r.Key)
		}
		if r.Bucket < 0 || r.Bucket >= chunks {
			t.Fatalf("key %q: bucket %d out of [0, %d)", r.Key, r.Bucket, chunks)
		}
		// Ranges: buckets never decrease along the key order.
		if i > 0 && r.Bucket < got[i-1].Bucket {
			t.Fatalf("key %q: bucket %d after bucket %d of %q", r.Key, r.Bucket, got[i-1].Bucket, got[i-1].Key)
		}
		size[r.Bucket] += w[r.Key]
		last[r.Bucket] = r.Key
	}
	// Bounded: a window of the row order plus the overhang of its last key.
	window := (total + chunks - 1) / chunks
	sum := 0
	for b, n := range size {
		sum += n
		if limit := window + w[last[b]] - 1; n > limit {
			t.Errorf("bucket %d: %d rows, want at most %d (window %d, last key %d rows)", b, n, limit, window, w[last[b]])
		}
	}
	if sum != total {
		t.Fatalf("buckets hold %d rows, staging %d", sum, total)
	}
	// Stable: the same staged rows in another order give the same chunks.
	shuffled := `SELECT * FROM (` + keys + `) s ORDER BY random()`
	for range 2 {
		again := rangeChunks(t, pool, shuffled, chunks)
		if fmt.Sprint(again) != fmt.Sprint(got) {
			t.Fatal("the same staged keys in another order gave other chunks")
		}
	}
	return size
}

func TestRangeChunks(t *testing.T) {
	pool := setup(t)

	t.Run("one key with very many rows among many light keys", func(t *testing.T) {
		// 5,000 keys of one row and key k02500 with 20,000 more: 25,000 rows,
		// windows of 391 rows over 64 chunks.
		keys := `SELECT 'k' || lpad(i::text, 5, '0') FROM generate_series(0, 4999) i
			UNION ALL SELECT 'k02500' FROM generate_series(1, 20000)`
		size := checkRangeChunks(t, pool, keys, 64)
		// The heavy key fills its bucket alone (with the light keys of its
		// window before it); the buckets after it are light again.
		big := 0
		for _, n := range size {
			if n > 1000 {
				big++
			} else if n > 391 {
				t.Errorf("a light bucket holds %d rows, want at most 391", n)
			}
		}
		if big != 1 {
			t.Errorf("%d heavy buckets, want 1 (the heavy key's)", big)
		}
		if len(size) < 64-52 {
			t.Errorf("%d buckets, want the light keys spread over the windows the heavy key leaves", len(size))
		}
	})

	t.Run("uneven weights spread evenly", func(t *testing.T) {
		// 20,000 keys of 1 to 7 rows: every bucket within one key of the window.
		keys := `SELECT 'c' || lpad(i::text, 6, '0') FROM generate_series(0, 19999) i, generate_series(1, i % 7 + 1)`
		size := checkRangeChunks(t, pool, keys, 256)
		if len(size) != 256 {
			t.Errorf("%d buckets, want 256", len(size))
		}
		total := 0
		for _, n := range size {
			total += n
		}
		low := total/256 - 7
		for b, n := range size {
			if n < low {
				t.Errorf("bucket %d: %d rows, want at least %d", b, n, low)
			}
		}
	})

	t.Run("fewer keys than chunks", func(t *testing.T) {
		size := checkRangeChunks(t, pool, `SELECT 'k' || i FROM generate_series(0, 9) i, generate_series(1, 3)`, 1024)
		if len(size) != 10 {
			t.Errorf("%d buckets, want one per key (10)", len(size))
		}
	})

	t.Run("empty staging", func(t *testing.T) {
		if got := rangeChunks(t, pool, `SELECT 'k' WHERE false`, 1024); len(got) != 0 {
			t.Errorf("%d buckets for no rows", len(got))
		}
	})
}
