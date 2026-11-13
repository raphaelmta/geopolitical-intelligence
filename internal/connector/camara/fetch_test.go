package camara

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

func TestPaginationFollowsAllPages(t *testing.T) {
	a, srv := newAPI(t)
	c := fastConnector(t, srv)
	ref := newRef()
	files, err := c.Fetch(ctx, ref, newRaw(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"/api/v2/deputados?idLegislatura=57&itens=100&ordem=ASC&ordenarPor=id&pagina=1",
		"/api/v2/deputados?idLegislatura=57&itens=100&ordem=ASC&ordenarPor=id&pagina=2",
		"/api/v2/deputados?idLegislatura=56&itens=100&ordem=ASC&ordenarPor=id&pagina=1",
		"/api/v2/partidos?idLegislatura=56&itens=100&ordem=ASC&ordenarPor=sigla&pagina=1",
		"/api/v2/partidos?idLegislatura=57&itens=100&ordem=ASC&ordenarPor=sigla&pagina=1",
	} {
		if n := a.count(q); n != 1 {
			t.Errorf("%s requested %d times, want 1", q, n)
		}
	}
	// Page 2 has no "next" link: no page 3.
	if n := a.count("/api/v2/deputados?idLegislatura=57&itens=100&ordem=ASC&ordenarPor=id&pagina=3"); n != 0 {
		t.Errorf("page 3 requested %d times", n)
	}
	var names []string
	for _, f := range files {
		names = append(names, path.Base(f.Key))
		if !strings.HasPrefix(f.Key, "raw/camara/"+ref.Format(time.DateOnly)+"/") {
			t.Errorf("key %s outside the collection's prefix", f.Key)
		}
	}
	// 100689 is only on page 1 of 57, 204511 only on page 2: every deputy
	// of every page is fetched, once.
	want := []string{
		"legislaturas_56.json", "partidos_56_p1.json", "deputados_56_p1.json",
		"legislaturas_57.json", "partidos_57_p1.json", "deputados_57_p1.json", "deputados_57_p2.json",
	}
	for _, id := range fixtureDeputies {
		want = append(want, "deputado_"+id+".json", "deputado_"+id+"_historico.json")
	}
	if !slices.Equal(names, want) {
		t.Errorf("files\n got %v\nwant %v", names, want)
	}
	for _, id := range fixtureDeputies {
		if a.count("/api/v2/deputados/"+id+"?") != 1 || a.count("/api/v2/deputados/"+id+"/historico?") != 1 {
			t.Errorf("deputy %s not fetched exactly once", id)
		}
	}
}

// fixtureDeputies are the deputies of testdata/camara, in id order.
var fixtureDeputies = []string{"74159", "74283", "100689", "204379", "204503", "204511"}

func TestFetchReusesStoredResponses(t *testing.T) {
	a, srv := newAPI(t)
	c := fastConnector(t, srv)
	raw := newRaw(t)
	ref := newRef()
	first, err := c.Fetch(ctx, ref, raw)
	if err != nil {
		t.Fatal(err)
	}
	a.edit(t, "deputado_204379.json", func(doc map[string]any) { doc["dados"].(map[string]any)["nomeCivil"] = "OUTRO NOME" })
	before := len(a.calls)
	second, err := c.Fetch(ctx, ref, raw)
	if err != nil {
		t.Fatalf("re-fetch of the same reference date: %v", err)
	}
	if len(a.calls) != before {
		t.Errorf("re-fetch sent new requests")
	}
	if !slices.Equal(first, second) {
		t.Errorf("re-fetch returned other files")
	}
}

func TestRateLimitResponseIsRetried(t *testing.T) {
	a, srv := newAPI(t)
	limited := 0
	a.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v2/legislaturas/56" && limited == 0 {
			limited++
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return true
		}
		return false
	}
	c := fastConnector(t, srv)
	if _, err := c.Fetch(ctx, newRef(), newRaw(t)); err != nil {
		t.Fatalf("Fetch after a 429: %v", err)
	}
	if n := a.count("/api/v2/legislaturas/56?"); n != 2 {
		t.Errorf("legislaturas/56 requested %d times, want 2 (429 then 200)", n)
	}
}

func TestFetchGivesUpAfterBoundedRetries(t *testing.T) {
	a, srv := newAPI(t)
	a.before = func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return true
	}
	c := fastConnector(t, srv)
	_, err := c.Fetch(ctx, newRef(), newRaw(t))
	if err == nil {
		t.Fatal("Fetch succeeded against a server that is down")
	}
	if n := a.count("/api/v2/legislaturas/56?"); n != c.attempts {
		t.Errorf("%d attempts, want %d", n, c.attempts)
	}
}

func TestFetchDoesNotStoreAMalformedResponse(t *testing.T) {
	a, srv := newAPI(t)
	good := a.files["deputado_204379_historico.json"]
	a.files["deputado_204379_historico.json"] = []byte(`{"dados": "fora do ar"}`)
	c := fastConnector(t, srv)
	raw := newRaw(t)
	ref := newRef()
	var se *legis.ShapeError
	if _, err := c.Fetch(ctx, ref, raw); !errors.As(err, &se) {
		t.Fatalf("got %v, want a shape error", err)
	}
	if ok, err := raw.Exists(ctx, rawPrefix(ref)+"deputado_204379_historico.json"); err != nil || ok {
		t.Fatalf("the malformed response was stored (%v, %v)", ok, err)
	}
	// The same reference date is not poisoned: once the API answers well,
	// the fetch completes.
	a.files["deputado_204379_historico.json"] = good
	if _, err := c.Fetch(ctx, ref, raw); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
}

func TestFetchSkipsOneUnavailableDeputy(t *testing.T) {
	a, srv := newAPI(t)
	delete(a.files, "deputado_100689_historico.json") // 404
	files, err := fastConnector(t, srv).Fetch(ctx, newRef(), newRaw(t))
	if err != nil {
		t.Fatalf("a 404 for one deputy failed the fetch: %v", err)
	}
	var names []string
	for _, f := range files {
		names = append(names, path.Base(f.Key))
	}
	if !slices.Contains(names, "deputado_100689_historico.unavailable.json") || slices.Contains(names, "deputado_100689_historico.json") {
		t.Errorf("files %v: want the unavailable marker instead of the history", names)
	}
	// A deputy whose record is unavailable: his history is not requested.
	delete(a.files, "deputado_74283.json")
	files, err = fastConnector(t, srv).Fetch(ctx, newRef(), newRaw(t))
	if err != nil {
		t.Fatal(err)
	}
	if n := a.count("/api/v2/deputados/74283/historico?"); n != 1 { // only by the first fetch
		t.Errorf("history requested %d times, want 1 (not after the record's 404)", n)
	}
	names = names[:0]
	for _, f := range files {
		names = append(names, path.Base(f.Key))
	}
	if !slices.Contains(names, "deputado_74283.unavailable.json") || slices.Contains(names, "deputado_74283_historico.json") {
		t.Errorf("files %v", names)
	}
	// A 404 on a list stays fatal.
	delete(a.files, "deputados_57_p2.json")
	if _, err := fastConnector(t, srv).Fetch(ctx, newRef(), newRaw(t)); !legis.ClientError(err) {
		t.Fatalf("got %v, want the list's 404", err)
	}
}

// The test server is http, so this checks http -> https; the https ->
// http direction is checked in legis (TestRedirectKeepsHostAndScheme).
func TestFetchRefusesRedirectToAnotherScheme(t *testing.T) {
	a, srv := newAPI(t)
	a.before = func(w http.ResponseWriter, r *http.Request) bool {
		// Same host, other scheme.
		http.Redirect(w, r, "https://"+r.Host+r.URL.Path, http.StatusFound)
		return true
	}
	if _, err := fastConnector(t, srv).Fetch(ctx, newRef(), newRaw(t)); !errors.Is(err, legis.ErrForeignRedirect) {
		t.Fatalf("got %v, want a refused redirect", err)
	}
}

func TestFetchRefusesForeignRedirect(t *testing.T) {
	a, srv := newAPI(t)
	a.before = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, "http://example.invalid/x", http.StatusFound)
		return true
	}
	_, err := fastConnector(t, srv).Fetch(ctx, newRef(), newRaw(t))
	if !errors.Is(err, legis.ErrForeignRedirect) {
		t.Fatalf("got %v, want a refused redirect", err)
	}
	if n := a.count("/api/v2/legislaturas/56?"); n != 1 {
		t.Errorf("a refused redirect was retried (%d requests)", n)
	}
}

func TestFetchCapsResponseSize(t *testing.T) {
	_, srv := newAPI(t)
	c := fastConnector(t, srv)
	c.maxBody = 100
	if _, err := c.Fetch(ctx, newRef(), newRaw(t)); !errors.Is(err, legis.ErrTooLarge) {
		t.Fatalf("got %v, want a size error", err)
	}
}

func TestFetchCapsRequests(t *testing.T) {
	_, srv := newAPI(t)
	c := fastConnector(t, srv)
	c.maxRequests = 3
	if _, err := c.Fetch(ctx, newRef(), newRaw(t)); !errors.Is(err, legis.ErrTooManyRequests) {
		t.Fatalf("got %v, want the request budget error", err)
	}
}

func TestFetchFailsOnUnexpectedShape(t *testing.T) {
	a, srv := newAPI(t)
	a.files["deputados_57_p1.json"] = []byte(`{"erro": "fora do ar"}`)
	_, err := fastConnector(t, srv).Fetch(ctx, newRef(), newRaw(t))
	var se *legis.ShapeError
	if !errors.As(err, &se) {
		t.Fatalf("got %v, want a shape error", err)
	}
}
