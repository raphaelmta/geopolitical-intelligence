package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"geoint/internal/api"
	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/queue"
	"geoint/internal/sources"
)

// withoutConnector makes the API see the catalogue with key's source as
// having no connector (every real source has one).
func withoutConnector(key string) func(*api.Deps) {
	return func(d *api.Deps) {
		d.Source = func(k string) (sources.Source, bool) {
			src, ok := sources.ByKey(k)
			if k == key {
				src.HasConnector = false
			}
			return src, ok
		}
	}
}

// noIngest fails if a request for key is published within a second.
func (e *env) noIngest(key string) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(bg, time.Second)
	defer cancel()
	got := make(chan struct{}, 1)
	_ = e.q.Consume(ctx, queue.StreamIngest, "api-test-none-"+key, "ingest.request."+key, func(context.Context, []byte) error {
		select {
		case got <- struct{}{}:
		default:
		}
		return nil
	})
	select {
	case <-got:
		e.t.Fatalf("an ingest request for %s was published", key)
	default:
	}
}

// nextIngest returns the next ingest request published for receita.
func (e *env) nextIngest() connector.Request {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	got := make(chan []byte, 1)
	go func() {
		_ = e.q.Consume(ctx, queue.StreamIngest, "api-test", "ingest.request.receita", func(_ context.Context, b []byte) error {
			select {
			case got <- b:
			default:
			}
			cancel()
			return nil
		})
	}()
	select {
	case b := <-got:
		var r connector.Request
		if err := json.Unmarshal(b, &r); err != nil {
			e.t.Fatal(err)
		}
		return r
	case <-ctx.Done():
		e.t.Fatal("no ingest request published")
	}
	return connector.Request{}
}

func TestCollectRequiresAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false, withoutConnector("camara"))
	wantError(t, e.do("POST", "/api/v1/sources/receita/collect", tokAnalyst, ""), http.StatusForbidden, "forbidden")

	want[map[string]any](t, e.do("POST", "/api/v1/sources/receita/collect", tokAdmin, ""), http.StatusAccepted)
	r := e.nextIngest()
	today := time.Now().UTC().Format(time.DateOnly)
	if r.Source != "receita" || !r.Scope.Full || r.ReferenceDate.Format(time.DateOnly) != today {
		t.Fatalf("request %+v", r)
	}
	if n := e.auditCount("source.collect", nil); n != 1 {
		t.Fatalf("%d source.collect rows", n)
	}
	// A source without a connector: 409, nothing audited nor published.
	wantError(t, e.do("POST", "/api/v1/sources/camara/collect", tokAdmin, ""), http.StatusConflict, "conflict")
	if n := e.auditCount("source.collect", nil); n != 1 {
		t.Fatalf("%d source.collect rows after the refused collect", n)
	}
	e.noIngest("camara")
	// A source that only collects in full refuses a slice: 400, nothing audited nor published.
	wantError(t, e.do("POST", "/api/v1/sources/tse/collect", tokAdmin, `{"cnpjs":["12345678"],"depth":1}`),
		http.StatusBadRequest, "bad_request")
	if n := e.auditCount("source.collect", nil); n != 1 {
		t.Fatalf("%d source.collect rows after the refused slice", n)
	}
	e.noIngest("tse")

	// The first request started and finished: a new full one is accepted.
	e.collected(1)
	want[map[string]any](t, e.do("POST", "/api/v1/sources/receita/collect", tokAdmin, `{}`), http.StatusAccepted)
	if r := e.nextIngest(); !r.Scope.Full {
		t.Fatalf("{} must ask for a full collection: %+v", r)
	}

	want[map[string]any](t, e.do("POST", "/api/v1/sources/receita/collect", tokAdmin,
		`{"cnpjs":["12.345.678/0001-90","12345678","87654321"],"depth":2}`), http.StatusAccepted)
	r = e.nextIngest()
	if r.Scope.Full || r.Scope.Depth != 2 || len(r.Scope.CNPJs) != 2 || r.Scope.CNPJs[0] != "12345678" || r.Scope.CNPJs[1] != "87654321" {
		t.Fatalf("slice request %+v", r)
	}
}

// collected records a full collection of source that started now and finished.
func (e *env) collected(source int16) {
	e.t.Helper()
	c, err := collection.Begin(bg, e.pool, source, time.Now().UTC().Truncate(24*time.Hour), collection.Scope{Full: true})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := collection.Finish(bg, e.pool, c.ID, nil); err != nil {
		e.t.Fatal(err)
	}
}

// ingestCount counts, for a second, the ingest requests for key that the
// durable consumer has not read yet (the ingest stream is a work queue: one
// consumer per subject, a read message is gone).
func (e *env) ingestCount(key string) int {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(bg, time.Second)
	defer cancel()
	var mu sync.Mutex
	n := 0
	_ = e.q.Consume(ctx, queue.StreamIngest, "api-test-count-"+key, "ingest.request."+key, func(context.Context, []byte) error {
		mu.Lock()
		n++
		mu.Unlock()
		return nil
	})
	mu.Lock()
	defer mu.Unlock()
	return n
}

type sourcePending struct {
	Key              string  `json:"key"`
	PendingRequestAt *string `json:"pending_request_at"`
	LastError        *string `json:"last_error"`
}

func (e *env) pendingAt(key string) *string {
	e.t.Helper()
	for _, s := range want[[]sourcePending](e.t, e.do("GET", "/api/v1/sources", tokAnalyst, ""), http.StatusOK) {
		if s.Key == key {
			return s.PendingRequestAt
		}
	}
	e.t.Fatalf("%s not listed", key)
	return nil
}

func TestCollectRefusesADuplicateFullRequest(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	if p := e.pendingAt("receita"); p != nil {
		t.Fatalf("pending_request_at %q before any request", *p)
	}
	want[map[string]any](t, e.do("POST", "/api/v1/sources/receita/collect", tokAdmin, ""), http.StatusAccepted)
	var by string
	var at time.Time
	if err := e.pool.QueryRow(bg, `SELECT requested_by, requested_at FROM collection_request WHERE full_scope AND published_at IS NOT NULL`).
		Scan(&by, &at); err != nil || by != "sub-admin" {
		t.Fatalf("request row by %q: %v", by, err)
	}
	if p := e.pendingAt("receita"); p == nil || *p != at.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("pending_request_at %v, want %s", p, at.UTC().Format(time.RFC3339Nano))
	}

	// A second full request while the first waits: audited, 409, nothing published.
	wantError(t, e.do("POST", "/api/v1/sources/receita/collect", tokAdmin, `{}`), http.StatusConflict, "conflict")
	if n := e.auditCount("source.collect", nil); n != 2 {
		t.Fatalf("%d source.collect rows, want 2 (the refused attempt is audited)", n)
	}
	if n := e.count(`SELECT count(*) FROM collection_request`); n != 1 {
		t.Fatalf("%d request rows, want 1", n)
	}
	// A slice is not held back.
	want[map[string]any](t, e.do("POST", "/api/v1/sources/receita/collect", tokAdmin, `{"cnpjs":["12345678"]}`), http.StatusAccepted)
	if n := e.count(`SELECT count(*) FROM collection_request WHERE NOT full_scope AND requested_by = 'sub-admin' AND published_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d slice request rows, want 1", n)
	}
	if n := e.ingestCount("receita"); n != 2 {
		t.Fatalf("%d messages, want the first full request and the slice", n)
	}

	// The collection starts (a live run, holding its run lock): no longer
	// pending, but running blocks.
	c, err := collection.Start(bg, e.pool, 1, time.Now().UTC().Truncate(24*time.Hour), time.Time{}, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	if p := e.pendingAt("receita"); p != nil {
		t.Fatalf("pending_request_at %q after the collection started", *p)
	}
	wantError(t, e.do("POST", "/api/v1/sources/receita/collect", tokAdmin, ""), http.StatusConflict, "conflict")
	// Finished: an admin may ask again at once (no 24 h cool-down).
	if err := collection.Finish(bg, e.pool, c.ID, nil); err != nil {
		t.Fatal(err)
	}
	want[map[string]any](t, e.do("POST", "/api/v1/sources/receita/collect", tokAdmin, ""), http.StatusAccepted)
	if n := e.ingestCount("receita"); n != 1 {
		t.Fatalf("%d new messages, want 1", n)
	}
}

func TestSourcesList(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true, withoutConnector("camara"))
	// The latest TSE collection failed with a long message that has an 11-digit run.
	col, err := collection.Begin(bg, e.pool, 2, time.Now().UTC().Truncate(24*time.Hour), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	// Senado's only collection is old: stale.
	old, err := collection.Begin(bg, e.pool, 4, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Finish(bg, e.pool, old.ID, nil); err != nil {
		t.Fatal(err)
	}
	msg := "falha JOANA 12345678901 " + strings.Repeat("é", 400)
	if err := collection.Fail(bg, e.pool, col.ID, errors.New(msg)); err != nil {
		t.Fatal(err)
	}
	type sourceView struct {
		Key               string  `json:"key"`
		Status            string  `json:"status"`
		Collectable       bool    `json:"collectable"`
		Editions          bool    `json:"editions"`
		LastReferenceDate *string `json:"last_reference_date"`
		LastObservedDate  *string `json:"last_observed_date"`
		NextExpectedAt    *string `json:"next_expected_at"`
		LastCheckedAt     *string `json:"last_checked_at"`
		LastError         *string `json:"last_error"`
	}
	for _, tok := range []string{tokAnalyst, tokAdmin} {
		got := want[[]sourceView](t, e.do("GET", "/api/v1/sources", tok, ""), http.StatusOK)
		status := map[string]string{}
		for _, s := range got {
			status[s.Key] = s.Status
			if s.Collectable != (s.Key != "camara") {
				t.Errorf("%s: collectable %v", s.Key, s.Collectable)
			}
			if s.Editions != (s.Key == "receita") {
				t.Errorf("%s: editions %v, want true only for receita", s.Key, s.Editions)
			}
			if s.Key == "tse" && (s.LastReferenceDate == nil || *s.LastReferenceDate != "2020-01-01") {
				t.Errorf("tse reference date %v", s.LastReferenceDate)
			}
			// last_observed_date: the fixture's collections were observed on
			// the day they started (today); null for a source never collected.
			today := time.Now().UTC().Format(time.DateOnly)
			switch {
			case s.Key == "camara" && s.LastObservedDate != nil:
				t.Errorf("camara last_observed_date %v, want null", *s.LastObservedDate)
			case s.Key == "receita" && (s.LastObservedDate == nil || *s.LastObservedDate != today):
				t.Errorf("receita last_observed_date %v, want %s", s.LastObservedDate, today)
			}
			// last_checked_at: an RFC 3339 UTC instant for collected sources, null for never.
			switch {
			case s.Key == "camara" && s.LastCheckedAt != nil:
				t.Errorf("camara last_checked_at %v, want null", *s.LastCheckedAt)
			case s.Key == "receita" || s.Key == "tse":
				if s.LastCheckedAt == nil || !strings.HasSuffix(*s.LastCheckedAt, "Z") {
					t.Errorf("%s last_checked_at %v", s.Key, s.LastCheckedAt)
				} else if _, err := time.Parse(time.RFC3339, *s.LastCheckedAt); err != nil {
					t.Errorf("%s last_checked_at %q: %v", s.Key, *s.LastCheckedAt, err)
				}
			}
			switch {
			case tok == tokAnalyst && s.LastError != nil:
				t.Errorf("%s: analyst sees last_error %q", s.Key, *s.LastError)
			case tok == tokAdmin && s.Key != "tse" && (s.LastError == nil || *s.LastError != ""):
				t.Errorf("admin %s last_error %v, want \"\" (sent for every source)", s.Key, s.LastError)
			case tok == tokAdmin && s.Key == "tse":
				if s.LastError == nil || utf8.RuneCountInString(*s.LastError) != 300 ||
					strings.Contains(*s.LastError, "12345678901") || !strings.Contains(*s.LastError, "***456789**") {
					t.Errorf("admin tse last_error %v", s.LastError)
				}
			}
		}
		if status["receita"] != "fresh" || status["tse"] != "failed" || status["camara"] != "never" || status["senado"] != "stale" {
			t.Fatalf("statuses %v", status)
		}
	}
}
