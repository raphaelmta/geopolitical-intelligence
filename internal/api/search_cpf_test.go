package api_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"geoint/internal/graph/graphtest"
)

// Synthetic CPFs only: 123.456.789-09 and 111.222.333-96 have valid check
// digits; 123.456.789-01 does not.

type searchHit struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Match string `json:"match"`
}

type searchBody struct {
	Items             []searchHit `json:"items"`
	CPFFragmentSearch *bool       `json:"cpf_fragment_search"`
}

func TestSearchByCPFFragment(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	b, rc := graphtest.FreshReceita(t, e.pool)
	b.Company(rc, "70000002", "EMPRESA FICTICIA 123.456.789-09 ME", "2135")
	_, maria := b.Person(rc, "70000002", "MARIA FICTICIA", "456789")
	_, joao := b.Person(rc, "70000003", "JOAO FICTICIO 12345678909", "456789")
	b.Person(rc, "70000004", "MARIA OUTRA", "111222")
	e.rebuildSearch()

	get := func(q, kind string) (*searchBody, string) {
		t.Helper()
		path := "/api/v1/search?q=" + url.QueryEscape(q)
		if kind != "" {
			path += "&kind=" + kind
		}
		rec := e.do("GET", path, tokAnalyst, "")
		raw := rec.Body.String()
		got := want[searchBody](t, rec, http.StatusOK)
		return &got, raw
	}

	for _, q := range []string{"123.456.789-09", "12345678909", "***456789**"} {
		got, raw := get(q, "")
		if got.CPFFragmentSearch == nil || !*got.CPFFragmentSearch || len(got.Items) != 2 {
			t.Fatalf("q=%q: %s", q, raw)
		}
		seen := map[int64]bool{}
		for _, it := range got.Items {
			seen[it.ID] = true
			if it.Kind != "person" || it.Match != "cpf_fragment" {
				t.Errorf("q=%q: hit %+v", q, it)
			}
		}
		if !seen[maria] || !seen[joao] {
			t.Errorf("q=%q: %s", q, raw)
		}
		// Neither the CPF nor its fragment comes back; names stay masked.
		if strings.Contains(raw, "12345678909") || strings.Contains(raw, "123.456.789-09") || strings.Contains(raw, `"456789"`) ||
			!strings.Contains(raw, "JOAO FICTICIO ***456789**") {
			t.Errorf("q=%q: %s", q, raw)
		}
	}
	// CPF and name words: the intersection.
	got, raw := get("maria 123.456.789-09", "")
	if len(got.Items) != 1 || got.Items[0].ID != maria || got.Items[0].Match != "cpf_fragment" {
		t.Errorf("maria + cpf: %s", raw)
	}
	// Scope company: nothing, still flagged.
	got, raw = get("123.456.789-09", "company")
	if len(got.Items) != 0 || got.CPFFragmentSearch == nil || !*got.CPFFragmentSearch {
		t.Errorf("kind company: %s", raw)
	}
	// A name search keeps its old shape: no flag, no match.
	_, raw = get("maria ficticia", "")
	if strings.Contains(raw, "cpf_fragment") || strings.Contains(raw, "match") {
		t.Errorf("name search: %s", raw)
	}
}

func TestSearchCPFErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	e.rebuildSearch()
	for q, code := range map[string]string{
		"123.456.789-01":                "invalid_cpf",
		"maria 12345678901":             "invalid_cpf",
		"000.000.000-00":                "invalid_cpf",
		"123.456.789-09 111.222.333-96": "multiple_cpfs",
		"maria ***456789** 11122233396": "multiple_cpfs",
	} {
		rec := e.do("GET", "/api/v1/search?q="+url.QueryEscape(q), tokAnalyst, "")
		wantError(t, rec, http.StatusBadRequest, code)
	}
	// A refused search writes no audit row.
	if n := e.auditCount("search", nil); n != 0 {
		t.Errorf("%d audit rows for refused searches", n)
	}
}

func TestSearchByCPFAuditHasNoCPF(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	b, rc := graphtest.FreshReceita(t, e.pool)
	b.Person(rc, "70000005", "MARIA FICTICIA", "456789")
	e.rebuildSearch()
	want[searchBody](t, e.do("GET", "/api/v1/search?q="+url.QueryEscape("maria 123.456.789-09")+"&kind=person", tokAnalyst, ""), http.StatusOK)
	var raw string
	var detail map[string]any
	if err := e.pool.QueryRow(bg, `SELECT detail::text, detail FROM audit_log WHERE action = 'search'`).Scan(&raw, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail) != 3 || detail["kind"] != "person" || detail["count"] != float64(1) || detail["cpf_fragment_search"] != true {
		t.Fatalf("audit %s, want kind, count and cpf_fragment_search only", raw)
	}
	for _, s := range []string{"456789", "123", "maria", "MARIA"} {
		if strings.Contains(raw, s) {
			t.Errorf("audit detail %s holds %q", raw, s)
		}
	}
}
