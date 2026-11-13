package freshness

// SetLongRunsSQL replaces the long-run query for a test; the returned
// function restores it.
func SetLongRunsSQL(q string) (restore func()) {
	old := longRunsSQL
	longRunsSQL = q
	return func() { longRunsSQL = old }
}
