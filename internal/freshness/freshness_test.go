package freshness_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"geoint/internal/collection"
	"geoint/internal/freshness"
	"geoint/internal/testutil"
)

var bg = context.Background()

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func TestFreshnessStatuses(t *testing.T) {
	p := testutil.DB(t)
	testutil.SeedSources(t, p)
	now := day(2026, 6, 1)

	// Observed on the reference date: staleness counts from the observed
	// date (TestFreshnessCountsFromThePublicationDate).
	begin := func(src int16, ref time.Time, s collection.Scope) int64 {
		t.Helper()
		c, err := collection.BeginObserved(bg, p, src, ref, ref, s)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	done := func(src int16, ref time.Time, s collection.Scope, stats map[string]int64) int64 {
		t.Helper()
		id := begin(src, ref, s)
		if err := collection.Finish(bg, p, id, stats); err != nil {
			t.Fatal(err)
		}
		return id
	}
	full := collection.Scope{Full: true}
	slice := collection.Scope{CNPJs: []string{"12345678"}, Depth: 1}

	// receita (stale after 45 days): done 10 days ago, then a slice and a
	// running collection, neither of which changes anything but the stats.
	done(1, now.AddDate(0, 0, -10), full, map[string]int64{"rows_read": 7})
	done(1, now.AddDate(0, 0, -1), slice, map[string]int64{"rows_read": 3})
	begin(1, now, full)
	// tse (stale after 30 days): done 40 days ago.
	done(2, now.AddDate(0, 0, -40), full, nil)
	// camara: nothing. cvm: only a slice, and a failure that is not the latest.
	failed := begin(5, now.AddDate(0, 0, -5), full)
	if err := collection.Fail(bg, p, failed, errors.New("old failure")); err != nil {
		t.Fatal(err)
	}
	done(5, now.AddDate(0, 0, -4), slice, nil)
	// senado: done, then the latest failed with a long message.
	done(4, now.AddDate(0, 0, -3), full, nil)
	latest := begin(4, now.AddDate(0, 0, -2), full)
	if err := collection.Fail(bg, p, latest, errors.New(strings.Repeat("x", 400))); err != nil {
		t.Fatal(err)
	}

	list, err := freshness.New(p, func() time.Time { return now }).List(bg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]freshness.SourceStatus{}
	for _, s := range list {
		got[s.Key] = s
	}
	if len(list) != 5 {
		t.Fatalf("got %d sources, want 5", len(list))
	}
	want := map[string]freshness.Status{"receita": freshness.Fresh, "tse": freshness.Stale,
		"camara": freshness.Never, "senado": freshness.Failed, "cvm": freshness.Never}
	for k, w := range want {
		if got[k].Status != w {
			t.Errorf("%s: status %q, want %q", k, got[k].Status, w)
		}
	}

	r := got["receita"]
	if r.LastReferenceDate == nil || !r.LastReferenceDate.Equal(now.AddDate(0, 0, -10)) {
		t.Errorf("receita last reference date = %v", r.LastReferenceDate)
	}
	if r.NextExpectedAt == nil || !r.NextExpectedAt.Equal(now.AddDate(0, 0, 20)) {
		t.Errorf("receita next expected = %v, want reference + 30 days", r.NextExpectedAt)
	}
	if r.LastCollectedAt == nil {
		t.Error("receita last collected at is nil")
	}
	if r.LastStats["rows_read"] != 3 {
		t.Errorf("receita last stats = %v, want those of the last done collection", r.LastStats)
	}
	if r.LastError != "" || r.Name == "" {
		t.Errorf("receita: %+v", r)
	}

	c := got["camara"]
	if c.LastReferenceDate != nil || c.NextExpectedAt != nil || c.LastCollectedAt != nil {
		t.Errorf("camara: %+v, want no dates", c)
	}
	if got["cvm"].LastError != "" {
		t.Errorf("cvm: error %q shown while the latest collection did not fail", got["cvm"].LastError)
	}
	if s := got["senado"]; len(s.LastError) != 400 || s.LastReferenceDate == nil {
		t.Errorf("senado: error of %d bytes, ref %v; want the full 400 bytes and the done reference", len(s.LastError), s.LastReferenceDate)
	}
}

// M10: an edition source is as current as its publication. Receita edition
// 2026-10 (reference date 2026-10-01) published on 2026-10-20 is fresh on
// 2026-11-20 (31 days after publication, 50 after the month's first day;
// stale after 45) and stale once 45 days after publication have passed.
func TestFreshnessCountsFromThePublicationDate(t *testing.T) {
	p := testutil.DB(t)
	testutil.SeedSources(t, p)
	c, err := collection.BeginObserved(bg, p, 1, day(2026, 10, 1), day(2026, 10, 20), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Finish(bg, p, c.ID, nil); err != nil {
		t.Fatal(err)
	}
	status := func(now time.Time) freshness.SourceStatus {
		t.Helper()
		list, err := freshness.New(p, func() time.Time { return now }).List(bg)
		if err != nil {
			t.Fatal(err)
		}
		return list[0]
	}
	st := status(day(2026, 11, 20))
	if st.Key != "receita" || st.Status != freshness.Fresh {
		t.Fatalf("%s on 2026-11-20: %s, want fresh", st.Key, st.Status)
	}
	if st.LastObservedDate == nil || !st.LastObservedDate.Equal(day(2026, 10, 20)) || !st.LastReferenceDate.Equal(day(2026, 10, 1)) {
		t.Errorf("observed %v reference %v", st.LastObservedDate, st.LastReferenceDate)
	}
	if st.NextExpectedAt == nil || !st.NextExpectedAt.Equal(day(2026, 10, 31)) {
		t.Errorf("next expected %v, want reference + 30 days", st.NextExpectedAt)
	}
	if st := status(day(2026, 12, 5)); st.Status != freshness.Stale {
		t.Fatalf("on 2026-12-05 (46 days after publication): %s, want stale", st.Status)
	}
}
