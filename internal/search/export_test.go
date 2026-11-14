package search

import (
	"context"
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

// ExtractCPF exposes extractCPF: the fragment, the rest of the query and
// whether the query carries a CPF.
func ExtractCPF(q string) (fragment, rest string, ok bool, err error) {
	c, ok, err := extractCPF(q)
	return c.fragment, c.rest, ok, err
}

// ValidCPF exposes validCPF.
func ValidCPF(digits string) bool { return validCPF(digits) }

// FragmentSQL is the query of a CPF-fragment search ($1 fragment, $2 tsquery
// or ”, $3 limit).
const FragmentSQL = fragmentSQL

// BuildVersionInputs returns the text the build version is a digest of.
func BuildVersionInputs(ctx context.Context, p *pgxpool.Pool) (string, error) {
	var in string
	err := p.QueryRow(ctx, versionInputsSQL, buildStatementsDigest).Scan(&in)
	return in, err
}
