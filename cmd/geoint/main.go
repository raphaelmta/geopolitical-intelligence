// Command geoint is the single binary of the platform.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"geoint/internal/db"
	"geoint/internal/sources"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: geoint <api|worker|migrate|collect|version>")
		return 2
	}
	var err error
	switch args[0] {
	case "api":
		err = serveAPI()
	case "migrate":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = migrate(ctx)
		stop()
	case "worker":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = worker(ctx)
		stop()
	case "collect":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = collect(ctx, args[1:], stdout)
		stop()
	case "version":
		fmt.Fprintln(stdout, version)
	default:
		fmt.Fprintf(stderr, "geoint: unknown command %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "geoint: %v\n", err)
		return 1
	}
	return 0
}

// migrate applies the schema migrations, upserts the catalogue of sources
// and sets the passwords of the worker and API roles (DB_WORKER_PASSWORD,
// DB_API_PASSWORD; an empty one leaves that role as it is, with a warning).
// It runs as the database owner.
func migrate(ctx context.Context) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("migrate: DATABASE_URL is not set")
	}
	if err := db.Migrate(ctx, url); err != nil {
		return err
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := sources.Sync(ctx, pool); err != nil {
		return err
	}
	passwords := map[string]string{}
	for role, env := range map[string]string{db.WorkerRole: "DB_WORKER_PASSWORD", db.APIRole: "DB_API_PASSWORD"} {
		if pw := os.Getenv(env); pw != "" {
			passwords[role] = pw
		} else {
			slog.Warn("migrate: password not set, role left unchanged (it cannot log in until it has one)",
				"role", role, "variable", env)
		}
	}
	if err := db.SetRolePasswords(ctx, pool, passwords); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
