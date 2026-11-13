// Package auth verifies OIDC access tokens issued by the identity provider
// (Keycloak) and carries the authenticated user through request contexts.
//
// Login itself never happens here: the browser signs in at the identity
// provider (authorization code + PKCE) and sends the access token as
// "Authorization: Bearer <token>". This package only checks that token.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/sync/singleflight"
)

// User is the authenticated caller, taken from a verified access token.
type User struct {
	Sub   string
	Name  string
	Roles []string
}

// Has reports whether u holds role (exact, case-sensitive match). Role
// hierarchy (admin includes analyst) lives in the realm as a composite role,
// not here.
func (u User) Has(role string) bool {
	return role != "" && slices.Contains(u.Roles, role)
}

// Verifier checks a raw access token and returns its user.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (User, error)
}

// ErrUnavailable means the identity provider could not be reached (or answered
// with an unusable document), so the token could not be checked. It is not the
// caller's fault: the middleware answers 503, not 401. Errors returned by this
// package's verifier match it with errors.Is.
var ErrUnavailable = errors.New("auth: identity provider unavailable")

// unavailableError is an ErrUnavailable that knows when retrying makes sense.
// warn: this caller ran the attempt that failed (the middleware logs it at
// Warn); answers from a remembered failure or a throttled refetch log at
// Debug, so unauthenticated clients cannot flood the log.
type unavailableError struct {
	retryAfter time.Duration
	cause      error
	warn       bool
}

func (e *unavailableError) Error() string        { return ErrUnavailable.Error() + ": " + e.cause.Error() }
func (e *unavailableError) Is(target error) bool { return target == ErrUnavailable }
func (e *unavailableError) Unwrap() error        { return e.cause }

// invalidError is a rejected token. reason is a fixed, non-sensitive phrase:
// it never contains token or claim text, so it is safe to log.
type invalidError struct{ reason string }

func (e *invalidError) Error() string { return "auth: invalid token: " + e.reason }

func invalid(reason string) error { return &invalidError{reason: reason} }

// errRequestEnded means the caller's own context ended while waiting for the
// identity provider; it says nothing about the identity provider.
var errRequestEnded = errors.New("auth: request ended while waiting for the identity provider")

const (
	httpTimeout = 10 * time.Second
	// maxIdPResponse bounds discovery and JWKS bodies read from the IdP.
	maxIdPResponse = 1 << 20
	// maxAuthorizationHeader bounds the Authorization header the middleware accepts.
	maxAuthorizationHeader = 8 * 1024
	// defaultMinRefetch is the minimum time between two JWKS fetch attempts.
	defaultMinRefetch = 30 * time.Second
	// defaultDiscoveryCooldown is how long a failed discovery is answered from
	// cache before the next attempt.
	defaultDiscoveryCooldown = 3 * time.Second
	// defaultMaxKeyAge is the age after which cached signing keys are
	// refreshed in the background, so a key the identity provider removed
	// stops being trusted.
	defaultMaxKeyAge = 15 * time.Minute
	// defaultMaxStaleKeyAge is how long keys are trusted without a successful
	// refresh before the verifier answers unavailable instead.
	defaultMaxStaleKeyAge = 24 * time.Hour
)

// supportedAlgs is the closed list of accepted signature algorithms; HS* and
// "none" are never accepted, whatever the discovery document says.
var supportedAlgs = []string{oidc.RS256, oidc.ES256}

type options struct {
	minRefetch        time.Duration
	discoveryCooldown time.Duration
	// maxKeyAge: cached keys older than this are refreshed in the background.
	maxKeyAge time.Duration
	// maxStaleKeyAge: keys not refreshed successfully for this long are no
	// longer trusted (ErrUnavailable until a refresh succeeds).
	maxStaleKeyAge time.Duration
	now            func() time.Time
	// onFetch, when set, is called after every JWKS fetch ends (tests).
	onFetch func()
}

// NewOIDCVerifier returns a Verifier for access tokens whose iss is exactly
// issuer and whose aud contains audience. Discovery and keys are fetched from
// issuer itself. See NewOIDCVerifierWithDiscovery.
func NewOIDCVerifier(ctx context.Context, issuer, audience string) (Verifier, error) {
	return NewOIDCVerifierWithDiscovery(ctx, issuer, issuer, audience)
}

// NewOIDCVerifierWithDiscovery is NewOIDCVerifier with the discovery document
// (and therefore the JWKS) fetched from discoveryURL, an address of the same
// issuer that is reachable from this process (for example the identity
// provider's name inside the container network), while tokens must still carry
// iss == issuer (the public address the browser uses). The discovery document
// must announce exactly issuer, and its jwks_uri must be on the same origin as
// discoveryURL. Signing keys travel over discoveryURL: use https, or a private
// network you trust.
//
// Nothing is fetched here: the first Verify runs discovery, so the API can
// start while the identity provider is still down. A failed discovery returns
// ErrUnavailable and is answered from cache for a short cooldown; concurrent
// callers share one attempt. Signing keys are fetched at most once per refetch
// interval (every 3 s while none was ever loaded), for a key id not seen
// before or in the background once the cached keys are 15 minutes old; keys
// no fetch confirmed for 24 hours are not trusted. ctx bounds the verifier's
// lifetime (key fetches use it), so it must outlive every request, not be a
// request context.
func NewOIDCVerifierWithDiscovery(ctx context.Context, issuer, discoveryURL, audience string) (Verifier, error) {
	return newOIDCVerifier(ctx, issuer, discoveryURL, audience, options{
		minRefetch:        defaultMinRefetch,
		discoveryCooldown: defaultDiscoveryCooldown,
		maxKeyAge:         defaultMaxKeyAge,
		maxStaleKeyAge:    defaultMaxStaleKeyAge,
		now:               time.Now,
	})
}

func newOIDCVerifier(ctx context.Context, issuer, discoveryURL, audience string, o options) (*oidcVerifier, error) {
	if err := checkURL(issuer); err != nil {
		return nil, fmt.Errorf("auth: issuer: %w", err)
	}
	if err := checkURL(discoveryURL); err != nil {
		return nil, fmt.Errorf("auth: discovery url: %w", err)
	}
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("auth: audience is required")
	}
	client := newHTTPClient()
	return &oidcVerifier{
		base:         oidc.ClientContext(ctx, client),
		client:       client,
		issuer:       issuer,
		discoveryURL: discoveryURL,
		audience:     audience,
		opts:         o,
	}, nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return errors.New("must be a plain http(s) URL with a host and no credentials, query or fragment")
	}
	return nil
}

// newHTTPClient is the client for discovery and JWKS: bounded time, no proxy
// taken from the environment, no redirects, bounded response bodies.
func newHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: httpTimeout}).DialContext,
		TLSHandshakeTimeout:   httpTimeout,
		ResponseHeaderTimeout: httpTimeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Timeout:   httpTimeout,
		Transport: limitedTransport{tr},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type limitedTransport struct{ next http.RoundTripper }

func (t limitedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBody{rc: resp.Body, left: maxIdPResponse}
	return resp, nil
}

// limitedBody fails, rather than truncates, a body longer than its limit.
type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// Probe for one more byte to tell "exactly at the limit" from "over it".
		var one [1]byte
		if n, _ := b.rc.Read(one[:]); n > 0 {
			return 0, errors.New("auth: identity provider response too large")
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

type oidcVerifier struct {
	base         context.Context
	client       *http.Client
	issuer       string
	discoveryURL string
	audience     string
	opts         options

	init  singleflight.Group
	ready atomic.Pointer[oidc.IDTokenVerifier]

	mu       sync.Mutex
	failedAt time.Time // last failed discovery, for the cooldown
	failErr  error
}

func (v *oidcVerifier) Verify(ctx context.Context, rawToken string) (User, error) {
	idv, err := v.verifier(ctx)
	if err != nil {
		return User{}, err
	}
	// go-oidc flattens key-set errors into strings; the key set reports what
	// happened through this per-call outcome instead.
	out := &sigOutcome{}
	tok, err := idv.Verify(context.WithValue(ctx, outcomeKey{}, out), rawToken)
	if err != nil {
		switch {
		case out.unavailable != nil:
			return User{}, out.unavailable
		case out.requestEnded || ctx.Err() != nil:
			return User{}, errRequestEnded
		case out.reason != "":
			return User{}, invalid(out.reason)
		}
		var expired *oidc.TokenExpiredError
		if errors.As(err, &expired) {
			return User{}, invalid("expired")
		}
		return User{}, invalid("format, issuer, audience or validity period rejected")
	}
	var c struct {
		Sub               string   `json:"sub"`
		Typ               *string  `json:"typ"`
		Name              string   `json:"name"`
		PreferredUsername string   `json:"preferred_username"`
		Roles             []string `json:"roles"`
	}
	if err := tok.Claims(&c); err != nil {
		return User{}, invalid("claims of the wrong type")
	}
	if c.Sub == "" {
		return User{}, invalid("missing sub")
	}
	// Keycloak marks its tokens with typ: an ID or refresh token signed by the
	// same keys must never pass as an access token.
	if c.Typ != nil && *c.Typ != "Bearer" {
		return User{}, invalid("not an access token (typ)")
	}
	name := c.Name
	if name == "" {
		name = c.PreferredUsername
	}
	return User{Sub: c.Sub, Name: name, Roles: c.Roles}, nil
}

// verifier returns the go-oidc verifier, running discovery on first use.
// Concurrent first callers share one discovery attempt. A failed attempt is
// remembered for the cooldown, during which callers get ErrUnavailable at
// once; after it the next request retries.
func (v *oidcVerifier) verifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	if idv := v.ready.Load(); idv != nil {
		return idv, nil
	}
	v.mu.Lock()
	if !v.failedAt.IsZero() {
		if left := v.opts.discoveryCooldown - v.opts.now().Sub(v.failedAt); left > 0 {
			err := v.failErr
			v.mu.Unlock()
			return nil, &unavailableError{retryAfter: left, cause: err}
		}
	}
	v.mu.Unlock()

	ran := false // set only by the caller whose function runs the attempt
	ch := v.init.DoChan("discovery", func() (any, error) {
		ran = true
		if idv := v.ready.Load(); idv != nil {
			return idv, nil
		}
		idv, err := v.discover()
		if err != nil {
			v.mu.Lock()
			v.failedAt, v.failErr = v.opts.now(), err
			v.mu.Unlock()
			return nil, &unavailableError{retryAfter: v.opts.discoveryCooldown, cause: err}
		}
		v.ready.Store(idv)
		return idv, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			var ue *unavailableError
			if errors.As(res.Err, &ue) {
				mine := *ue // shared by every caller: each gets its own copy
				mine.warn = ran
				return nil, &mine
			}
			return nil, res.Err
		}
		return res.Val.(*oidc.IDTokenVerifier), nil
	case <-ctx.Done():
		// The shared attempt goes on (on base) for the next caller.
		return nil, errRequestEnded
	}
}

// discover runs on base, not on a request context: the attempt is shared and
// the key set keeps fetching with it later.
func (v *oidcVerifier) discover() (*oidc.IDTokenVerifier, error) {
	// InsecureIssuerURLContext only lifts go-oidc's "discovery URL == issuer"
	// check, needed when discovery goes through an internal address; the
	// announced issuer is checked against the configured one right below.
	p, err := oidc.NewProvider(oidc.InsecureIssuerURLContext(v.base, v.issuer), v.discoveryURL)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := p.Claims(&doc); err != nil {
		return nil, fmt.Errorf("discovery document: %w", err)
	}
	if doc.Issuer != v.issuer {
		return nil, fmt.Errorf("discovery announces issuer %q, want %q", doc.Issuer, v.issuer)
	}
	if err := sameOrigin(doc.JWKSURI, v.discoveryURL); err != nil {
		return nil, fmt.Errorf("jwks_uri: %w", err)
	}
	keys := &keySet{
		url:        doc.JWKSURI,
		client:     v.client,
		base:       v.base,
		minRefetch: v.opts.minRefetch,
		emptyRetry: v.opts.discoveryCooldown,
		maxAge:     v.opts.maxKeyAge,
		maxStale:   v.opts.maxStaleKeyAge,
		now:        v.opts.now,
		onFetch:    v.opts.onFetch,
	}
	return oidc.NewVerifier(v.issuer, keys, &oidc.Config{
		ClientID:             v.audience, // checked against aud
		SupportedSigningAlgs: supportedAlgs,
	}), nil
}

func sameOrigin(raw, ref string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return err
	}
	if u.Scheme != r.Scheme || u.Host != r.Host || u.User != nil {
		return fmt.Errorf("%q is not on the origin of %q", raw, ref)
	}
	return nil
}

type ctxKey struct{}

// FromContext returns the user Middleware stored in ctx.
func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// Middleware requires a valid bearer access token on every request and puts
// its user in the request context. The token is read only from the
// Authorization header (never from the query string or cookies).
//
// Missing, malformed or rejected tokens get 401 {"error":"unauthorized"};
// an unreachable identity provider gets 503 {"error":"auth_unavailable"} with
// Retry-After, and so does a token signed by a key id not seen yet while the
// key refetch is throttled (it may be a newly rotated key). Neither the token nor claim text is sent to the client or
// logged; rejections are logged at debug level with a fixed reason.
func Middleware(v Verifier) func(http.Handler) http.Handler {
	if v == nil {
		panic("auth: Middleware needs a Verifier")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			raw, reason := bearerToken(r)
			if reason != "" {
				slog.DebugContext(ctx, "auth: request rejected", "reason", reason)
				unauthorized(w)
				return
			}
			u, err := v.Verify(ctx, raw)
			switch {
			case err == nil:
				next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKey{}, u)))
			case errors.Is(err, errRequestEnded) || ctx.Err() != nil:
				// The client went away (or its deadline passed) while we waited
				// for the identity provider: nothing to report about the IdP.
				slog.DebugContext(ctx, "auth: request ended while waiting for the identity provider")
				unavailable(w, defaultDiscoveryCooldown)
			case errors.Is(err, ErrUnavailable):
				retry := defaultDiscoveryCooldown
				var ue *unavailableError
				isUE := errors.As(err, &ue)
				if isUE && ue.retryAfter > 0 {
					retry = ue.retryAfter
				}
				// The cause comes from the IdP or the HTTP stack, not from the
				// token. Only the request that ran the failed attempt warns.
				if isUE && !ue.warn {
					slog.DebugContext(ctx, "auth: identity provider unavailable (remembered or throttled)", "error", err)
				} else {
					slog.WarnContext(ctx, "auth: identity provider unavailable", "error", err)
				}
				unavailable(w, retry)
			default:
				reason := "rejected by verifier"
				var ie *invalidError
				if errors.As(err, &ie) {
					reason = ie.reason
				}
				slog.DebugContext(ctx, "auth: token rejected", "reason", reason)
				unauthorized(w)
			}
		})
	}
}

// bearerToken extracts the token from exactly one "Bearer <token>" header,
// with the scheme matched case-insensitively. It returns a reason when there
// is no usable token; the reason never contains the header value.
func bearerToken(r *http.Request) (string, string) {
	values := r.Header.Values("Authorization")
	switch {
	case len(values) == 0:
		return "", "no authorization header"
	case len(values) > 1:
		return "", "several authorization headers"
	case len(values[0]) > maxAuthorizationHeader:
		return "", "authorization header too long"
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", "not a bearer authorization"
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", "malformed bearer token"
	}
	return token, ""
}

// RequireRole answers 403 unless the user Middleware stored holds role, and
// 401 when there is no user at all (Middleware missing in front of it).
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := FromContext(r.Context())
			if !ok {
				slog.ErrorContext(r.Context(), "auth: RequireRole used without Middleware", "role", role)
				unauthorized(w)
				return
			}
			if !u.Has(role) {
				slog.InfoContext(r.Context(), "auth: role missing", "sub", u.Sub, "role", role)
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

func unavailable(w http.ResponseWriter, retry time.Duration) {
	secs := max(1, int(math.Ceil(retry.Seconds())))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusServiceUnavailable, "auth_unavailable")
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
