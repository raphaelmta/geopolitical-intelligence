package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"geoint/internal/db"
	"geoint/internal/sources"
	"geoint/internal/testutil"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"no args", nil, 2, "", "usage: geoint"},
		{"unknown subcommand", []string{"bogus"}, 2, "", "unknown command"},
		{"version", []string{"version"}, 0, version, ""},
		{"collect without source", []string{"collect"}, 1, "", "usage: geoint collect"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			checkOutput(t, "stdout", stdout.String(), tc.wantStdout)
			checkOutput(t, "stderr", stderr.String(), tc.wantStderr)
		})
	}
}

// checkOutput: an empty want means the stream must be empty (strings.Contains
// with "" would accept anything); otherwise it must contain want.
func checkOutput(t *testing.T, name, got, want string) {
	t.Helper()
	if want == "" {
		if got != "" {
			t.Errorf("%s = %q, want it empty", name, got)
		}
		return
	}
	if !strings.Contains(got, want) {
		t.Errorf("%s = %q, want to contain %q", name, got, want)
	}
}

func TestMigrateAppliesSchemaAndSourcesTwice(t *testing.T) {
	t.Setenv("DATABASE_URL", testutil.EmptySchemaURL(t))
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"migrate"}, &stdout, &stderr); code != 0 {
			t.Fatalf("run %d: exit code %d, stderr %q", i, code, stderr.String())
		}
	}
	pool, err := db.Open(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM source").Scan(&n); err != nil || n != len(sources.All) {
		t.Fatalf("sources rows=%d err=%v", n, err)
	}
}

// A role password that would be refused never reaches the server; migrate
// fails with the reason (the real roles are left alone: they are
// cluster-wide and other test packages log in with them).
func TestMigrateRefusesAWeakRolePassword(t *testing.T) {
	t.Setenv("DATABASE_URL", testutil.EmptySchemaURL(t))
	t.Setenv("DB_WORKER_PASSWORD", "short")
	t.Setenv("DB_API_PASSWORD", "")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"migrate"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "shorter than 16") {
		t.Fatalf("exit code %d, stderr %q; want 1 and the reason", code, stderr.String())
	}
}
