// Package testutil provides helpers for tests that need real services.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/db"
	"geoint/internal/sources"
)

// EmptySchemaURL creates a fresh, empty schema on the server named by
// TEST_DATABASE_URL and returns a URL whose search_path points to it. The
// schema is dropped when the test ends, so tests can run in parallel
// packages against one server.
func EmptySchemaURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("TEST_DATABASE_URL is not set; run the tests through `make test`")
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	schema := "t_" + hex.EncodeToString(buf)

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// DB returns a pool on a new, migrated schema that is dropped on Cleanup.
func DB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	u := EmptySchemaURL(t)
	if err := db.Migrate(ctx, u); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := db.Open(ctx, u)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// SeedSources inserts the rows of sources.All into the schema behind pool.
func SeedSources(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := sources.Sync(context.Background(), pool); err != nil {
		t.Fatalf("seed sources: %v", err)
	}
}
