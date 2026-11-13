package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/db"
	"geoint/internal/freshness"
	"geoint/internal/queue"
	"geoint/internal/sources"
)

const collectUsage = "usage: geoint collect <source> [--cnpjs a,b --depth N] [--ref YYYY-MM-DD]\n" +
	"  --ref: the newest date to collect (default today, UTC; never in the future). For a source\n" +
	"         published as editions it pins the edition: --ref 2026-10-31 loads Receita edition\n" +
	"         2026-10 even when 2026-11 is already published."

const (
	maxDepth = 6
	maxCNPJs = 1000
	// collectTimeout bounds connecting to the queue and publishing.
	collectTimeout = 30 * time.Second
)

var nonDigits = regexp.MustCompile(`\D`)

// buildRequest validates the arguments of `geoint collect` (source first,
// then flags) and returns the request to queue.
func buildRequest(args []string, now time.Time) (connector.Request, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return connector.Request{}, errors.New(collectUsage)
	}
	key := args[0]
	src, ok := sources.ByKey(key)
	if !ok {
		return connector.Request{}, fmt.Errorf("unknown source %q", key)
	}
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cnpjs := fs.String("cnpjs", "", "comma-separated CNPJs (8 or 14 digits)")
	depth := fs.Int("depth", 0, "how many ownership levels to follow")
	refStr := fs.String("ref", "", "reference date, YYYY-MM-DD (default: today, UTC)")
	if err := fs.Parse(args[1:]); err != nil {
		return connector.Request{}, fmt.Errorf("%w\n%s", err, collectUsage)
	}
	if fs.NArg() > 0 {
		return connector.Request{}, fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), collectUsage)
	}
	if *depth < 0 || *depth > maxDepth {
		return connector.Request{}, fmt.Errorf("--depth must be between 0 and %d", maxDepth)
	}

	today := now.UTC().Truncate(24 * time.Hour)
	ref := today
	if *refStr != "" {
		var err error
		if ref, err = time.Parse("2006-01-02", *refStr); err != nil {
			return connector.Request{}, fmt.Errorf("--ref %q: want YYYY-MM-DD", *refStr)
		}
		// A collection dated in the future would look fresh for months and
		// make every later collection "older than already collected".
		if ref.After(today) {
			return connector.Request{}, fmt.Errorf("--ref %s is in the future (today is %s, UTC)",
				ref.Format(time.DateOnly), today.Format(time.DateOnly))
		}
	}

	scope := collection.Scope{Full: true}
	if *cnpjs != "" && !src.SupportsSlice {
		return connector.Request{}, fmt.Errorf("%s only collects in full: --cnpjs and --depth are not supported", key)
	}
	if *cnpjs != "" {
		scope = collection.Scope{Depth: *depth}
		seen := map[string]bool{}
		for _, raw := range strings.Split(*cnpjs, ",") {
			d := nonDigits.ReplaceAllString(raw, "")
			if len(d) != 8 && len(d) != 14 {
				return connector.Request{}, fmt.Errorf("invalid CNPJ %q: want 8 or 14 digits", strings.TrimSpace(raw))
			}
			if !seen[d] {
				seen[d] = true
				scope.CNPJs = append(scope.CNPJs, d)
			}
		}
		if len(scope.CNPJs) > maxCNPJs {
			return connector.Request{}, fmt.Errorf("at most %d CNPJs per request, got %d", maxCNPJs, len(scope.CNPJs))
		}
	} else if *depth != 0 {
		return connector.Request{}, errors.New("--depth requires --cnpjs")
	}
	return connector.Request{Source: key, ReferenceDate: ref, Scope: scope}, nil
}

// collectBy is collection_request.requested_by for requests made here.
const collectBy = "cli"

// collect records and queues one collection request for the worker, through
// the same path as the scheduler and the "Coletar agora" button
// (freshness.Service.Request): the request is recorded (requested_by "cli")
// and a full request is refused while a collection of the source is running
// or a full request for it is waiting to start, with a clear message and a
// non-zero exit. A slice is never refused.
func collect(ctx context.Context, args []string, stdout io.Writer) error {
	req, err := buildRequest(args, time.Now())
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	env, err := requireEnv("DATABASE_URL", "NATS_URL")
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	pool, err := db.Open(ctx, env["DATABASE_URL"])
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	defer pool.Close()
	q, err := queue.Connect(env["NATS_URL"])
	if err != nil {
		return err
	}
	defer q.Close()
	return submit(ctx, freshness.New(pool, nil), q, req, stdout)
}

// submit records and publishes req (see collect).
func submit(ctx context.Context, svc *freshness.Service, pub freshness.Publisher, req connector.Request, stdout io.Writer) error {
	src, ok := sources.ByKey(req.Source)
	if !ok {
		return fmt.Errorf("collect: unknown source %q", req.Source)
	}
	if err := svc.Request(ctx, pub, src, req, collectBy); err != nil {
		if errors.Is(err, freshness.ErrRequestBlocked) {
			return fmt.Errorf("collect: not queued: %w; wait until it ends (sources screen or the worker log) and ask again", err)
		}
		return fmt.Errorf("collect: %w", err)
	}
	what := "full scope"
	if !req.Scope.Full {
		what = fmt.Sprintf("%d CNPJ(s), depth %d", len(req.Scope.CNPJs), req.Scope.Depth)
	}
	fmt.Fprintf(stdout, "queued %s collection for %s (%s)\n", req.Source, req.ReferenceDate.Format("2006-01-02"), what)
	// collect only queues: the worker resolves edition sources (Receita) to
	// the newest edition published by that date and, for a full scope, skips
	// it if that edition is already loaded; the outcome is in the worker log.
	fmt.Fprintln(stdout, "sources published as editions load the newest edition published by that date;"+
		" a full collection of an edition already loaded is skipped (worker log: \"nothing new\")")
	return nil
}
