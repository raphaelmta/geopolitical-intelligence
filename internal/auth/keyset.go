package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// keySet is the oidc.KeySet behind the verifier. It replaces go-oidc's
// RemoteKeySet, which refetches the JWKS whenever no cached key verifies a
// token, so any unauthenticated client could make the API hit the identity
// provider once per forged token. Here:
//
//   - only the compact serialization is accepted, and the kid is read from
//     the protected header only;
//   - a token whose kid is cached but whose signature does not verify is
//     invalid, with no fetch;
//   - an unknown kid (or no kid and no cached key verifies) triggers a fetch,
//     shared by concurrent callers, but only if the last fetch attempt
//     (successful or not) is old enough: minRefetch, or emptyRetry while no
//     key was ever loaded. Otherwise the identity provider is reported
//     unavailable with the time left (a legitimately rotated key must not
//     log users out; tokens of cached keys never wait for it);
//   - keys older than maxAge are refreshed in the background (same sharing
//     and interval), so a key removed from the JWKS stops being trusted;
//   - a failed fetch keeps the old keys and reports the identity provider as
//     unavailable; keys that no fetch confirmed for maxStale are no longer
//     trusted (unavailable until a fetch succeeds).
//
// Only RS256/ES256 signing keys (RSA of at least 2048 bits, P-256) are kept.
type keySet struct {
	url        string
	client     *http.Client
	base       context.Context
	minRefetch time.Duration
	emptyRetry time.Duration
	maxAge     time.Duration
	maxStale   time.Duration
	now        func() time.Time
	onFetch    func()

	mu          sync.Mutex
	keys        []jose.JSONWebKey
	loadedAt    time.Time // last successful fetch
	lastAttempt time.Time
	lastErr     error
	flight      *fetchFlight
}

type fetchFlight struct {
	done chan struct{}
	err  error
	// background: started by a request that does not wait for it.
	background bool
}

// sigOutcome carries what VerifySignature found back to Verify through the
// context, since go-oidc turns key-set errors into plain strings.
type sigOutcome struct {
	reason       string
	unavailable  *unavailableError
	requestEnded bool
}

type outcomeKey struct{}

var allowedAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}

// errThrottled means a fetch was wanted but the refetch interval has not passed.
var errThrottled = errors.New("auth: key refetch throttled")

// errKeysTooOld means no fetch confirmed the cached keys for maxStale.
var errKeysTooOld = errors.New("auth: signing keys not confirmed by the identity provider for too long")

func (k *keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	out, _ := ctx.Value(outcomeKey{}).(*sigOutcome)
	if out == nil {
		out = &sigOutcome{}
	}
	fail := func(reason string) ([]byte, error) {
		out.reason = reason
		return nil, errors.New("auth: " + reason)
	}
	refreshFailed := func(err error) ([]byte, error) {
		if errors.Is(err, errRequestEnded) {
			out.requestEnded = true
			return nil, err
		}
		var ue *unavailableError
		if errors.As(err, &ue) {
			out.unavailable = ue
		}
		return nil, err
	}

	jws, err := jose.ParseSignedCompact(raw, allowedAlgs)
	if err != nil {
		return fail("malformed signature")
	}
	if len(jws.Signatures) != 1 {
		return fail("not exactly one signature")
	}
	kid := jws.Signatures[0].Protected.KeyID

	keys, age := k.cached()
	switch {
	case keys != nil && age > k.maxStale:
		// Too old to trust: only a successful fetch lets the token through.
		if err := k.refresh(ctx); err != nil {
			return refreshFailed(err)
		}
		if keys, _ = k.cached(); keys == nil {
			return refreshFailed(&unavailableError{retryAfter: k.minRefetch, cause: errKeysTooOld})
		}
	case keys != nil && age > k.maxAge:
		k.refreshInBackground()
	}

	payload, known := verifyWith(jws, keys, kid)
	if payload != nil {
		return payload, nil
	}
	if known && kid != "" {
		return fail("bad signature")
	}

	if err := k.refresh(ctx); err != nil {
		return refreshFailed(err)
	}
	keys, _ = k.cached()
	if payload, _ := verifyWith(jws, keys, kid); payload != nil {
		return payload, nil
	}
	return fail("unknown key id or bad signature")
}

// cached returns the cached keys (nil when none) and their age.
func (k *keySet) cached() ([]jose.JSONWebKey, time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.keys) == 0 {
		return nil, 0
	}
	return k.keys, k.now().Sub(k.loadedAt)
}

// verifyWith tries the keys matching kid (all keys when kid is empty). known
// reports whether any key matched.
func verifyWith(jws *jose.JSONWebSignature, keys []jose.JSONWebKey, kid string) (payload []byte, known bool) {
	for _, key := range keys {
		if kid != "" && key.KeyID != kid {
			continue
		}
		known = true
		if p, err := jws.Verify(key.Key); err == nil {
			return p, true
		}
	}
	return nil, known
}

// intervalLocked is the minimum time between two fetch attempts: short
// while no key was ever loaded (nothing can be verified until one is).
func (k *keySet) intervalLocked() time.Duration {
	if len(k.keys) == 0 {
		return k.emptyRetry
	}
	return k.minRefetch
}

// waitLocked is how long until the next fetch attempt is allowed (<= 0: now).
func (k *keySet) waitLocked(now time.Time) time.Duration {
	if k.lastAttempt.IsZero() {
		return 0
	}
	return k.intervalLocked() - now.Sub(k.lastAttempt)
}

func (k *keySet) startLocked(now time.Time, background bool) *fetchFlight {
	k.lastAttempt = now
	f := &fetchFlight{done: make(chan struct{}), background: background}
	k.flight = f
	go k.fetch(f)
	return f
}

// refresh fetches the key set unless the last attempt is too recent. Callers
// arriving while a fetch runs wait for it instead of starting another. Every
// failure is an *unavailableError (or errRequestEnded); warn is set only for
// the caller that ran the failed fetch.
func (k *keySet) refresh(ctx context.Context) error {
	k.mu.Lock()
	f := k.flight
	started := false
	if f == nil {
		now := k.now()
		if left := k.waitLocked(now); left > 0 {
			cause := k.lastErr
			k.mu.Unlock()
			if cause == nil {
				cause = errThrottled
			}
			return &unavailableError{retryAfter: left, cause: cause}
		}
		f = k.startLocked(now, false)
		started = true
	}
	k.mu.Unlock()

	select {
	case <-f.done:
		if f.err != nil {
			k.mu.Lock()
			retry := k.intervalLocked()
			k.mu.Unlock()
			return &unavailableError{retryAfter: retry, cause: f.err, warn: started}
		}
		return nil
	case <-ctx.Done():
		return errRequestEnded
	}
}

// refreshInBackground starts a fetch nobody waits for, if none runs and the
// interval allows one.
func (k *keySet) refreshInBackground() {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if k.flight != nil || k.waitLocked(now) > 0 {
		return
	}
	k.startLocked(now, true)
}

func (k *keySet) fetch(f *fetchFlight) {
	keys, err := k.load()
	k.mu.Lock()
	if err == nil {
		k.keys = keys // replaces the set: removed keys are no longer trusted
		k.loadedAt = k.now()
	}
	k.lastErr = err
	f.err = err
	k.flight = nil
	k.mu.Unlock()
	close(f.done)
	if err != nil && f.background {
		// No request reports a background failure: log it once here. The
		// cause comes from the IdP or the HTTP stack, never from a token.
		slog.Warn("auth: background refresh of the signing keys failed; keeping the cached keys", "error", err)
	}
	if k.onFetch != nil {
		k.onFetch()
	}
}

// load runs on base, not on a request context: the fetch is shared.
func (k *keySet) load() ([]jose.JSONWebKey, error) {
	req, err := http.NewRequestWithContext(k.base, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body) // bounded by the client's transport
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	var keys []jose.JSONWebKey
	for _, raw := range set.Keys {
		var key jose.JSONWebKey
		if err := key.UnmarshalJSON(raw); err != nil {
			continue // a key type we do not use (go-jose rejects unknown kty)
		}
		if usable(key) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks: no usable signing key")
	}
	return keys, nil
}

// usable keeps public RS256/ES256 signing keys of adequate strength.
func usable(key jose.JSONWebKey) bool {
	if !key.IsPublic() || (key.Use != "" && key.Use != "sig") {
		return false
	}
	switch pub := key.Key.(type) {
	case *rsa.PublicKey:
		return (key.Algorithm == "" || key.Algorithm == string(jose.RS256)) && pub.N.BitLen() >= 2048
	case *ecdsa.PublicKey:
		return (key.Algorithm == "" || key.Algorithm == string(jose.ES256)) && pub.Curve == elliptic.P256()
	}
	return false
}
