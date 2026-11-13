package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReasonCodesCoverTheResolver reads the reason texts the resolver writes
// (the CASE of its INSERT INTO identity_proposal) and fails when one of them
// maps to "other", or when the codes no longer cover them one to one.
func TestReasonCodesCoverTheResolver(t *testing.T) {
	src, err := os.ReadFile("../identity/resolver.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "INSERT INTO identity_proposal")
	if start < 0 {
		t.Fatal("resolver.go: no INSERT INTO identity_proposal")
	}
	end := strings.Index(text[start:], "coalesce(c.c, 0)")
	if end < 0 {
		t.Fatal("resolver.go: the reason CASE of the proposal INSERT was not found")
	}
	stmt := text[start : start+end]
	lits := regexp.MustCompile(`(?:THEN|ELSE format\()\s*'([^']*)'`).FindAllStringSubmatch(stmt, -1)
	if len(lits) < 3 {
		t.Fatalf("found %d reason literals in the proposal INSERT, want at least 3:\n%s", len(lits), stmt)
	}
	seen := map[string]string{}
	for _, l := range lits {
		reason := strings.ReplaceAll(l[1], "%s", "12")
		code := reasonCode(reason)
		if code == reasonOther {
			t.Errorf("resolver reason %q has no reason code", reason)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("reasons %q and %q share the code %s", prev, reason, code)
		}
		seen[code] = reason
	}
	if reasonCode("something else") != reasonOther {
		t.Error("an unknown reason is not other")
	}
}
