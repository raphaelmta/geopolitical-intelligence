// Package db opens the PostgreSQL pool and applies the embedded migrations.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open returns a connection pool for url and checks that the server answers.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies all pending migrations. It is safe to call repeatedly.
//
// Numbering rule: a new migration always takes a number above the highest
// one ever applied anywhere; skipped numbers (0010, 0015, 0020) stay unused.
// goose refuses a pending migration numbered below the applied maximum.
func Migrate(ctx context.Context, url string) error {
	return migrate(ctx, url, 0)
}

// MigrateTo applies the pending migrations up to and including version
// (tests that need the schema as an earlier migration left it).
func MigrateTo(ctx context.Context, url string, version int64) error {
	return migrate(ctx, url, version)
}

// migrate applies pending migrations up to version (0: all).
func migrate(ctx context.Context, url string, version int64) error {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("db: parse url: %w", err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	defer sqlDB.Close()

	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("db: migrations fs: %w", err)
	}
	// The session lock makes concurrent migrates wait for each other.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return fmt.Errorf("db: migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("db: migration provider: %w", err)
	}
	if version > 0 {
		_, err = provider.UpTo(ctx, version)
	} else {
		_, err = provider.Up(ctx)
	}
	if err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	return nil
}
