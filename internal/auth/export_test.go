package auth

import (
	"context"
	"time"
)

// NewOIDCVerifierWithOptions exposes the test-only knobs: refetch interval,
// discovery cooldown and clock (key ages at their defaults).
func NewOIDCVerifierWithOptions(ctx context.Context, issuer, discoveryURL, audience string,
	minRefetch, cooldown time.Duration, now func() time.Time) (Verifier, error) {
	return NewOIDCVerifierForTest(ctx, issuer, discoveryURL, audience, TestOptions{
		MinRefetch: minRefetch, Cooldown: cooldown, Now: now,
	})
}

// TestOptions are every knob of the verifier; zero durations mean the defaults.
type TestOptions struct {
	MinRefetch, Cooldown, MaxKeyAge, MaxStaleKeyAge time.Duration
	Now                                             func() time.Time
	// OnFetch is called after every JWKS fetch ends.
	OnFetch func()
}

// NewOIDCVerifierForTest builds a verifier with o.
func NewOIDCVerifierForTest(ctx context.Context, issuer, discoveryURL, audience string, o TestOptions) (Verifier, error) {
	or := func(d, def time.Duration) time.Duration {
		if d == 0 {
			return def
		}
		return d
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return newOIDCVerifier(ctx, issuer, discoveryURL, audience, options{
		minRefetch:        or(o.MinRefetch, defaultMinRefetch),
		discoveryCooldown: or(o.Cooldown, defaultDiscoveryCooldown),
		maxKeyAge:         or(o.MaxKeyAge, defaultMaxKeyAge),
		maxStaleKeyAge:    or(o.MaxStaleKeyAge, defaultMaxStaleKeyAge),
		now:               now,
		onFetch:           o.OnFetch,
	})
}
