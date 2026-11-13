package auth_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"geoint/internal/auth"
)

// signJOSE signs claims with alg and key; kid "" leaves the kid out.
func signJOSE(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	var sk any = key
	if kid != "" {
		sk = jose.JSONWebKey{Key: key, KeyID: kid}
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: sk}, (&jose.SignerOptions{}).WithType("JWT"))
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

func newECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func ecPublic(k *ecdsa.PrivateKey, kid string) jose.JSONWebKey {
	return jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Algorithm: string(jose.ES256), Use: "sig"}
}

// newVerifierWith builds a verifier on f with f's clock and o's other knobs.
func newVerifierWith(t *testing.T, f *fakeIdP, o auth.TestOptions) auth.Verifier {
	t.Helper()
	if o.MinRefetch == 0 {
		o.MinRefetch = minRefetch
	}
	if o.Cooldown == 0 {
		o.Cooldown = cooldown
	}
	o.Now = f.clock.Now
	v, err := auth.NewOIDCVerifierForTest(t.Context(), f.issuer, f.srv.URL, audience, o)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func assertStatus(t *testing.T, name string, rec interface{ Result() *http.Response }, want int) {
	t.Helper()
	if got := rec.Result().StatusCode; got != want {
		t.Errorf("%s: status = %d, want %d", name, got, want)
	}
}

func TestES256TokensVerify(t *testing.T) {
	ec, other := newECKey(t), newECKey(t)
	f := newFakeIdP(t, "")
	f.setJWKS(ecPublic(ec, "e1"))
	v := newVerifier(t, f)
	decodeUser(t, do(protected(v), "/", "Bearer "+signJOSE(t, jose.ES256, ec, "e1", f.claims())))

	// Another P-256 key under the same kid: a bad signature, no fetch.
	_, before := f.hits()
	assertUnauthorized(t, do(protected(v), "/", "Bearer "+signJOSE(t, jose.ES256, other, "e1", f.claims())))
	parts := strings.Split(signJOSE(t, jose.ES256, ec, "e1", f.claims()), ".")
	c := f.claims()
	c["roles"] = []string{"admin"}
	assertUnauthorized(t, do(protected(v), "/", "Bearer "+parts[0]+"."+b64(c)+"."+parts[2]))
	if _, after := f.hits(); after != before {
		t.Errorf("bad ES256 signatures under a known kid fetched the JWKS %d times", after-before)
	}
}

// A token's alg header must match the key type its kid names; neither an
// RS256 header over an EC key nor an ES256 header over an RSA key passes.
func TestCrossTypeAlgHeadersAreRejected(t *testing.T) {
	rk, ec := newKey(t, "k1"), newECKey(t)
	f := newFakeIdP(t, "")
	f.setJWKS(rk.public(), ecPublic(ec, "e1"))
	v := newVerifier(t, f)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, rk, f.claims()))) // keys loaded

	input := func(alg, kid string) string {
		return b64(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"}) + "." + b64(f.claims())
	}
	// RS256 header naming the EC key, signed with PKCS#1 v1.5 by an RSA key.
	in := input("RS256", "e1")
	h := sha256.Sum256([]byte(in))
	rsaSig, err := rsa.SignPKCS1v15(rand.Reader, rk.priv, crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	rsHeaderECKey := in + "." + base64.RawURLEncoding.EncodeToString(rsaSig)
	// ES256 header naming the RSA key, with a well-formed ES256 signature.
	in = input("ES256", "k1")
	h = sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, ec, h[:])
	if err != nil {
		t.Fatal(err)
	}
	esSig := make([]byte, 64)
	r.FillBytes(esSig[:32])
	s.FillBytes(esSig[32:])
	esHeaderRSAKey := in + "." + base64.RawURLEncoding.EncodeToString(esSig)

	_, before := f.hits()
	assertUnauthorized(t, do(protected(v), "/", "Bearer "+rsHeaderECKey))
	assertUnauthorized(t, do(protected(v), "/", "Bearer "+esHeaderRSAKey))
	if _, after := f.hits(); after != before {
		t.Errorf("cross-type tokens fetched the JWKS %d times", after-before)
	}
}

func TestKidlessTokens(t *testing.T) {
	k, forger := newKey(t, "k1"), newKey(t, "x")
	f := newFakeIdP(t, "", k)
	v := newVerifier(t, f)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k, f.claims()))) // keys loaded
	// Without a kid every cached key is tried: a token of a known key passes.
	decodeUser(t, do(protected(v), "/", "Bearer "+signJOSE(t, jose.RS256, k.priv, "", f.claims())))
	// A forged kid-less token is never accepted and, within the interval,
	// fetches nothing.
	_, before := f.hits()
	for range 5 {
		if rec := do(protected(v), "/", "Bearer "+signJOSE(t, jose.RS256, forger.priv, "", f.claims())); rec.Code == http.StatusOK {
			t.Fatal("forged kid-less token accepted")
		}
	}
	if _, after := f.hits(); after != before {
		t.Errorf("forged kid-less tokens fetched the JWKS %d times within the interval", after-before)
	}
}

// jsonJWS builds a JSON-serialized JWS by hand: the protected header, an
// optional unprotected header, RS256 by k.
func jsonJWS(t *testing.T, k testKey, protectedHdr map[string]string, unprotected map[string]string, claims map[string]any) string {
	t.Helper()
	prot, payload := b64(protectedHdr), b64(claims)
	h := sha256.Sum256([]byte(prot + "." + payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.priv, crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]any{"protected": prot, "payload": payload, "signature": base64.RawURLEncoding.EncodeToString(sig)}
	if unprotected != nil {
		obj["header"] = unprotected
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Only the compact serialization is accepted, and the kid is read from the
// protected header only.
func TestNonCompactJWSIsRejected(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	v := newVerifier(t, f)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k, f.claims()))) // keys loaded

	multi, err := jose.NewMultiSigner([]jose.SigningKey{
		{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k.priv, KeyID: "k1"}},
		{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k.priv, KeyID: "k1"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(f.claims())
	obj, err := multi.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"JSON serialization, kid protected": jsonJWS(t, k, map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"}, nil, f.claims()),
		"kid only in the unprotected header": jsonJWS(t, k, map[string]string{"alg": "RS256", "typ": "JWT"},
			map[string]string{"kid": "k1"}, f.claims()),
		"two signatures": obj.FullSerialize(),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			// The header check refuses spaces; JSON has none here.
			if strings.ContainsAny(tok, " \t") {
				t.Fatal("test token has whitespace")
			}
			assertUnauthorized(t, do(protected(v), "/", "Bearer "+tok))
		})
	}
}

// fetchSignal returns an OnFetch hook and a wait function that blocks until
// the next fetch has ended.
func fetchSignal(t *testing.T) (func(), func()) {
	ch := make(chan struct{}, 16)
	return func() { ch <- struct{}{} }, func() {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("no JWKS fetch ended")
		}
	}
}

const maxKeyAge = 15 * time.Minute

func TestRemovedKeyStopsBeingTrustedAfterMaxKeyAge(t *testing.T) {
	k1, k2 := newKey(t, "k1"), newKey(t, "k2")
	f := newFakeIdP(t, "", k1, k2)
	hook, wait := fetchSignal(t)
	v := newVerifierWith(t, f, auth.TestOptions{MaxKeyAge: maxKeyAge, OnFetch: hook})
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())))
	wait()

	f.setKeys(k2) // k1 removed at the IdP
	f.clock.Advance(maxKeyAge - time.Second)
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims()))) // young enough: no refresh
	if _, n := f.hits(); n != 1 {
		t.Fatalf("JWKS fetched %d times before the keys were old, want 1", n)
	}
	f.clock.Advance(2 * time.Second)
	// Old keys: this request is still answered from cache, and starts a
	// background refresh.
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())))
	wait()
	if _, n := f.hits(); n != 2 {
		t.Fatalf("JWKS fetched %d times, want 2", n)
	}
	// After the refresh k1 is gone (unknown kid, refetch throttled → 503).
	if rec := do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())); rec.Code == http.StatusOK {
		t.Fatal("removed key still trusted after the refresh")
	}
	decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k2, f.claims())))
}

func TestFailedRefreshKeepsKeysUntilMaxStaleAge(t *testing.T) {
	const maxStale = 24 * time.Hour
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	hook, wait := fetchSignal(t)
	v := newVerifierWith(t, f, auth.TestOptions{MaxKeyAge: maxKeyAge, MaxStaleKeyAge: maxStale, OnFetch: hook})
	tok := "Bearer " + sign(t, k, f.claims())
	decodeUser(t, do(protected(v), "/", tok))
	wait()

	f.set(func(f *fakeIdP) { f.jwksDown = true })
	f.clock.Advance(maxKeyAge + time.Second)
	decodeUser(t, do(protected(v), "/", tok)) // old keys kept; background refresh fails
	wait()
	decodeUser(t, do(protected(v), "/", tok)) // within the interval: no new attempt
	if _, n := f.hits(); n != 2 {
		t.Fatalf("JWKS fetched %d times, want 2 (no retry within the interval)", n)
	}
	f.clock.Advance(minRefetch)
	decodeUser(t, do(protected(v), "/", tok)) // retried on the normal interval
	wait()
	if _, n := f.hits(); n != 3 {
		t.Fatalf("JWKS fetched %d times, want 3", n)
	}

	// A day without a successful refresh: the keys are no longer trusted.
	f.clock.Advance(maxStale)
	rec := do(protected(v), "/", tok)
	if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != `{"error":"auth_unavailable"}` {
		t.Fatalf("keys a day old: status %d body %q, want 503 auth_unavailable", rec.Code, rec.Body.String())
	}
	wait()
	f.set(func(f *fakeIdP) { f.jwksDown = false })
	if rec := do(protected(v), "/", tok); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("within the interval after the failure: status %d, want 503", rec.Code)
	}
	f.clock.Advance(minRefetch)
	decodeUser(t, do(protected(v), "/", tok)) // a successful refresh trusts the keys again
}

func TestEmptyKeyCacheRetriesAfterTheShortCooldown(t *testing.T) {
	k := newKey(t, "k1")
	f := newFakeIdP(t, "", k)
	f.set(func(f *fakeIdP) { f.jwksDown = true })
	v := newVerifier(t, f)
	tok := "Bearer " + sign(t, k, f.claims())
	assertThrottled(t, do(protected(v), "/", tok), "3")
	f.clock.Advance(time.Second)
	assertThrottled(t, do(protected(v), "/", tok), "2")
	f.set(func(f *fakeIdP) { f.jwksDown = false })
	f.clock.Advance(cooldown)
	decodeUser(t, do(protected(v), "/", tok))
	if _, n := f.hits(); n != 2 {
		t.Errorf("JWKS fetched %d times, want 2", n)
	}
}

func warnLines(mu *sync.Mutex, buf *strings.Builder) int {
	mu.Lock()
	defer mu.Unlock()
	return strings.Count(buf.String(), "level=WARN")
}

func TestOutageWarnsOnlyForTheRequestThatFetched(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	captureLogs(t, &mu, &buf)

	reset := func() { mu.Lock(); buf.Reset(); mu.Unlock() }
	t.Run("jwks", func(t *testing.T) {
		reset()
		k1, k2 := newKey(t, "k1"), newKey(t, "k2")
		f := newFakeIdP(t, "", k1)
		v := newVerifier(t, f)
		decodeUser(t, do(protected(v), "/", "Bearer "+sign(t, k1, f.claims())))
		f.setKeys(k1, k2)
		f.set(func(f *fakeIdP) { f.jwksDown = true })
		f.clock.Advance(minRefetch)
		tok := "Bearer " + sign(t, k2, f.claims())
		for range 10 {
			assertStatus(t, "jwks down", do(protected(v), "/", tok), http.StatusServiceUnavailable)
		}
		if n := warnLines(&mu, &buf); n != 1 {
			t.Errorf("%d warnings for one failed fetch, want 1:\n%s", n, buf.String())
		}
	})
	t.Run("discovery", func(t *testing.T) {
		reset()
		k := newKey(t, "k1")
		f := newFakeIdP(t, "", k)
		f.setDown(true)
		v := newVerifier(t, f)
		tok := "Bearer " + sign(t, k, f.claims())
		for range 10 {
			assertStatus(t, "discovery down", do(protected(v), "/", tok), http.StatusServiceUnavailable)
		}
		if n := warnLines(&mu, &buf); n != 1 {
			t.Errorf("%d warnings for one failed discovery, want 1:\n%s", n, buf.String())
		}
	})
}
