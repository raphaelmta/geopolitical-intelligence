package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestSummarizeNearestRank(t *testing.T) {
	seq := func(n int) []float64 {
		v := make([]float64, n)
		for i := range v {
			v[n-1-i] = float64(i + 1) // unsorted on purpose
		}
		return v
	}
	for _, c := range []struct {
		n             int
		p50, p95, max float64
	}{
		{1, 1, 1, 1},
		{5, 3, 5, 5},
		{20, 10, 19, 20},
		{240, 120, 228, 240},
	} {
		s := summarize(seq(c.n), 2)
		if s.N != c.n || s.P50 != c.p50 || s.P95 != c.p95 || s.Max != c.max || s.Errors != 2 {
			t.Errorf("n=%d: %+v, want p50 %v p95 %v max %v errors 2", c.n, s, c.p50, c.p95, c.max)
		}
	}
	if s := summarize(nil, 3); s.N != 0 || s.Errors != 3 {
		t.Errorf("empty: %+v", s)
	}
}

func TestGroupKeepsErrorsOutOfThePercentiles(t *testing.T) {
	items := []sample{
		{Group: "a", Status: 200, Ms: 10}, {Group: "a", Status: 500, Ms: 99999}, {Group: "b", Status: 200, Ms: 5},
	}
	keys, by := group(items, func(s sample) string { return s.Group }, func(s sample) (float64, bool) { return s.Ms, s.Status == 200 })
	if strings.Join(keys, ",") != "a,b" {
		t.Fatalf("keys %v", keys)
	}
	if a := by["a"]; a.N != 1 || a.Max != 10 || a.Errors != 1 {
		t.Errorf("a: %+v", a)
	}
}

func searchResults(extra ...sample) *results {
	r := &results{Pass: "t"}
	for i := 0; i < 19; i++ {
		r.Search = append(r.Search, sample{Group: "fixed", Scope: "all", Status: http.StatusOK, Ms: 100})
	}
	r.Search = append(r.Search, extra...)
	return r
}

func TestSearchVerdict(t *testing.T) {
	// A designed refusal counts as an answer, timed.
	md := searchResults(sample{Group: "fixed", Scope: "all", Status: http.StatusBadRequest, Error: "status 400 query_too_short", Ms: 2}).markdown()
	if !strings.Contains(md, "| 0 | passou |") || !strings.Contains(md, "Recusadas por desenho (400 `query_too_short`, contadas como resposta): 1.") {
		t.Errorf("refused search not counted as an answer:\n%s", md)
	}
	// Any other error fails the target.
	md = searchResults(sample{Group: "fixed", Scope: "all", Status: http.StatusInternalServerError, Error: "status 500"}).markdown()
	if !strings.Contains(md, "**FALHOU**") {
		t.Errorf("an error did not fail the target:\n%s", md)
	}
	// A p95 above 1 s fails it; adverse terms are outside the target set.
	slow := searchResults()
	slow.Search = append(slow.Search, sample{Group: "fixed", Scope: "all", Status: 200, Ms: 1500}, sample{Group: "fixed", Scope: "all", Status: 200, Ms: 1500})
	if md := slow.markdown(); !strings.Contains(md, "**FALHOU**") {
		t.Errorf("p95 > 1 s passed:\n%s", md)
	}
	adverse := searchResults(sample{Group: "adverse", Scope: "all", Status: 200, Ms: 5000}, sample{Group: "adverse", Scope: "all", Status: 200, Ms: 5000})
	if md := adverse.markdown(); strings.Contains(md, "**FALHOU**") {
		t.Errorf("adverse terms counted in the target:\n%s", md)
	}
}

func TestJobVerdict(t *testing.T) {
	r := &results{Paths: []jobResult{{Group: "g", Status: "done", ClientMs: 1000}, {Group: "g", Status: "done", ClientMs: 119000}}}
	if md := r.markdown(); !strings.Contains(md, "→ passou") {
		t.Errorf("jobs within 120 s:\n%s", md)
	}
	r.Paths = append(r.Paths, jobResult{Group: "g", Status: "failed", Error: "timeout"})
	if md := r.markdown(); !strings.Contains(md, "**FALHOU**") {
		t.Errorf("a failed job did not fail:\n%s", md)
	}
	r = &results{Control: []jobResult{{Group: "g", Mode: "documented", Status: "done", ClientMs: 121000}}}
	if md := r.markdown(); !strings.Contains(md, "**FALHOU**") {
		t.Errorf("a job over 120 s passed:\n%s", md)
	}
}

func TestLabelNamesOnlyAllowedEntities(t *testing.T) {
	public := map[int64]bool{1: true}
	for _, c := range []struct {
		kind, want string
		id         int64
	}{
		{"person", `person "NOME" #1`, 1},
		{"company", `company "NOME" #1`, 1},
		{"person", "pessoa #2", 2},
		{"company", "empresa #2", 2},
		{"foreign", "estrangeiro #2", 2},
		{"party", `party "NOME" #2`, 2},
		{"public_body", `public_body "NOME" #2`, 2},
		{"something_new", "entidade #2", 2},
	} {
		if got := label(c.kind, "NOME", c.id, public); got != c.want {
			t.Errorf("label(%s, #%d) = %s, want %s", c.kind, c.id, got, c.want)
		}
		if !public[c.id] && c.kind != "party" && c.kind != "public_body" && strings.Contains(label(c.kind, "NOME", c.id, public), "NOME") {
			t.Error(fmt.Sprint("name leaked for ", c.kind))
		}
	}
}

func TestStateFileTracksOnlyCreatedUsers(t *testing.T) {
	t.Setenv("MEASURE_STATE", t.TempDir()+"/state")
	if u, err := readState(); err != nil || len(u) != 0 {
		t.Fatalf("empty state: %v %v", u, err)
	}
	_ = remember(tempUser{ID: "id-1", Username: "measure-a"})
	_ = remember(tempUser{ID: "id-2", Username: "measure-b"})
	forget("id-1")
	u, err := readState()
	if err != nil || len(u) != 1 || u[0].ID != "id-2" || u[0].Username != "measure-b" {
		t.Fatalf("state after forget: %v %v", u, err)
	}
}
