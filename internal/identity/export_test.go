package identity

import "testing"

// SetNamesPerBatch makes Resolve use batches of n names until the test ends.
func SetNamesPerBatch(t *testing.T, n int) {
	old := namesPerBatch
	namesPerBatch = n
	t.Cleanup(func() { namesPerBatch = old })
}

// SetAfterBatch runs fn after each committed Resolve batch until the test ends.
func SetAfterBatch(t *testing.T, fn func()) {
	afterBatch = fn
	t.Cleanup(func() { afterBatch = nil })
}

// SetBeforeCollection runs fn before each collection is resolved until the test ends.
func SetBeforeCollection(t *testing.T, fn func(collectionID int64) error) {
	beforeCollection = fn
	t.Cleanup(func() { beforeCollection = nil })
}
