package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/freshness"
	"geoint/internal/testutil"
)

var now = time.Date(2026, 11, 11, 15, 30, 0, 0, time.UTC)

func TestBuildRequestDefaults(t *testing.T) {
	r, err := buildRequest([]string{"receita"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != "receita" || !r.Scope.Full || !r.ReferenceDate.Equal(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected %+v", r)
	}
}

func TestBuildRequestCNPJsDepthAndRef(t *testing.T) {
	r, err := buildRequest([]string{"receita", "--cnpjs", "12.345.678/0001-95, 12345678", "--depth", "2", "--ref", "2026-10-01"}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := collection.Scope{CNPJs: []string{"12345678000195", "12345678"}, Depth: 2}
	if !reflect.DeepEqual(r.Scope, want) || !r.ReferenceDate.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected %+v", r)
	}
}

func TestBuildRequestRejectsBadInput(t *testing.T) {
	cases := map[string][]string{
		"no args":        {},
		"flag first":     {"--cnpjs", "12345678"},
		"unknown source": {"nope"},
		"short cnpj":     {"receita", "--cnpjs", "123"},
		"empty item":     {"receita", "--cnpjs", "12345678,"},
		"bad ref":        {"receita", "--ref", "11/11/2026"},
		"negative depth": {"receita", "--cnpjs", "12345678", "--depth", "-1"},
		"depth w/o cnpj": {"receita", "--depth", "2"},
		"depth too big":  {"receita", "--cnpjs", "12345678", "--depth", "7"},
		"too many":       {"receita", "--cnpjs", manyCNPJs(1001)},
		"extra arg":      {"receita", "extra"},
		"unknown flag":   {"receita", "--wat"},
		"future ref":     {"cvm", "--ref", "2026-11-12"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := buildRequest(args, now); err == nil {
				t.Fatalf("%v accepted", args)
			}
		})
	}
	// A source that only collects in full refuses a slice, with a clear message.
	if _, err := buildRequest([]string{"tse", "--cnpjs", "12345678"}, now); err == nil || !strings.Contains(err.Error(), "only collects in full") {
		t.Errorf("tse slice: err = %v", err)
	}
	if r, err := buildRequest([]string{"tse"}, now); err != nil || !r.Scope.Full {
		t.Errorf("tse full: %+v, %v", r, err)
	}
	if _, err := buildRequest([]string{"nope"}, now); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the source: %v", err)
	}
}

func manyCNPJs(n int) string {
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf("%014d", i))
	}
	return strings.Join(parts, ",")
}

func TestBuildRequestDedupesCNPJs(t *testing.T) {
	r, err := buildRequest([]string{"receita", "--cnpjs", "12.345.678,12345678,87654321"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Scope.CNPJs, []string{"12345678", "87654321"}) {
		t.Fatalf("cnpjs = %v", r.Scope.CNPJs)
	}
	if _, err := buildRequest([]string{"receita", "--cnpjs", manyCNPJs(1000)}, now); err != nil {
		t.Errorf("1000 CNPJs rejected: %v", err)
	}
}

func TestBuildRequestPinsAnEditionWithAPastRef(t *testing.T) {
	r, err := buildRequest([]string{"receita", "--ref", "2026-10-31"}, now)
	if err != nil || !r.ReferenceDate.Equal(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %+v, %v", r, err)
	}
	if _, err := buildRequest([]string{"receita", "--ref", "2026-11-11"}, now); err != nil {
		t.Errorf("today rejected: %v", err)
	}
	if _, err := buildRequest([]string{"receita", "--ref", "2026-11-12"}, now); err == nil || !strings.Contains(err.Error(), "future") {
		t.Errorf("tomorrow: err = %v, want a future-date error", err)
	}
}

type recordingPub struct{ subjects []string }

func (p *recordingPub) Publish(_ context.Context, subject string, _ any) error {
	p.subjects = append(p.subjects, subject)
	return nil
}

// geoint collect goes through freshness.Service.Request: recorded as "cli",
// refused (non-zero exit, clear message) while a collection runs or a full
// request waits; a slice is never refused.
func TestSubmitRecordsAndAppliesTheBlockingRule(t *testing.T) {
	pool := testutil.DB(t)
	testutil.SeedSources(t, pool)
	svc := freshness.New(pool, nil)
	pub := &recordingPub{}
	var out strings.Builder
	full := connector.Request{Source: "tse", ReferenceDate: time.Now().UTC().Truncate(24 * time.Hour), Scope: collection.Scope{Full: true}}
	if err := submit(context.Background(), svc, pub, full, &out); err != nil {
		t.Fatal(err)
	}
	var by string
	var isFull bool
	if err := pool.QueryRow(context.Background(), `SELECT requested_by, full_scope FROM collection_request ORDER BY id DESC LIMIT 1`).
		Scan(&by, &isFull); err != nil {
		t.Fatal(err)
	}
	if by != "cli" || !isFull || len(pub.subjects) != 1 || pub.subjects[0] != "ingest.request.tse" {
		t.Fatalf("recorded by %q full=%v, published %v", by, isFull, pub.subjects)
	}
	// The request now waits in the queue: a second full one is refused.
	err := submit(context.Background(), svc, pub, full, &out)
	if !errors.Is(err, freshness.ErrRequestBlocked) || !strings.Contains(err.Error(), "not queued") {
		t.Fatalf("second full request: err = %v, want ErrRequestBlocked with a clear message", err)
	}
	if len(pub.subjects) != 1 {
		t.Fatalf("published %v after a refusal", pub.subjects)
	}
	// A slice of Receita is never refused, even while a full one runs.
	run, err := collection.Start(context.Background(), pool, 1, full.ReferenceDate, time.Time{}, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Release()
	slice := connector.Request{Source: "receita", ReferenceDate: full.ReferenceDate, Scope: collection.Scope{CNPJs: []string{"12345678"}}}
	if err := submit(context.Background(), svc, pub, slice, &out); err != nil {
		t.Fatalf("slice refused: %v", err)
	}
	fullReceita := slice
	fullReceita.Scope = collection.Scope{Full: true}
	if err := submit(context.Background(), svc, pub, fullReceita, &out); !errors.Is(err, freshness.ErrRequestBlocked) {
		t.Fatalf("full receita while one runs: err = %v, want ErrRequestBlocked", err)
	}
}
