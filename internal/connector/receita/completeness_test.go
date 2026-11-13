package receita

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
)

// edition lists n parts of each data kind (e.g. Socios0.zip … Socios<n-1>.zip),
// overridden per kind by counts.
func edition(n int, counts map[string]int) []string {
	var out []string
	for _, k := range dataKinds {
		c := n
		if v, ok := counts[k]; ok {
			c = v
		}
		for i := range c {
			out = append(out, fmt.Sprintf("%s%d.zip", k, i))
		}
	}
	return append(out, "Qualificacoes.zip")
}

// ruleConnector is fastConnector with the completeness rules as configured
// by the environment (newReceita defaults unless a test sets the variables).
func ruleConnector(t *testing.T, baseURL string) *receita {
	t.Helper()
	c := fastConnector(t, baseURL)
	d := newReceita(baseURL)
	c.minParts, c.minPartsExplicit, c.settle = d.minParts, d.minPartsExplicit, d.settle
	return c
}

// doneWithParts records a done full Receita collection that loaded n parts
// of each data kind (collection_file rows, as the loader records them).
func doneWithParts(t *testing.T, pool *pgxpool.Pool, ref time.Time, n int) {
	t.Helper()
	col, err := collection.Begin(ctx, pool, sourceID, ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range edition(n, nil) {
		if !dataFile.MatchString(name) {
			continue
		}
		key := rawPrefix(ref) + name
		if err := collection.RecordSourceFile(ctx, pool, col.ID, collection.File{Name: fmt.Sprintf("K3241.PART%d", i), RawKey: key,
			SHA256: strings.Repeat("a", 64), Size: 10, SourceURL: "https://example.org/receita/" + name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := collection.Finish(ctx, pool, col.ID, map[string]int64{"rows_read": 1, "rows_skipped": 0}); err != nil {
		t.Fatal(err)
	}
}

var nov12 = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)

func TestPrefixUploadIsNotComplete(t *testing.T) {
	e := newEnv(t)
	doneWithParts(t, e.pool, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), 10)
	d := newEditionsDAV(t, map[string][]string{
		"2026-10": edition(10, nil),
		"2026-11": edition(10, map[string]int{"Socios": 9}), // Socios0-8 of 10: gap-free, but short
	}, nil)
	c := fastConnector(t, d.URL+"/dav/") // minParts 1: the last collection's counts are what matter
	got, err := c.ResolveReference(ctx, nov12, e.pool)
	if err != nil || !got.ReferenceDate.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %v, %v; want 2026-10-01 (November has 9 of 10 Socios parts)", got, err)
	}
	d.mu.Lock()
	d.folders["2026-11"] = edition(10, nil)
	d.mu.Unlock()
	if got, err := c.ResolveReference(ctx, nov12, e.pool); err != nil || !got.ReferenceDate.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("after the upload finished: got %v, %v; want 2026-11-01", got, err)
	}
}

func TestFirstLoadNeedsTheDefaultPartCount(t *testing.T) {
	e := newEnv(t)
	d := newEditionsDAV(t, map[string][]string{"2026-11": edition(3, nil)}, nil)
	t.Setenv("RECEITA_SETTLE_HOURS", "0")
	c := ruleConnector(t, d.URL+"/dav/")
	if c.minParts != defaultMinParts {
		t.Fatalf("minParts = %d, want the default %d", c.minParts, defaultMinParts)
	}
	if _, err := c.ResolveReference(ctx, nov12, e.pool); !errors.Is(err, connector.ErrNoEdition) {
		t.Errorf("3 parts with no previous load: err = %v, want ErrNoEdition", err)
	}
	t.Setenv("RECEITA_MIN_PARTS", "3")
	c = ruleConnector(t, d.URL+"/dav/")
	if got, err := c.ResolveReference(ctx, nov12, e.pool); err != nil || !got.ReferenceDate.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("RECEITA_MIN_PARTS=3: got %v, %v", got, err)
	}
}

func TestEditionMustBeSettled(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	gmt := func(t time.Time) string { return t.UTC().Format(http.TimeFormat) }
	d := newEditionsDAV(t, map[string][]string{"2026-10": edition(1, nil), "2026-11": edition(1, nil)}, nil)
	for _, m := range []string{"2026-10", "2026-11"} {
		for _, f := range edition(1, nil) {
			d.lastMod[m+"/"+f] = gmt(time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC))
		}
	}
	t.Setenv("RECEITA_MIN_PARTS", "1")
	c := ruleConnector(t, d.URL+"/dav/")
	c.now = func() time.Time { return now }
	if c.settle != defaultSettleHours*time.Hour {
		t.Fatalf("settle = %v, want the default", c.settle)
	}
	resolve := func() string {
		got, err := c.ResolveReference(ctx, nov12, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got.ReferenceDate.Format("2006-01")
	}
	set := func(lm string) {
		d.mu.Lock()
		if lm == "" {
			delete(d.lastMod, "2026-11/Socios0.zip")
		} else {
			d.lastMod["2026-11/Socios0.zip"] = lm
		}
		d.mu.Unlock()
	}
	set(gmt(now.Add(-2 * time.Hour))) // uploaded 2 h ago, 6 h required
	if got := resolve(); got != "2026-10" {
		t.Errorf("fresh upload: resolved %s, want 2026-10", got)
	}
	set(gmt(now.Add(-7 * time.Hour)))
	if got := resolve(); got != "2026-11" {
		t.Errorf("settled upload: resolved %s, want 2026-11", got)
	}
	set("") // no date: not settled
	if got := resolve(); got != "2026-10" {
		t.Errorf("missing last-modified: resolved %s, want 2026-10", got)
	}
	set("2026-11-12T03:00:00Z") // not RFC 1123: not settled
	if got := resolve(); got != "2026-10" {
		t.Errorf("malformed last-modified: resolved %s, want 2026-10", got)
	}
	t.Setenv("RECEITA_SETTLE_HOURS", "0") // disabled
	c = ruleConnector(t, d.URL+"/dav/")
	c.now = func() time.Time { return now }
	if got := resolve(); got != "2026-11" {
		t.Errorf("settle disabled: resolved %s, want 2026-11", got)
	}
}

func TestFetchRefusesAListingChangedSinceResolution(t *testing.T) {
	ref := time.Date(2031, 7, 1, 0, 0, 0, 0, time.UTC) // own month: nothing else stores these keys
	d := newEditionsDAV(t, map[string][]string{"2031-07": edition(1, nil)}, nil)
	c := fastConnector(t, d.URL+"/dav/")
	raw := newRaw(t)
	got, err := c.ResolveReference(ctx, ref, nil)
	if err != nil || !got.ReferenceDate.Equal(ref) {
		t.Fatalf("resolve: %v, %v", got, err)
	}
	// One more part appears between resolution and download.
	d.mu.Lock()
	d.folders["2031-07"] = edition(1, map[string]int{"Socios": 2})
	d.mu.Unlock()
	_, err = c.Fetch(ctx, got.ReferenceDate, raw)
	if !errors.Is(err, errEditionChanged) || !strings.Contains(err.Error(), "2031-07") {
		t.Fatalf("Fetch err = %v, want errEditionChanged", err)
	}
	for _, f := range edition(1, map[string]int{"Socios": 2}) {
		if _, ok, err := raw.Stat(ctx, rawPrefix(ref)+f); err != nil || ok {
			t.Errorf("%s stored (ok=%v err=%v): nothing may be downloaded", f, ok, err)
		}
	}
	// A size change is caught too.
	d.mu.Lock()
	d.folders["2031-07"] = edition(1, nil)
	d.mu.Unlock()
	if _, err := c.ResolveReference(ctx, ref, nil); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.sizes["2031-07/Empresas0.zip"] = 11
	d.mu.Unlock()
	if _, err := c.Fetch(ctx, ref, raw); !errors.Is(err, errEditionChanged) {
		t.Errorf("size change: err = %v, want errEditionChanged", err)
	}
}

// An explicit RECEITA_MIN_PARTS overrides the part counts of the last
// collection: the owner's way out when the Receita publishes fewer parts.
func TestExplicitMinPartsOverridesTheHistory(t *testing.T) {
	e := newEnv(t)
	doneWithParts(t, e.pool, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), 10)
	d := newEditionsDAV(t, map[string][]string{"2026-11": edition(8, nil)}, nil)
	t.Setenv("RECEITA_SETTLE_HOURS", "0")
	c := ruleConnector(t, d.URL+"/dav/")
	if _, err := c.ResolveReference(ctx, nov12, e.pool); !errors.Is(err, connector.ErrNoEdition) {
		t.Fatalf("8 parts after a load of 10, no override: err = %v, want ErrNoEdition", err)
	}
	t.Setenv("RECEITA_MIN_PARTS", "8")
	c = ruleConnector(t, d.URL+"/dav/")
	if got, err := c.ResolveReference(ctx, nov12, e.pool); err != nil || !got.ReferenceDate.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("RECEITA_MIN_PARTS=8 set explicitly: got %v, %v; want 2026-11-01", got, err)
	}
}

// The observed date of an edition is the UTC day of its newest data part.
func TestEditionObservedDateIsThePublicationDay(t *testing.T) {
	d := newEditionsDAV(t, map[string][]string{"2026-10": edition(1, nil)}, nil)
	for i, f := range edition(1, nil) {
		d.lastMod["2026-10/"+f] = time.Date(2026, 10, 10+i, 23, 30, 0, 0, time.UTC).Format(http.TimeFormat)
	}
	got, err := fastConnector(t, d.URL+"/dav/").ResolveReference(ctx, nov12, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Three data parts dated 10, 11 and 12 October; Qualificacoes (13th) is no data part.
	if want := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC); !got.ObservedDate.Equal(want) || !got.ReferenceDate.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %+v, want reference 2026-10-01 observed %s", got, want)
	}
}
