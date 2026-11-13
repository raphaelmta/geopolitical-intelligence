package sources_test

import (
	"context"
	"testing"
	"time"

	"geoint/internal/sources"
	"geoint/internal/testutil"
)

func TestByKey(t *testing.T) {
	s, ok := sources.ByKey("receita")
	if !ok || s.ID != 1 || !s.Snapshot || s.StaleAfter != 45*24*time.Hour || !s.HasConnector {
		t.Fatalf("got %+v ok=%v", s, ok)
	}
	for _, src := range sources.All {
		if !src.HasConnector {
			t.Errorf("%s: HasConnector false; every catalogued source has a connector", src.Key)
		}
		if src.SupportsSlice != (src.Key == "receita") {
			t.Errorf("%s: SupportsSlice %v; only receita collects slices", src.Key, src.SupportsSlice)
		}
	}
	// TSE republishes the accounts of an open election daily: a week's
	// cadence, stale after a month (not half a year).
	if tse, _ := sources.ByKey("tse"); tse.PublishEvery != 7*24*time.Hour || tse.StaleAfter != 30*24*time.Hour {
		t.Errorf("tse: publish every %v, stale after %v; want 7 and 30 days", tse.PublishEvery, tse.StaleAfter)
	}
	for _, src := range sources.All {
		if src.Editions != (src.Key == "receita") {
			t.Errorf("%s: Editions %v; only receita is published as editions", src.Key, src.Editions)
		}
	}
	if _, ok := sources.ByKey("nope"); ok {
		t.Fatal("unknown key found")
	}
}

func TestSyncIsIdempotentAndStoresIntervals(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := sources.Sync(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM source").Scan(&n); err != nil || n != len(sources.All) {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	var stale time.Duration
	var snap bool
	var pub time.Duration
	if err := pool.QueryRow(ctx, "SELECT publish_every, stale_after, snapshot FROM source WHERE key='cvm'").Scan(&pub, &stale, &snap); err != nil {
		t.Fatal(err)
	}
	if pub != 7*24*time.Hour || stale != 30*24*time.Hour || !snap {
		t.Fatalf("pub=%v stale=%v snap=%v", pub, stale, snap)
	}
	var editions bool
	if err := pool.QueryRow(ctx, "SELECT editions FROM source WHERE key='receita'").Scan(&editions); err != nil || !editions {
		t.Fatalf("receita editions = %v, %v", editions, err)
	}
}
