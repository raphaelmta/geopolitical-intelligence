package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func mustRSA(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustEC(t *testing.T, c elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestJWKSKeepsOnlyUsableSigningKeys pins the key filter: only public RS256
// keys of at least 2048 bits and ES256 keys on P-256, for signing.
func TestJWKSKeepsOnlyUsableSigningKeys(t *testing.T) {
	rsa2048, rsa1024 := mustRSA(t, 2048), mustRSA(t, 1024)
	p256, p384 := mustEC(t, elliptic.P256()), mustEC(t, elliptic.P384())
	keys := []jose.JSONWebKey{
		{Key: &rsa2048.PublicKey, KeyID: "rsa-ok", Algorithm: "RS256", Use: "sig"},
		{Key: &rsa2048.PublicKey, KeyID: "rsa-no-alg-no-use"},
		{Key: &rsa2048.PublicKey, KeyID: "rsa-enc", Algorithm: "RS256", Use: "enc"},
		{Key: &rsa2048.PublicKey, KeyID: "rsa-rs512", Algorithm: "RS512", Use: "sig"},
		{Key: &rsa2048.PublicKey, KeyID: "rsa-as-es256", Algorithm: "ES256", Use: "sig"},
		{Key: &rsa1024.PublicKey, KeyID: "rsa-1024", Algorithm: "RS256", Use: "sig"},
		{Key: rsa2048, KeyID: "rsa-private", Algorithm: "RS256", Use: "sig"},
		{Key: &p256.PublicKey, KeyID: "ec-ok", Algorithm: "ES256", Use: "sig"},
		{Key: &p256.PublicKey, KeyID: "ec-as-rs256", Algorithm: "RS256", Use: "sig"},
		{Key: &p384.PublicKey, KeyID: "ec-p384", Algorithm: "ES384", Use: "sig"},
		{Key: &p384.PublicKey, KeyID: "ec-p384-no-alg", Use: "sig"},
		{Key: []byte("0123456789abcdef0123456789abcdef"), KeyID: "oct", Algorithm: "HS256", Use: "sig"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	defer srv.Close()
	k := &keySet{url: srv.URL, client: newHTTPClient(), base: context.Background()}
	got, err := k.load()
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, key := range got {
		kids = append(kids, key.KeyID)
	}
	slices.Sort(kids)
	if want := []string{"ec-ok", "rsa-no-alg-no-use", "rsa-ok"}; !slices.Equal(kids, want) {
		t.Fatalf("kept keys %v, want %v", kids, want)
	}
}

// TestVerifySignatureAcceptsOnlyCompactJWS checks the key set itself: a JSON
// serialization is refused even when its signature is good, and so is a kid
// carried only in an unprotected header.
func TestVerifySignatureAcceptsOnlyCompactJWS(t *testing.T) {
	priv := mustRSA(t, 2048)
	now := time.Now()
	k := &keySet{now: func() time.Time { return now }, minRefetch: time.Hour, emptyRetry: time.Hour,
		maxAge: time.Hour, maxStale: 24 * time.Hour, lastAttempt: now, loadedAt: now,
		keys: []jose.JSONWebKey{{Key: &priv.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	payload := enc(map[string]string{"sub": "x"})
	build := func(protected map[string]string, unprotected map[string]string) string {
		prot := enc(protected)
		h := sha256.Sum256([]byte(prot + "." + payload))
		sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
		if err != nil {
			t.Fatal(err)
		}
		obj := map[string]any{"protected": prot, "payload": payload, "signature": base64.RawURLEncoding.EncodeToString(sig)}
		if unprotected != nil {
			obj["header"] = unprotected
		}
		b, _ := json.Marshal(obj)
		return string(b)
	}
	compact := func() string {
		prot := enc(map[string]string{"alg": "RS256", "kid": "k1"})
		h := sha256.Sum256([]byte(prot + "." + payload))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
		return prot + "." + payload + "." + base64.RawURLEncoding.EncodeToString(sig)
	}()
	if _, err := k.VerifySignature(context.Background(), compact); err != nil {
		t.Fatalf("compact token: %v", err)
	}
	for name, raw := range map[string]string{
		"JSON, kid protected":   build(map[string]string{"alg": "RS256", "kid": "k1"}, nil),
		"JSON, kid unprotected": build(map[string]string{"alg": "RS256"}, map[string]string{"kid": "k1"}),
	} {
		if _, err := k.VerifySignature(context.Background(), raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
