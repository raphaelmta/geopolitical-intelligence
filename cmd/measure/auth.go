package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// keycloak talks to the Keycloak of the stack: the admin REST API (with the
// bootstrap admin of the settings file) to create and delete the temporary
// measuring user, and the real login endpoints of the application's public
// client (authorization code + PKCE S256, as the browser does; the client
// forbids direct grants and the realm is never changed).
type keycloak struct {
	base, realm, client, redirect string
	adminUser, adminPassword      string
	http                          *http.Client
}

// randomString returns n random bytes, base64url without padding.
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (k *keycloak) adminToken(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {k.adminUser}, "password": {k.adminPassword}}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := k.postForm(ctx, k.base+"/realms/master/protocol/openid-connect/token", form, &tok); err != nil {
		return "", fmt.Errorf("admin token: %w", err)
	}
	return tok.AccessToken, nil
}

func (k *keycloak) postForm(ctx context.Context, u string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// The body of an error is an OAuth error object; never a token.
		return fmt.Errorf("%s: status %s: %s", u, resp.Status, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

func (k *keycloak) admin(ctx context.Context, tok, method, path string, in any) (*http.Response, []byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, nil, err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+"/admin/realms/"+k.realm+path, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, b, nil
}

// tempUser is the temporary measuring user: random name and password, held
// only in memory.
type tempUser struct {
	ID, Username, password string
}

// createUser creates an enabled user with role analyst in the realm.
func (k *keycloak) createUser(ctx context.Context) (tempUser, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return tempUser{}, err
	}
	u := tempUser{Username: "measure-" + hex.EncodeToString(b), password: randomString(24)}
	tok, err := k.adminToken(ctx)
	if err != nil {
		return u, err
	}
	rep := map[string]any{
		"username": u.Username, "enabled": true, "emailVerified": true,
		"firstName": "Medicao", "lastName": "Temporaria", "email": u.Username + "@measure.invalid",
		"credentials": []map[string]any{{"type": "password", "value": u.password, "temporary": false}},
	}
	resp, body, err := k.admin(ctx, tok, http.MethodPost, "/users", rep)
	if err != nil {
		return u, err
	}
	if resp.StatusCode != http.StatusCreated {
		return u, fmt.Errorf("create user: %s: %s", resp.Status, truncate(string(body), 200))
	}
	loc := resp.Header.Get("Location")
	u.ID = loc[strings.LastIndex(loc, "/")+1:]
	resp, body, err = k.admin(ctx, tok, http.MethodGet, "/roles/analyst", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return u, fmt.Errorf("role analyst: %v %s", err, statusOf(resp))
	}
	var role map[string]any
	if err := json.Unmarshal(body, &role); err != nil {
		return u, err
	}
	resp, body, err = k.admin(ctx, tok, http.MethodPost, "/users/"+u.ID+"/role-mappings/realm", []any{role})
	if err != nil || resp.StatusCode != http.StatusNoContent {
		return u, fmt.Errorf("assign analyst: %v %s %s", err, statusOf(resp), truncate(string(body), 200))
	}
	return u, nil
}

// deleteUser deletes the user (a fresh admin token: they last 60 s).
func (k *keycloak) deleteUser(ctx context.Context, id string) error {
	tok, err := k.adminToken(ctx)
	if err != nil {
		return err
	}
	resp, body, err := k.admin(ctx, tok, http.MethodDelete, "/users/"+id, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete user: %s %s", resp.Status, truncate(string(body), 200))
	}
	return nil
}

var formAction = regexp.MustCompile(`<form[^>]*\baction="([^"]+)"`)

// session holds the tokens of a logged-in user; Token refreshes them a
// minute before the access token expires (it lasts 5 minutes).
type session struct {
	k       *keycloak
	mu      sync.Mutex
	access  string
	refresh string
	expires time.Time
}

// login runs the authorization code flow with PKCE S256 against the public
// client, as the web app does: authorization request, the login form, the
// redirect to the app's callback carrying the code, the code exchange.
// Keycloak's cookies are marked Secure; over plain HTTP on localhost they
// are forwarded by hand (as a browser does for localhost).
func (k *keycloak) login(ctx context.Context, u tempUser) (*session, error) {
	verifier := randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state, nonce := randomString(16), randomString(16)
	cookies := map[string]string{}
	noRedirect := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(req *http.Request) (*http.Response, []byte, error) {
		var parts []string
		for n, v := range cookies {
			parts = append(parts, n+"="+v)
		}
		if len(parts) > 0 {
			req.Header.Set("Cookie", strings.Join(parts, "; "))
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		for _, c := range resp.Cookies() {
			cookies[c.Name] = c.Value
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return resp, b, nil
	}
	q := url.Values{"client_id": {k.client}, "response_type": {"code"}, "scope": {"openid"},
		"redirect_uri": {k.redirect}, "state": {state}, "nonce": {nonce},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, k.base+"/realms/"+k.realm+"/protocol/openid-connect/auth?"+q.Encode(), nil)
	resp, body, err := do(req)
	if err != nil {
		return nil, fmt.Errorf("login page: %w", err)
	}
	m := formAction.FindSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil {
		return nil, fmt.Errorf("login page: %s, form found %v", resp.Status, m != nil)
	}
	action := html.UnescapeString(string(m[1]))
	// The password goes only to this realm's own login endpoint.
	if !strings.HasPrefix(action, k.base+"/realms/"+k.realm+"/") {
		return nil, errors.New("login page: the form posts outside the realm; refusing to send the password")
	}
	form := url.Values{"username": {u.Username}, "password": {u.password}, "credentialId": {""}}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, action, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _, err = do(req)
	if err != nil {
		return nil, fmt.Errorf("login post: %w", err)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || !strings.HasPrefix(loc.String(), k.redirect) {
		return nil, fmt.Errorf("login post: %s (no redirect to the callback)", resp.Status)
	}
	if loc.Query().Get("state") != state || loc.Query().Get("code") == "" {
		return nil, errors.New("login post: callback without the expected state or a code")
	}
	s := &session{k: k}
	form = url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"redirect_uri": {k.redirect}, "client_id": {k.client}, "code_verifier": {verifier}}
	if err := s.token(ctx, form); err != nil {
		return nil, fmt.Errorf("code exchange: %w", err)
	}
	return s, nil
}

func (s *session) token(ctx context.Context, form url.Values) error {
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := s.k.postForm(ctx, s.k.base+"/realms/"+s.k.realm+"/protocol/openid-connect/token", form, &tok); err != nil {
		return err
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return errors.New("token response without tokens")
	}
	s.access, s.refresh = tok.AccessToken, tok.RefreshToken
	s.expires = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return nil
}

// Token returns a valid access token, refreshing it (refresh tokens are
// single-use in this realm: the newest one is kept) when it expires within
// a minute.
func (s *session) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Until(s.expires) > time.Minute {
		return s.access, nil
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.refresh}, "client_id": {s.k.client}}
	if err := s.token(ctx, form); err != nil {
		return "", fmt.Errorf("refresh: %w", err)
	}
	return s.access, nil
}

func statusOf(resp *http.Response) string {
	if resp == nil {
		return "no response"
	}
	return resp.Status
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
