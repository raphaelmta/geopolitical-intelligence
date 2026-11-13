package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"geoint/internal/api"
)

func failingAudit(d *api.Deps) {
	d.Audit = func(context.Context, string, string, *int64, map[string]any) error { return errors.New("audit down") }
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(bg, query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// Write actions are audited first: when the audit row cannot be written,
// the action is not performed.
func TestWriteNotPerformedWithoutAudit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false, failingAudit)
	prop, r1, _, r2, _, _ := proposalFixture(e)

	wantError(t, e.do("POST", fmt.Sprintf("/api/v1/identity/proposals/%d/decision", prop), tokAnalyst, `{"confirm":true}`),
		http.StatusInternalServerError, "internal")
	wantError(t, e.do("POST", "/api/v1/identity/merge", tokAnalyst, fmt.Sprintf(`{"record_ids":[%d,%d],"reason":"x"}`, r1, r2)),
		http.StatusInternalServerError, "internal")
	wantError(t, e.do("POST", "/api/v1/identity/split", tokAnalyst, fmt.Sprintf(`{"record_ids":[%d],"reason":"x"}`, r2)),
		http.StatusInternalServerError, "internal")
	if n := e.count(`SELECT count(*) FROM identity_decision`); n != 0 {
		t.Fatalf("%d identity decisions taken without audit", n)
	}
	if n := e.count(`SELECT count(*) FROM identity_proposal WHERE status = 'pending'`); n != 1 {
		t.Fatal("proposal decided without audit")
	}

	wantError(t, e.do("POST", "/api/v1/jobs", tokAnalyst, `{"type":"paths","params":{"a":1,"b":2}}`),
		http.StatusInternalServerError, "internal")
	if n := e.count(`SELECT count(*) FROM job`); n != 0 {
		t.Fatalf("%d jobs enqueued without audit", n)
	}

	wantError(t, e.do("POST", "/api/v1/sources/receita/collect", tokAdmin, ""), http.StatusInternalServerError, "internal")
	e.noIngest("receita")
}

// A client that leaves right before the action does not erase the attempt:
// the audit row is written on a context of its own.
func TestWriteAuditSurvivesClientGone(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	prop, _, _, _, _, _ := proposalFixture(e)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	r := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/identity/proposals/%d/decision", prop),
		strings.NewReader(`{"confirm":true}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tokAnalyst)
	r.Host = testHost
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Body.Len() != 0 {
		t.Fatalf("answer written to a gone client: %s", rec.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.auditCount("identity.decide", nil) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no audit row for the attempt")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := e.count(`SELECT count(*) FROM identity_proposal WHERE status = 'pending'`); n != 1 {
		t.Fatal("the cancelled request still decided the proposal")
	}
}
