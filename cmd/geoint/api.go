package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"geoint/internal/api"
	"geoint/internal/auth"
	"geoint/internal/db"
	"geoint/internal/freshness"
	"geoint/internal/graph"
	"geoint/internal/identity"
	"geoint/internal/jobs"
	"geoint/internal/queue"
	"geoint/internal/search"
)

const apiShutdownTimeout = 10 * time.Second

// apiServer returns the hardened HTTP server of the API.
func apiServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// serveAPI serves the HTTP API until SIGTERM or SIGINT, then shuts down
// gracefully. The identity provider may be down at start: the verifier
// discovers it on the first request (503 auth_unavailable until then).
func serveAPI() error {
	env, err := requireEnv("DATABASE_URL", "NATS_URL", "OIDC_ISSUER", "OIDC_AUDIENCE")
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	addr := os.Getenv("API_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	discovery := os.Getenv("OIDC_DISCOVERY_URL")
	if discovery == "" {
		discovery = env["OIDC_ISSUER"]
	}
	// Host names accepted besides localhost, 127.0.0.1, ::1 and api.
	allowedHosts, err := api.ParseAllowedHosts(os.Getenv("API_ALLOWED_HOSTS"))
	if err != nil {
		return fmt.Errorf("api: API_ALLOWED_HOSTS: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The verifier fetches keys with this context: it lives as long as the process.
	life, cancelLife := context.WithCancel(context.Background())
	defer cancelLife()

	verifier, err := auth.NewOIDCVerifierWithDiscovery(life, env["OIDC_ISSUER"], discovery, env["OIDC_AUDIENCE"])
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	pool, err := db.Open(ctx, env["DATABASE_URL"])
	if err != nil {
		return err
	}
	defer pool.Close()
	q, err := queue.Connect(env["NATS_URL"])
	if err != nil {
		return err
	}
	defer q.Close()

	jobSvc := jobs.New(pool, q)
	jobs.RegisterGraphTypes(jobSvc) // enqueue only: the worker runs them
	h := api.NewRouter(api.Deps{
		Pool:      pool,
		Graph:     graph.New(pool),
		Search:    search.New(pool),
		Jobs:      jobSvc,
		Identity:  &identity.Resolver{Pool: pool},
		Fresh:     freshness.New(pool, time.Now),
		Queue:     q,
		Verifier:  verifier,
		WebOrigin: os.Getenv("WEB_ORIGIN"),

		AllowedHosts: allowedHosts,
	})
	srv := apiServer(addr, h)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("api started", "addr", addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), apiShutdownTimeout)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	cancelLife()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
