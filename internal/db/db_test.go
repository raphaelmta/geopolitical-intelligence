package db_test

import (
	"context"
	"sync"
	"testing"

	"geoint/internal/db"
	"geoint/internal/testutil"
)

func TestConcurrentMigrateIsSafe(t *testing.T) {
	u := testutil.EmptySchemaURL(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = db.Migrate(context.Background(), u) }()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("migrate %d: %v", i, err)
		}
	}
}
