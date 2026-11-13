package auth_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"geoint/internal/auth"
)

const audience = "geoint-api"

// fakeIdP is an OIDC issuer on httptest: discovery plus JWKS, with keys the
// test can rotate, switches that make discovery or JWKS answer 503, request
// counters and a fake clock for the verifiers built on it.
type fakeIdP struct {
	srv    *httptest.Server
	issuer string // the issuer the discovery document announces
	clock  *clock

	mu             sync.Mutex
	keys           []jose.JSONWebKey
	down           bool // discovery and JWKS answer 503
	jwksDown       bool // only JWKS answers 503
	discoveryHits  int
	jwksHits       int
	discoveryDelay time.Duration
	jwksDelay      time.Duration
	jwksURI        string // overrides the announced jwks_uri
	padding        int    // bytes of filler added to discovery and JWKS bodies
}

// clock is the fake time the verifiers use for refetch intervals and cooldowns.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const (
	minRefetch = 30 * time.Second
	cooldown   = 3 * time.Second
)

type testKey struct {
	priv *rsa.PrivateKey
	kid  string
}

func newKey(t *testing.T, kid string) testKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{priv: priv, kid: kid}
}

func (k testKey) public() jose.JSONWebKey {
	return jose.JSONWebKey{Key: &k.priv.PublicKey, KeyID: k.kid, Algorithm: string(jose.RS256), Use: "sig"}
}

// newFakeIdP starts an issuer whose announced issuer is its own URL unless
// publicIssuer is set (the issuer reached through an internal address).
func newFakeIdP(t *testing.T, publicIssuer string, keys ...testKey) *fakeIdP {
	t.Helper()
	f := &fakeIdP{clock: &clock{t: time.Now()}}
	for _, k := range keys {
		f.keys = append(f.keys, k.public())
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.issuer = f.srv.URL
	if publicIssuer != "" {
		f.issuer = publicIssuer
	}
	return f
}

func (f *fakeIdP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down, keys := f.down, append([]jose.JSONWebKey(nil), f.keys...)
	jwksURI, padding := f.jwksURI, strings.Repeat("x", f.padding)
	var delay time.Duration
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		f.discoveryHits++
		delay = f.discoveryDelay
	case "/keys":
		f.jwksHits++
		delay = f.jwksDelay
		down = down || f.jwksDown
	}
	f.mu.Unlock()
	time.Sleep(delay)
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if jwksURI == "" {
		jwksURI = f.srv.URL + "/keys"
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.issuer,
			"jwks_uri":                              jwksURI,
			"authorization_endpoint":                f.srv.URL + "/auth",
			"token_endpoint":                        f.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"x-padding":                             padding,
		})
	case "/keys":
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys, "x-padding": padding})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeIdP) setKeys(keys ...testKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = nil
	for _, k := range keys {
		f.keys = append(f.keys, k.public())
	}
}

// setJWKS publishes exactly these keys (any type).
func (f *fakeIdP) setJWKS(keys ...jose.JSONWebKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

func (f *fakeIdP) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// set runs fn with f locked, to change the other switches.
func (f *fakeIdP) set(fn func(f *fakeIdP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeIdP) hits() (discovery, jwks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discoveryHits, f.jwksHits
}

// claims returns a valid Keycloak-like access token claim set for f.
func (f *fakeIdP) claims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   f.issuer,
		"sub":   "user-1",
		"aud":   []string{"account", audience},
		"exp":   now.Add(5 * time.Minute).Unix(),
		"iat":   now.Unix(),
		"typ":   "Bearer",
		"azp":   "geoint-web",
		"name":  "Ana Analista",
		"roles": []string{"analyst"},
	}
}

func sign(t *testing.T, k testKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k.priv, KeyID: k.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func b64(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// newVerifier builds a verifier on f with the production intervals and f's
// fake clock, so tests move time instead of sleeping.
func newVerifier(t *testing.T, f *fakeIdP) auth.Verifier {
	t.Helper()
	v, err := auth.NewOIDCVerifierWithOptions(t.Context(), f.issuer, f.srv.URL, audience, minRefetch, cooldown, f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// stubVerifier is a Verifier with a fixed answer.
type stubVerifier struct {
	user  auth.User
	err   error
	calls int
}

func (s *stubVerifier) Verify(context.Context, string) (auth.User, error) {
	s.calls++
	return s.user, s.err
}

// echoUser answers 200 with the user the middleware put in the context.
var echoUser = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "no user in context", http.StatusTeapot)
		return
	}
	_ = json.NewEncoder(w).Encode(u)
})

// do runs one request through h with the given Authorization header values.
func do(h http.Handler, target string, authz ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for _, a := range authz {
		req.Header.Add("Authorization", a)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func protected(v auth.Verifier) http.Handler { return auth.Middleware(v)(echoUser) }

func adminOnly(v auth.Verifier) http.Handler {
	return auth.Middleware(v)(auth.RequireRole("admin")(echoUser))
}

func assertUnauthorized(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"unauthorized"}` {
		t.Errorf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if wa := rec.Header().Get("WWW-Authenticate"); wa != "Bearer" {
		t.Errorf("WWW-Authenticate = %q", wa)
	}
}

func decodeUser(t *testing.T, rec *httptest.ResponseRecorder) auth.User {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var u auth.User
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestMissingTokenIs401(t *testing.T) {
	s := &stubVerifier{user: auth.User{Sub: "x"}}
	assertUnauthorized(t, do(protected(s), "/"))
	if s.calls != 0 {
		t.Errorf("verifier called %d times without a token", s.calls)
	}
}

func TestInvalidTokenIs401NotServerError(t *testing.T) {
	s := &stubVerifier{err: errors.New("oidc: secret detail token-abc")}
	rec := do(protected(s), "/", "Bearer token-abc")
	assertUnauthorized(t, rec)
	if strings.Contains(rec.Body.String(), "token-abc") || strings.Contains(rec.Body.String(), "detail") {
		t.Errorf("response leaks verification details: %q", rec.Body.String())
	}
}

func TestMalformedAuthorizationIs401(t *testing.T) {
	s := &stubVerifier{user: auth.User{Sub: "x"}}
	cases := map[string][]string{
		"basic scheme":      {"Basic dXNlcjpwYXNz"},
		"scheme only":       {"Bearer"},
		"empty token":       {"Bearer "},
		"two tokens":        {"Bearer a b"},
		"two spaces":        {"Bearer  a"},
		"tab separated":     {"Bearer\ta"},
		"two headers":       {"Bearer a", "Bearer b"},
		"oversized header":  {"Bearer " + strings.Repeat("a", 8*1024)},
		"no space":          {"Bearera"},
		"token with tab":    {"Bearer a\tb"},
		"prefix of scheme":  {"Bear a"},
		"scheme with colon": {"Bearer: a"},
	}
	for name, hdrs := range cases {
		t.Run(name, func(t *testing.T) {
			assertUnauthorized(t, do(protected(s), "/", hdrs...))
		})
	}
	if s.calls != 0 {
		t.Errorf("verifier called %d times for malformed headers", s.calls)
	}
}

func TestVerifierUnavailableIs503(t *testing.T) {
	s := &stubVerifier{err: auth.ErrUnavailable}
	rec := do(protected(s), "/", "Bearer a")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"auth_unavailable"}` {
		t.Errorf("body = %q", got)
	}
}

func TestValidTokenPutsUserInContext(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	u := decodeUser(t, do(protected(newVerifier(t, f)), "/", "Bearer "+sign(t, k, f.claims())))
	if u.Sub != "user-1" || u.Name != "Ana Analista" || len(u.Roles) != 1 || u.Roles[0] != "analyst" {
		t.Errorf("user = %+v", u)
	}
}

func TestNameFallsBackToPreferredUsername(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	c := f.claims()
	delete(c, "name")
	c["preferred_username"] = "ana"
	u := decodeUser(t, do(protected(newVerifier(t, f)), "/", "Bearer "+sign(t, k, c)))
	if u.Name != "ana" {
		t.Errorf("name = %q, want preferred_username", u.Name)
	}
}

func TestLowercaseBearerAccepted(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	decodeUser(t, do(protected(newVerifier(t, f)), "/", "bearer "+sign(t, k, f.claims())))
}

func TestRequireRoleForbidsOtherRoles(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	v := newVerifier(t, f)

	rec := do(adminOnly(v), "/", "Bearer "+sign(t, k, f.claims()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("analyst on admin route: status = %d, want 403", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"forbidden"}` {
		t.Errorf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	c := f.claims()
	c["roles"] = []string{"analyst", "admin"}
	decodeUser(t, do(adminOnly(v), "/", "Bearer "+sign(t, k, c)))
}

func TestRequireRoleWithoutMiddlewareIs401(t *testing.T) {
	assertUnauthorized(t, do(auth.RequireRole("analyst")(echoUser), "/"))
}

func TestMissingRolesClaimMeansNoRoles(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	v := newVerifier(t, f)
	c := f.claims()
	delete(c, "roles")
	tok := "Bearer " + sign(t, k, c)

	if u := decodeUser(t, do(protected(v), "/", tok)); len(u.Roles) != 0 {
		t.Errorf("roles = %v, want none", u.Roles)
	}
	h := auth.Middleware(v)(auth.RequireRole("analyst")(echoUser))
	if rec := do(h, "/", tok); rec.Code != http.StatusForbidden {
		t.Errorf("role route: status = %d, want 403", rec.Code)
	}
}

func TestUserHas(t *testing.T) {
	u := auth.User{Roles: []string{"analyst", "custom"}}
	if !u.Has("analyst") || !u.Has("custom") || u.Has("admin") || u.Has("") || u.Has("Analyst") {
		t.Errorf("Has is wrong for %v", u.Roles)
	}
}

// Every token below must be rejected with 401 by the real OIDC verifier.
func TestRejectedTokensAre401(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	v := newVerifier(t, f)
	other := newKey(t, "k1") // same kid, different key

	with := func(mut func(map[string]any)) string {
		c := f.claims()
		mut(c)
		return sign(t, k, c)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&k.priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hsInput := b64(map[string]string{"alg": "HS256", "typ": "JWT", "kid": "k1"}) + "." + b64(f.claims())
	mac := hmac.New(sha256.New, pubDER)
	mac.Write([]byte(hsInput))
	hs256 := hsInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	cases := map[string]string{
		"audience missing":                with(func(c map[string]any) { delete(c, "aud") }),
		"wrong issuer":                    with(func(c map[string]any) { c["iss"] = "http://evil.example/realms/geoint" }),
		"missing sub":                     with(func(c map[string]any) { delete(c, "sub") }),
		"empty sub":                       with(func(c map[string]any) { c["sub"] = "" }),
		"ID token":                        with(func(c map[string]any) { c["typ"] = "ID" }),
		"refresh token":                   with(func(c map[string]any) { c["typ"] = "Refresh" }),
		"nbf in the future":               with(func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }),
		"roles not a list":                with(func(c map[string]any) { c["roles"] = "admin" }),
		"other key":                       sign(t, other, f.claims()),
		"alg none":                        b64(map[string]string{"alg": "none", "typ": "JWT"}) + "." + b64(f.claims()) + ".",
		"HS256 with public key as secret": hs256,
		"garbage":                         "not-a-jwt",
		"tampered payload": func() string {
			parts := strings.Split(sign(t, k, f.claims()), ".")
			c := f.claims()
			c["roles"] = []string{"admin"}
			return parts[0] + "." + b64(c) + "." + parts[2]
		}(),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			assertUnauthorized(t, do(protected(v), "/", "Bearer "+tok))
		})
	}
}

func TestTokenInQueryStringIgnored(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	tok := sign(t, k, f.claims())
	assertUnauthorized(t, do(protected(newVerifier(t, f)), "/?access_token="+tok))
}

func TestExpiredTokenIs401(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	c := f.claims()
	c["exp"] = time.Now().Add(-time.Minute).Unix()
	assertUnauthorized(t, do(protected(newVerifier(t, f)), "/", "Bearer "+sign(t, k, c)))
}

func TestWrongAudienceIs401(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	c := f.claims()
	c["aud"] = []string{"account", "other-api"}
	assertUnauthorized(t, do(protected(newVerifier(t, f)), "/", "Bearer "+sign(t, k, c)))
}

func TestKeyRotation(t *testing.T) {
	k1, k2, k3 := newKey(t, "k1"), newKey(t, "k2"), newKey(t, "k3")
	f := newFakeIdP(t, "", k1)
	v := newVerifier(t, f)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())))

	// The IdP publishes k2. Within the refetch interval an unknown kid is not
	// fetched: 503 with Retry-After (a rotated key must not log the user
	// out); once the interval has passed it is fetched.
	f.setKeys(k1, k2)
	assertThrottled(t, do(protected(v), "/", "Bearer "+sign(t, k2, f.claims())), "30")
	f.clock.Advance(minRefetch)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k2, f.claims())))

	// k1 is retired. Until the next refetch the cached k1 still verifies.
	f.setKeys(k2)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())))

	// An unknown kid after the interval triggers a refetch, which drops k1;
	// from then on a k1 token is refused: unavailable while the refetch is
	// throttled, unauthorized once a refetch confirms the key is gone.
	f.clock.Advance(minRefetch)
	assertUnauthorized(t, do(protected(v), "/", "Bearer "+sign(t, k3, f.claims())))
	assertThrottled(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())), "30")
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k2, f.claims())))
	f.clock.Advance(minRefetch)
	assertUnauthorized(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())))
}

// assertThrottled checks a 503 auth_unavailable with that Retry-After.
func assertThrottled(t *testing.T, rec *httptest.ResponseRecorder, retryAfter string) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"auth_unavailable"}` {
		t.Errorf("body = %q", got)
	}
	if got := rec.Header().Get("Retry-After"); got != retryAfter {
		t.Errorf("Retry-After = %q, want %q", got, retryAfter)
	}
}

func TestIdPDownIs503ThenRecovers(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	f.setDown(true)
	v := newVerifier(t, f) // must not need the IdP to construct
	tok := "Bearer " + sign(t, k, f.claims())

	rec := do(protected(v), "/", tok)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("IdP down: status = %d, want 503", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"auth_unavailable"}` {
		t.Errorf("body = %q", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want the 3 s cooldown", got)
	}

	// During the cooldown the failure is answered from cache, without asking the IdP.
	f.setDown(false)
	if _, err := v.Verify(t.Context(), sign(t, k, f.claims())); !errors.Is(err, auth.ErrUnavailable) {
		t.Errorf("Verify error = %v, want ErrUnavailable", err)
	}
	f.clock.Advance(time.Second)
	if rec := do(protected(v), "/", tok); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("in cooldown: status %d Retry-After %q, want 503 and 2", rec.Code, rec.Header().Get("Retry-After"))
	}
	if d, _ := f.hits(); d != 1 {
		t.Errorf("discovery fetched %d times during the cooldown, want 1", d)
	}

	f.clock.Advance(cooldown)
	decodeUser(t, do(protected(v), "/", tok))
}

func TestRequestCancelledWhileWaitingForDiscovery(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	captureLogs(t, &mu, &buf)

	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	f.set(func(f *fakeIdP) { f.discoveryDelay = 300 * time.Millisecond })
	v := newVerifier(t, f)
	tok := sign(t, k, f.claims())

	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+tok)
	time.AfterFunc(20*time.Millisecond, cancel)
	rec := httptest.NewRecorder()
	protected(v).ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || rec.Code == http.StatusUnauthorized {
		t.Errorf("cancelled request: status %d", rec.Code)
	}

	// The cancellation is not an IdP failure: no cooldown, the shared attempt
	// completes and the next request uses it.
	decodeUser(t, do(protected(v), "/", "Bearer "+tok))
	if d, _ := f.hits(); d != 1 {
		t.Errorf("discovery fetched %d times, want 1", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(buf.String(), "level=WARN") || strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("cancelled request logged as a failure: %s", buf.String())
	}
}

// forger signs tokens that pass every claim check but not the signature.
func forged(t *testing.T, f *fakeIdP, forger testKey, kid string) string {
	t.Helper()
	return sign(t, testKey{priv: forger.priv, kid: kid}, f.claims())
}

func randomKid(t *testing.T) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestForgedUnknownKidsFetchAtMostOncePerInterval(t *testing.T) {
	k, forger := newKey(t, "k1"), newKey(t, "forger")
	f := newFakeIdP(t, "", k)
	v := newVerifier(t, f)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k, f.claims())))
	_, base := f.hits()

	burst := func() {
		var wg sync.WaitGroup
		for range 50 {
			tok := forged(t, f, forger, randomKid(t))
			wg.Go(func() {
				// Invalid (401) or, while the refetch is throttled,
				// unavailable (503): never accepted.
				if _, err := v.Verify(t.Context(), tok); err == nil {
					t.Errorf("forged token accepted")
				}
			})
		}
		wg.Wait()
	}

	burst() // within the interval of the first load: no fetch at all
	if _, n := f.hits(); n != base {
		t.Errorf("JWKS fetched %d times within the interval, want 0", n-base)
	}
	f.clock.Advance(minRefetch)
	burst() // interval passed: one shared fetch
	burst() // and none again
	if _, n := f.hits(); n-base > 1 {
		t.Errorf("JWKS fetched %d times for 100 forged tokens, want at most 1", n-base)
	}
}

func TestForgedKnownKidNeverFetches(t *testing.T) {
	k, forger := newKey(t, "k1"), newKey(t, "forger")
	f := newFakeIdP(t, "", k)
	v := newVerifier(t, f)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k, f.claims())))
	_, base := f.hits()
	f.clock.Advance(minRefetch)
	for range 20 {
		assertUnauthorized(t, do(protected(v), "/", "Bearer "+forged(t, f, forger, "k1")))
	}
	if _, n := f.hits(); n != base {
		t.Errorf("JWKS fetched %d times for forged tokens with a known kid, want 0", n-base)
	}
}

func TestJWKSDownWithNewKidIs503ThenRecovers(t *testing.T) {
	k1, k2 := newKey(t, "k1"), newKey(t, "k2")
	f := newFakeIdP(t, "", k1)
	v := newVerifier(t, f)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())))

	f.setKeys(k1, k2)
	f.set(func(f *fakeIdP) { f.jwksDown = true })
	f.clock.Advance(minRefetch)
	tok2 := "Bearer " + sign(t, k2, f.claims())
	if rec := do(protected(v), "/", tok2); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("new kid with JWKS down: status = %d, want 503", rec.Code)
	}
	_, n := f.hits()
	// Old keys are kept on failure.
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())))

	// JWKS is back, but within the interval nothing is fetched: still 503.
	f.set(func(f *fakeIdP) { f.jwksDown = false })
	if rec := do(protected(v), "/", tok2); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("within interval after failure: status = %d, want 503", rec.Code)
	}
	if _, n2 := f.hits(); n2 != n {
		t.Errorf("JWKS fetched within the interval after a failure")
	}
	f.clock.Advance(minRefetch)
	decodeUser(t, do(protected(v), "/", tok2))
}

func TestValidTokenNotBlockedByForgedStream(t *testing.T) {
	k, forger := newKey(t, "k1"), newKey(t, "forger")
	f := newFakeIdP(t, "", k)
	v := newVerifier(t, f)
	valid := sign(t, k, f.claims())
	decodeUser(t, do(protected(v), "/", "Bearer "+valid))

	f.set(func(f *fakeIdP) { f.jwksDelay = 2 * time.Second })
	f.clock.Advance(minRefetch)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				_, _ = v.Verify(t.Context(), forged(t, f, forger, randomKid(t)))
			}
		})
	}
	time.Sleep(50 * time.Millisecond) // a forged token has started the slow fetch
	start := time.Now()
	for range 10 {
		if _, err := v.Verify(t.Context(), valid); err != nil {
			t.Errorf("valid token: %v", err)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("valid tokens took %v while forged tokens hold a fetch", d)
	}
	close(done)
	wg.Wait()
	if _, n := f.hits(); n > 2 {
		t.Errorf("JWKS fetched %d times, want at most 2", n)
	}
}

func TestJWKSOnOtherOriginIsUnavailable(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	f.set(func(f *fakeIdP) { f.jwksURI = "http://keys.example/keys" })
	if _, err := newVerifier(t, f).Verify(t.Context(), sign(t, k, f.claims())); !errors.Is(err, auth.ErrUnavailable) {
		t.Errorf("Verify error = %v, want ErrUnavailable", err)
	}
}

func TestOversizedIdPResponsesAreUnavailable(t *testing.T) {
	for _, path := range []string{"discovery", "jwks"} {
		t.Run(path, func(t *testing.T) {
			k := newKey(t, "k1")
			f := newFakeIdP(t, "", k)
			tok := sign(t, k, f.claims())
			v := newVerifier(t, f)
			if path == "jwks" {
				// Discovery and the first key set at normal size, then a new
				// kid whose fetch meets the oversized key set.
				if _, err := v.Verify(t.Context(), tok); err != nil {
					t.Fatal(err)
				}
				k2 := testKey{priv: k.priv, kid: "k2"}
				f.setKeys(k2)
				f.clock.Advance(minRefetch)
				tok = sign(t, k2, f.claims())
			}
			f.set(func(f *fakeIdP) { f.padding = 1 << 20 })
			if _, err := v.Verify(t.Context(), tok); !errors.Is(err, auth.ErrUnavailable) {
				t.Errorf("Verify error = %v, want ErrUnavailable", err)
			}
			// Just under the cap is fine.
			f.set(func(f *fakeIdP) { f.padding = 1<<20 - 4096 })
			f.clock.Advance(minRefetch)
			if _, err := v.Verify(t.Context(), tok); err != nil {
				t.Errorf("under the cap: %v", err)
			}
		})
	}
}

func TestIdPUnreachableIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	v, err := auth.NewOIDCVerifier(t.Context(), url, audience)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(t.Context(), "a.b.c"); !errors.Is(err, auth.ErrUnavailable) {
		t.Errorf("Verify error = %v, want ErrUnavailable", err)
	}
}

func TestConcurrentFirstRequestsShareDiscovery(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	f.mu.Lock()
	f.discoveryDelay = 50 * time.Millisecond
	f.mu.Unlock()
	v := newVerifier(t, f)
	tok := sign(t, k, f.claims())
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := v.Verify(t.Context(), tok); err != nil {
				t.Errorf("Verify: %v", err)
			}
		})
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.discoveryHits != 1 {
		t.Errorf("discovery fetched %d times, want 1", f.discoveryHits)
	}
}

func TestDiscoveryURLDifferentFromIssuer(t *testing.T) {
	const public = "http://localhost:8081/realms/geoint"
	k := newKey(t, "k1")
	f := newFakeIdP(t, public, k)
	v, err := auth.NewOIDCVerifierWithDiscovery(t.Context(), public, f.srv.URL, audience)
	if err != nil {
		t.Fatal(err)
	}
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k, f.claims())))

	c := f.claims()
	c["iss"] = f.srv.URL // the internal address is not the issuer
	assertUnauthorized(t, do(protected(v), "/", "Bearer "+sign(t, k, c)))
}

func TestDiscoveryAnnouncingOtherIssuerIsUnavailable(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "http://evil.example/realms/geoint", k)
	v, err := auth.NewOIDCVerifierWithDiscovery(t.Context(), "http://localhost:8081/realms/geoint", f.srv.URL, audience)
	if err != nil {
		t.Fatal(err)
	}
	c := f.claims()
	c["iss"] = "http://localhost:8081/realms/geoint"
	if _, err := v.Verify(t.Context(), sign(t, k, c)); !errors.Is(err, auth.ErrUnavailable) {
		t.Errorf("Verify error = %v, want ErrUnavailable", err)
	}
}

func TestConstructorRejectsBadConfig(t *testing.T) {
	cases := []struct{ issuer, discovery, aud string }{
		{"", "", audience},
		{"http://idp/realms/x", "", ""},
		{"ftp://idp/realms/x", "", audience},
		{"http:///realms/x", "", audience},
		{"http://idp/realms/x?q=1", "", audience},
		{"http://idp/realms/x", "file:///etc/passwd", audience},
	}
	for _, c := range cases {
		var err error
		if c.discovery == "" {
			_, err = auth.NewOIDCVerifier(t.Context(), c.issuer, c.aud)
		} else {
			_, err = auth.NewOIDCVerifierWithDiscovery(t.Context(), c.issuer, c.discovery, c.aud)
		}
		if err == nil {
			t.Errorf("config %+v accepted", c)
		}
	}
}

// captureLogs sends slog output at every level to buf for the rest of the test.
func captureLogs(t *testing.T, mu *sync.Mutex, buf *strings.Builder) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{mu, buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func TestLogsNeverContainToken(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	captureLogs(t, &mu, &buf)

	k, other := newKey(t, "k1"), newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	v := newVerifier(t, f)
	c := f.claims()
	c["aud"] = "other-api-INJECTED"
	c2 := f.claims()
	c2["iss"] = "http://evil.example/INJECTED\nlevel=ERROR"
	tokens := []string{
		sign(t, other, f.claims()),
		sign(t, k, c),
		sign(t, k, c2),
		"Bearer " + strings.Repeat("x", 9000),
		sign(t, k, f.claims()), // valid, on an admin route as analyst
	}
	for _, tok := range tokens {
		do(adminOnly(v), "/", "Bearer "+tok)
		do(adminOnly(v), "/", "Bearer "+tok, "Bearer "+tok)
	}
	f.setDown(true)
	v2 := newVerifier(t, f)
	do(protected(v2), "/", "Bearer "+tokens[0])

	mu.Lock()
	defer mu.Unlock()
	logs := buf.String()
	if logs == "" {
		t.Fatal("nothing logged")
	}
	if strings.Contains(logs, "INJECTED") {
		t.Errorf("logs contain attacker-controlled claim text: %q", logs)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if strings.Contains(line, "rejected") && !strings.Contains(line, "level=DEBUG") {
			t.Errorf("token rejection logged above debug: %q", line)
		}
	}
	for _, tok := range tokens {
		for _, part := range strings.Split(tok, ".") {
			if len(part) > 16 && strings.Contains(logs, part) {
				t.Fatalf("logs contain token material: %q", logs)
			}
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestMain keeps the middleware's logs out of the test output.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}
