package auth_test

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const realmFile = "../../deploy/keycloak/realm-geoint.json"

// forbiddenKey reports whether a realm-file key would carry users, secrets or
// key material, none of which may ever be committed.
func forbiddenKey(k string) bool {
	switch k {
	case "users", "federatedUsers", "credentials", "components", "keys", "privateKey", "certificate", "smtpServer":
		return true
	case "passwordPolicy", "resetPasswordAllowed":
		return false
	}
	l := strings.ToLower(k)
	return strings.Contains(l, "secret") || strings.Contains(l, "password")
}

func walk(t *testing.T, path string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if forbiddenKey(k) {
				t.Errorf("realm file has forbidden key %s.%s", path, k)
			}
			walk(t, path+"."+k, child)
		}
	case []any:
		for _, child := range x {
			walk(t, path+"[]", child)
		}
	case string:
		if strings.Contains(x, "-----BEGIN") {
			t.Errorf("realm file has PEM material at %s", path)
		}
	}
}

type realmFileShape struct {
	Realm                 string `json:"realm"`
	Enabled               bool   `json:"enabled"`
	SSLRequired           string `json:"sslRequired"`
	RegistrationAllowed   *bool  `json:"registrationAllowed"`
	ResetPasswordAllowed  *bool  `json:"resetPasswordAllowed"`
	BruteForceProtected   bool   `json:"bruteForceProtected"`
	FailureFactor         int    `json:"failureFactor"`
	WaitIncrementSeconds  int    `json:"waitIncrementSeconds"`
	MaxFailureWaitSeconds int    `json:"maxFailureWaitSeconds"`
	AccessTokenLifespan   int    `json:"accessTokenLifespan"`
	RevokeRefreshToken    bool   `json:"revokeRefreshToken"`
	RefreshTokenMaxReuse  *int   `json:"refreshTokenMaxReuse"`
	SSOSessionIdleTimeout int    `json:"ssoSessionIdleTimeout"`
	SSOSessionMaxLifespan int    `json:"ssoSessionMaxLifespan"`
	PasswordPolicy        string `json:"passwordPolicy"`
	Roles                 struct {
		Realm []struct {
			Name       string `json:"name"`
			Composite  bool   `json:"composite"`
			Composites struct {
				Realm  []string       `json:"realm"`
				Client map[string]any `json:"client"`
			} `json:"composites"`
		} `json:"realm"`
	} `json:"roles"`
	Clients []struct {
		ClientID                  string            `json:"clientId"`
		Enabled                   bool              `json:"enabled"`
		PublicClient              bool              `json:"publicClient"`
		BearerOnly                bool              `json:"bearerOnly"`
		FullScopeAllowed          *bool             `json:"fullScopeAllowed"`
		StandardFlowEnabled       bool              `json:"standardFlowEnabled"`
		ImplicitFlowEnabled       *bool             `json:"implicitFlowEnabled"`
		DirectAccessGrantsEnabled *bool             `json:"directAccessGrantsEnabled"`
		ServiceAccountsEnabled    *bool             `json:"serviceAccountsEnabled"`
		DefaultClientScopes       []string          `json:"defaultClientScopes"`
		OptionalClientScopes      *[]string         `json:"optionalClientScopes"`
		RedirectURIs              []string          `json:"redirectUris"`
		WebOrigins                []string          `json:"webOrigins"`
		Attributes                map[string]string `json:"attributes"`
		ProtocolMappers           []struct {
			Name           string            `json:"name"`
			Protocol       string            `json:"protocol"`
			ProtocolMapper string            `json:"protocolMapper"`
			Config         map[string]string `json:"config"`
		} `json:"protocolMappers"`
	} `json:"clients"`
}

func TestRealmFileIsSecure(t *testing.T) {
	raw, err := os.ReadFile(realmFile)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("realm file is not valid JSON: %v", err)
	}
	walk(t, "$", generic)

	var r realmFileShape
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.Realm != "geoint" || !r.Enabled {
		t.Errorf("realm = %q enabled=%v", r.Realm, r.Enabled)
	}
	if r.SSLRequired != "external" {
		t.Errorf("sslRequired = %q", r.SSLRequired)
	}
	if r.RegistrationAllowed == nil || *r.RegistrationAllowed {
		t.Error("registrationAllowed must be explicitly false")
	}
	if r.ResetPasswordAllowed == nil || *r.ResetPasswordAllowed {
		t.Error("resetPasswordAllowed must be explicitly false")
	}
	if !r.BruteForceProtected {
		t.Error("bruteForceProtected must be true")
	}
	if r.FailureFactor < 1 || r.FailureFactor > 10 || r.WaitIncrementSeconds < 30 || r.MaxFailureWaitSeconds < 300 {
		t.Errorf("brute-force thresholds: failureFactor=%d waitIncrement=%d maxWait=%d",
			r.FailureFactor, r.WaitIncrementSeconds, r.MaxFailureWaitSeconds)
	}
	if r.AccessTokenLifespan != 300 {
		t.Errorf("accessTokenLifespan = %d", r.AccessTokenLifespan)
	}
	if !r.RevokeRefreshToken || r.RefreshTokenMaxReuse == nil || *r.RefreshTokenMaxReuse != 0 {
		t.Error("refresh token rotation must be on with no reuse")
	}
	if r.SSOSessionIdleTimeout != 1800 || r.SSOSessionMaxLifespan != 36000 {
		t.Errorf("sso idle/max = %d/%d", r.SSOSessionIdleTimeout, r.SSOSessionMaxLifespan)
	}
	if m := regexp.MustCompile(`length\((\d+)\)`).FindStringSubmatch(r.PasswordPolicy); m == nil {
		t.Errorf("passwordPolicy = %q, want a length rule", r.PasswordPolicy)
	} else if n, _ := strconv.Atoi(m[1]); n < 12 {
		t.Errorf("passwordPolicy length = %d, want >= 12", n)
	}
	if !strings.Contains(r.PasswordPolicy, "notUsername") {
		t.Errorf("passwordPolicy = %q, want notUsername", r.PasswordPolicy)
	}

	var roles []string
	for _, role := range r.Roles.Realm {
		roles = append(roles, role.Name)
		switch role.Name {
		case "admin":
			// admin includes analyst in the realm, so an admin's token carries both.
			if !role.Composite || !slices.Equal(role.Composites.Realm, []string{"analyst"}) || len(role.Composites.Client) != 0 {
				t.Errorf("admin must be a composite of exactly analyst: %+v", role)
			}
		default:
			if role.Composite {
				t.Errorf("role %s must not be composite", role.Name)
			}
		}
	}
	// Nobody gets a platform role by default: the realm's default role must
	// stay Keycloak's own (no defaultRole/defaultRoles override in the file).
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"defaultRole", "defaultRoles", "defaultGroups"} {
		if _, ok := top[k]; ok {
			t.Errorf("realm file sets %s", k)
		}
	}
	if strings.Contains(string(raw), "default-roles-") {
		t.Error("realm file touches the default-roles composite")
	}
	for _, want := range []string{"analyst", "admin"} {
		if !slices.Contains(roles, want) {
			t.Errorf("realm role %q missing (have %v)", want, roles)
		}
	}

	if len(r.Clients) != 1 {
		t.Fatalf("want exactly one client, got %d", len(r.Clients))
	}
	c := r.Clients[0]
	if c.ClientID != "geoint-web" || !c.Enabled {
		t.Errorf("client = %q enabled=%v", c.ClientID, c.Enabled)
	}
	if !c.PublicClient || c.BearerOnly {
		t.Error("geoint-web must be a public client")
	}
	// fullScopeAllowed true: tokens carry every realm role the user holds,
	// including Keycloak's default ones; harmless because Has matches exactly.
	if c.FullScopeAllowed == nil || !*c.FullScopeAllowed {
		t.Error("fullScopeAllowed must be explicitly true (roles mapper needs the realm roles in scope)")
	}
	if !c.StandardFlowEnabled {
		t.Error("standard flow must be on")
	}
	for name, flag := range map[string]*bool{
		"implicitFlowEnabled":       c.ImplicitFlowEnabled,
		"directAccessGrantsEnabled": c.DirectAccessGrantsEnabled,
		"serviceAccountsEnabled":    c.ServiceAccountsEnabled,
	} {
		if flag == nil || *flag {
			t.Errorf("%s must be explicitly false", name)
		}
	}
	// Scopes are listed explicitly: "basic" carries sub, and offline_access
	// (long-lived offline tokens) must not be requestable by a browser client.
	if !slices.Contains(c.DefaultClientScopes, "basic") || slices.Contains(c.DefaultClientScopes, "offline_access") {
		t.Errorf("defaultClientScopes = %v", c.DefaultClientScopes)
	}
	if c.OptionalClientScopes == nil || slices.Contains(*c.OptionalClientScopes, "offline_access") {
		t.Error("optionalClientScopes must be explicit and exclude offline_access")
	}
	if !slices.Equal(c.RedirectURIs, []string{"http://localhost:5173/*"}) {
		t.Errorf("redirectUris = %v", c.RedirectURIs)
	}
	if !slices.Equal(c.WebOrigins, []string{"http://localhost:5173"}) {
		t.Errorf("webOrigins = %v", c.WebOrigins)
	}
	wantAttrs := map[string]string{
		"pkce.code.challenge.method":                "S256",
		"post.logout.redirect.uris":                 "http://localhost:5173/*",
		"oauth2.device.authorization.grant.enabled": "false",
		"oidc.ciba.grant.enabled":                   "false",
		"standard.token.exchange.enabled":           "false",
	}
	for k, want := range wantAttrs {
		if got := c.Attributes[k]; got != want {
			t.Errorf("attribute %s = %q, want %q", k, got, want)
		}
	}

	var haveAudience, haveRoles bool
	for _, m := range c.ProtocolMappers {
		if m.Protocol != "openid-connect" {
			continue
		}
		switch m.ProtocolMapper {
		case "oidc-audience-mapper":
			haveAudience = m.Config["included.custom.audience"] == "geoint-api" &&
				m.Config["access.token.claim"] == "true" && m.Config["id.token.claim"] == "false"
		case "oidc-usermodel-realm-role-mapper":
			haveRoles = m.Config["claim.name"] == "roles" && m.Config["multivalued"] == "true" &&
				m.Config["access.token.claim"] == "true" && m.Config["id.token.claim"] == "false" &&
				m.Config["userinfo.token.claim"] == "false"
		}
	}
	if !haveAudience {
		t.Error("missing audience mapper adding geoint-api to access tokens only")
	}
	if !haveRoles {
		t.Error("missing realm role mapper to top-level multivalued claim roles in access tokens")
	}
}

const (
	e2eRealmFile    = "../../deploy/keycloak/realm-e2e.json"
	composeFile     = "../../deploy/compose.yml"
	composeE2EFile  = "../../deploy/compose.e2e.yml"
	e2eRealmName    = "geoint-e2e"
	e2ePasswordMark = "e2e-only-"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return v
}

// The e2e realm (deploy/keycloak/realm-e2e.json, imported only by the
// throwaway stack of `make e2e`) is the production realm plus two test
// users. Only its name, display name and users may differ, so no security
// setting can drift between the two files: the e2e run tests the
// production client, flows, roles, mappers and brute-force settings.
func TestE2ERealmDiffersFromProductionOnlyInNameAndTestUsers(t *testing.T) {
	prod := readJSON(t, realmFile)
	e2e := readJSON(t, e2eRealmFile)

	if e2e["realm"] != e2eRealmName {
		t.Errorf("e2e realm name = %v, want %s (a name the normal stack never uses)", e2e["realm"], e2eRealmName)
	}
	if d, _ := e2e["displayName"].(string); !strings.Contains(d, "make e2e") {
		t.Errorf("e2e realm displayName = %q, want it to say it is the make e2e test realm", d)
	}
	users, _ := e2e["users"].([]any)
	want := map[string]string{"analista": "analyst", "admin-e2e": "admin"}
	if len(users) != len(want) {
		t.Fatalf("e2e realm has %d users, want %d", len(users), len(want))
	}
	for _, u := range users {
		m, _ := u.(map[string]any)
		name, _ := m["username"].(string)
		role, ok := want[name]
		if !ok {
			t.Errorf("unexpected e2e user %q", name)
			continue
		}
		if roles, _ := m["realmRoles"].([]any); len(roles) != 1 || roles[0] != role {
			t.Errorf("user %s realmRoles = %v, want [%s]", name, m["realmRoles"], role)
		}
		if ra, _ := m["requiredActions"].([]any); len(ra) != 0 || m["enabled"] != true {
			t.Errorf("user %s: enabled=%v requiredActions=%v", name, m["enabled"], m["requiredActions"])
		}
		creds, _ := m["credentials"].([]any)
		if len(creds) != 1 {
			t.Errorf("user %s has %d credentials", name, len(creds))
			continue
		}
		c, _ := creds[0].(map[string]any)
		pw, _ := c["value"].(string)
		// Obviously test-only, and valid under the realm's password policy.
		if c["type"] != "password" || c["temporary"] != false || !strings.HasPrefix(pw, e2ePasswordMark) || len(pw) < 12 || pw == name {
			t.Errorf("user %s: credential must be a non-temporary %s… password of 12+ characters", name, e2ePasswordMark)
		}
	}

	// Everything else is identical to production.
	rest := map[string]any{}
	for k, v := range e2e {
		switch k {
		case "realm", "displayName", "users":
		default:
			rest[k] = v
		}
	}
	prodRest := map[string]any{}
	for k, v := range prod {
		if k != "realm" {
			prodRest[k] = v
		}
	}
	for k := range prodRest {
		if _, ok := rest[k]; !ok {
			t.Errorf("e2e realm lacks production key %s", k)
		}
	}
	for k, v := range rest {
		pv, ok := prodRest[k]
		if !ok {
			t.Errorf("e2e realm adds key %s", k)
			continue
		}
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(pv)
		if string(a) != string(b) {
			t.Errorf("e2e realm key %s differs from production:\n e2e:  %s\n prod: %s", k, a, b)
		}
	}
	// Outside users, the e2e file carries no secret or key material either.
	walk(t, "$", rest)
}

// The e2e realm is mounted only by the e2e override, never by the normal
// stack; and the e2e override does not import the production realm.
func TestE2ERealmIsMountedOnlyByTheE2EStack(t *testing.T) {
	base, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(base), "realm-e2e") || strings.Contains(string(base), e2eRealmName) {
		t.Error("deploy/compose.yml must not reference the e2e realm")
	}
	over, err := os.ReadFile(composeE2EFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(over), "./keycloak/realm-e2e.json:/opt/keycloak/data/import/realm-e2e.json:ro") {
		t.Error("deploy/compose.e2e.yml must mount realm-e2e.json read-only for import")
	}
	if strings.Contains(string(over), "realm-geoint.json") {
		t.Error("deploy/compose.e2e.yml must not import the production realm")
	}
}

// The production values the e2e stack overrides must still name the realm of
// realm-geoint.json, so a typo there cannot hide behind a green e2e run.
func TestComposePointsAtTheProductionRealm(t *testing.T) {
	realm, _ := readJSON(t, realmFile)["realm"].(string)
	if realm == "" {
		t.Fatal("realm-geoint.json has no realm name")
	}
	raw, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"OIDC_ISSUER":        "http://localhost:8081/realms/" + realm,
		"OIDC_DISCOVERY_URL": "http://keycloak:8080/realms/" + realm,
		"WEB_OIDC_AUTHORITY": "http://localhost:8081/realms/" + realm,
	}
	for name, value := range want {
		m := regexp.MustCompile(`(?m)^\s+`+name+`:\s*(\S+)\s*$`).FindAllStringSubmatch(string(raw), -1)
		if len(m) != 1 {
			t.Errorf("deploy/compose.yml sets %s %d times, want once", name, len(m))
			continue
		}
		if m[0][1] != value {
			t.Errorf("deploy/compose.yml %s = %s, want %s", name, m[0][1], value)
		}
	}
}
