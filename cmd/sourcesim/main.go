// Command sourcesim serves the repository's testdata/ as the five public
// sources, for the end-to-end test (deploy/compose.e2e.yml). Test tooling
// only: it is not part of the geoint binary or of deploy/compose.yml.
//
//	sourcesim [-addr :8080] [-testdata /testdata]
//	sourcesim -check [-addr :8080]   exit 0 when the server answers /healthz
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"geoint/internal/sourcesim"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dir := flag.String("testdata", "/testdata", "the repository's testdata directory")
	check := flag.Bool("check", false, "health check: GET /healthz on addr and exit")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	if *check {
		os.Exit(healthCheck(*addr))
	}
	sim, err := sourcesim.New(*dir, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(sim),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	slog.Info("sourcesim listening", "addr", *addr, "testdata", *dir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func healthCheck(addr string) int {
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + host + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// logRequests logs method, path and status of every request (no headers:
// the Receita route carries a Basic auth user).
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" {
			slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status)
		}
	})
}
