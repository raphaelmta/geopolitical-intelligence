package search

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewForTest returns a Service with a frequent-token threshold and a
// generation check interval chosen by the test.
func NewForTest(p *pgxpool.Pool, frequentThreshold int64, checkEvery time.Duration) *Service {
	s := New(p)
	s.frequentThreshold = frequentThreshold
	s.checkEvery = checkEvery
	return s
}

// SetAfterLock makes Rebuild call f right after taking the rebuild lock.
func (s *Service) SetAfterLock(f func()) { s.afterLock = f }

// SetBeforeSwap makes Rebuild call f before each swap attempt (1, 2, ...).
func (s *Service) SetBeforeSwap(f func(attempt int)) { s.beforeSwap = f }

// SetSwapRetry sets the swap's lock timeout, number of attempts and first
// backoff.
func (s *Service) SetSwapRetry(lockTimeout time.Duration, attempts int, backoff time.Duration) {
	s.swapLockTimeout, s.swapAttempts, s.swapBackoff = lockTimeout, attempts, backoff
}
