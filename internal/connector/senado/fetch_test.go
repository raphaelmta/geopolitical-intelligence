package senado

import (
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"geoint/internal/connector/legis"
)

// The Senado API has no pagination: a legislature's list comes whole, and
// every senator's affiliations come in one response. "Every page" is thus
// every legislature list and the affiliations of every senator who was in
// office in one of them, each fetched once.
func TestPaginationFollowsAllPages(t *testing.T) {
	a, srv := newAPI(t)
	c := fastConnector(t, srv)
	ref := newRef()
	files, err := c.Fetch(ctx, ref, newRaw(t))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, path.Base(f.Key))
		if !strings.HasPrefix(f.Key, "raw/senado/"+ref.Format(time.DateOnly)+"/") {
			t.Errorf("key %s outside the collection's prefix", f.Key)
		}
	}
	// 5918 never took office (no Exercicios): his affiliations are not needed.
	want := []string{"senadores_56.json", "senadores_57.json", "senador_751_filiacoes.json", "senador_4605_filiacoes.json",
		"senador_4642_filiacoes.json", "senador_5016_filiacoes.json", "senador_5322_filiacoes.json", "senador_6358_filiacoes.json",
		"senador_6359_filiacoes.json", "senador_6366_filiacoes.json", "senador_6369_filiacoes.json"}
	if !slices.Equal(names, want) {
		t.Errorf("files\n got %v\nwant %v", names, want)
	}
	for _, p := range []string{"/dadosabertos/senador/lista/legislatura/56?", "/dadosabertos/senador/lista/legislatura/57?",
		"/dadosabertos/senador/751/filiacoes?", "/dadosabertos/senador/4642/filiacoes?"} {
		if n := a.count(p); n != 1 {
			t.Errorf("%s requested %d times, want 1", p, n)
		}
	}
	if n := a.count("/dadosabertos/senador/5918/filiacoes?"); n != 0 {
		t.Errorf("affiliations of a senator never in office requested")
	}
}

func TestRateLimitResponseIsRetried(t *testing.T) {
	a, srv := newAPI(t)
	limited := 0
	a.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/dadosabertos/senador/lista/legislatura/56" && limited == 0 {
			limited++
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return true
		}
		return false
	}
	if _, err := fastConnector(t, srv).Fetch(ctx, newRef(), newRaw(t)); err != nil {
		t.Fatalf("Fetch after a 429: %v", err)
	}
	if n := a.count("/dadosabertos/senador/lista/legislatura/56?"); n != 2 {
		t.Errorf("list 56 requested %d times, want 2 (429 then 200)", n)
	}
}

func TestFetchRefusesForeignRedirect(t *testing.T) {
	a, srv := newAPI(t)
	a.before = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, "http://example.invalid/x", http.StatusFound)
		return true
	}
	if _, err := fastConnector(t, srv).Fetch(ctx, newRef(), newRaw(t)); !errors.Is(err, legis.ErrForeignRedirect) {
		t.Fatalf("got %v, want a refused redirect", err)
	}
	if n := a.count("/dadosabertos/senador/lista/legislatura/56?"); n != 1 {
		t.Errorf("a refused redirect was retried (%d requests)", n)
	}
}

func TestFetchDoesNotStoreAMalformedResponse(t *testing.T) {
	a, srv := newAPI(t)
	good := a.files["senadores_57.json"]
	a.files["senadores_57.json"] = []byte(`{"ListaParlamentarLegislatura": {}}`)
	c := fastConnector(t, srv)
	raw := newRaw(t)
	ref := newRef()
	var se *legis.ShapeError
	if _, err := c.Fetch(ctx, ref, raw); !errors.As(err, &se) {
		t.Fatalf("got %v, want a shape error", err)
	}
	if ok, err := raw.Exists(ctx, rawPrefix(ref)+"senadores_57.json"); err != nil || ok {
		t.Fatalf("the malformed response was stored (%v, %v)", ok, err)
	}
	a.files["senadores_57.json"] = good
	if _, err := c.Fetch(ctx, ref, raw); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
}

func TestFetchFailsOnUnexpectedShape(t *testing.T) {
	a, srv := newAPI(t)
	a.files["senadores_57.json"] = []byte(`{"ListaParlamentarLegislatura": {"Metadados": {}}}`)
	_, err := fastConnector(t, srv).Fetch(ctx, newRef(), newRaw(t))
	var se *legis.ShapeError
	if !errors.As(err, &se) {
		t.Fatalf("got %v, want a shape error", err)
	}
}
