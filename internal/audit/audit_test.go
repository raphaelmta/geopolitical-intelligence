package audit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"geoint/internal/audit"
	"geoint/internal/testutil"
)

func TestAuditLogWritesRow(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	id := int64(42)
	before := time.Now().Add(-time.Minute)
	if err := audit.Log(ctx, pool, "sub-1", "entity.view", &id, map[string]any{"depth": 2}); err != nil {
		t.Fatal(err)
	}

	var (
		n        int
		user     string
		action   string
		entityID *int64
		at       time.Time
		detail   string
	)
	if err := pool.QueryRow(ctx, `SELECT count(*) OVER (), user_sub, action, entity_id, at, detail::text FROM audit_log`).
		Scan(&n, &user, &action, &entityID, &at, &detail); err != nil {
		t.Fatal(err)
	}
	if n != 1 || user != "sub-1" || action != "entity.view" || entityID == nil || *entityID != 42 {
		t.Errorf("row = n=%d user=%q action=%q entity=%v", n, user, action, entityID)
	}
	if at.Before(before) || at.After(time.Now().Add(time.Minute)) {
		t.Errorf("at = %v", at)
	}
	if detail != `{"depth": 2}` {
		t.Errorf("detail = %s", detail)
	}
}

func TestAuditLogNilDetailAndEntity(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	if err := audit.Log(ctx, pool, "sub-1", "search", nil, nil); err != nil {
		t.Fatal(err)
	}
	var entityID *int64
	var detail string
	if err := pool.QueryRow(ctx, `SELECT entity_id, detail::text FROM audit_log`).Scan(&entityID, &detail); err != nil {
		t.Fatal(err)
	}
	if entityID != nil || detail != "{}" {
		t.Errorf("entity=%v detail=%s", entityID, detail)
	}
}

func TestAuditLogRejectsBadInput(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	big := map[string]any{"x": strings.Repeat("a", 16*1024)}
	cases := map[string]func() error{
		"empty user":       func() error { return audit.Log(ctx, pool, "", "search", nil, nil) },
		"blank user":       func() error { return audit.Log(ctx, pool, "  ", "search", nil, nil) },
		"blank action":     func() error { return audit.Log(ctx, pool, "sub-1", "\t\n", nil, nil) },
		"empty action":     func() error { return audit.Log(ctx, pool, "sub-1", "", nil, nil) },
		"oversized detail": func() error { return audit.Log(ctx, pool, "sub-1", "search", nil, big) },
		"unmarshalable":    func() error { return audit.Log(ctx, pool, "sub-1", "search", nil, map[string]any{"f": func() {}}) },
	}
	for name, f := range cases {
		if err := f(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows written for rejected input", n)
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	id := int64(7)
	if err := audit.Log(ctx, pool, "sub-1", "entity.view", &id, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"delete":        `DELETE FROM audit_log`,
		"truncate":      `TRUNCATE audit_log`,
		"update detail": `UPDATE audit_log SET detail = '{}'`,
		"update user":   `UPDATE audit_log SET user_sub = 'someone-else'`,
		"no-op update":  `UPDATE audit_log SET action = action`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: accepted by the database", name)
		}
	}
	var n int
	var user string
	if err := pool.QueryRow(ctx, `SELECT count(*), min(user_sub) FROM audit_log`).Scan(&n, &user); err != nil {
		t.Fatal(err)
	}
	if n != 1 || user != "sub-1" {
		t.Errorf("rows=%d user=%q after refused changes", n, user)
	}
}

func TestAuditLogTableChecks(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	for _, sql := range []string{
		`INSERT INTO audit_log (user_sub, action) VALUES ('', 'x')`,
		`INSERT INTO audit_log (user_sub, action) VALUES ('u', '')`,
		`INSERT INTO audit_log (user_sub, action) VALUES ('  ', 'x')`,
		`INSERT INTO audit_log (user_sub, action) VALUES ('u', E' \t\n')`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: accepted", sql)
		}
	}
}
