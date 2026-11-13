package receita

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"geoint/internal/connector"
)

var completeEdition = []string{"Empresas0.zip", "Estabelecimentos0.zip", "Socios0.zip", "Qualificacoes.zip"}

// editionsDAV serves a WebDAV base folder with month subfolders (name →
// file names) plus other entries; only listings are served. lastMod gives
// getlastmodified per "month/file". Fields may change between requests.
type editionsDAV struct {
	*httptest.Server
	mu       sync.Mutex
	folders  map[string][]string
	files    []string
	lastMod  map[string]string
	sizes    map[string]int // "month/file" → size (default 10)
	listings atomic.Int32
}

func newEditionsDAV(t *testing.T, folders map[string][]string, files []string) *editionsDAV {
	t.Helper()
	d := &editionsDAV{folders: folders, files: files, lastMod: map[string]string{}, sizes: map[string]int{}}
	d.Server = httptest.NewServer(http.HandlerFunc(d.handle))
	t.Cleanup(d.Close)
	return d
}

// editionsServer is newEditionsDAV for tests that need only the server.
func editionsServer(t *testing.T, folders map[string][]string, files []string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	d := newEditionsDAV(t, folders, files)
	return d.Server, &d.listings
}

func (d *editionsDAV) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PROPFIND" || r.Header.Get("Depth") != "1" {
		http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		return
	}
	d.listings.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	dir := func(href string) string {
		return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href)
	}
	file := func(href, key string) string {
		size, ok := d.sizes[key]
		if !ok {
			size = 10
		}
		lm := ""
		if v, ok := d.lastMod[key]; ok {
			lm = "<d:getlastmodified>" + v + "</d:getlastmodified>"
		}
		return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getcontentlength>%d</d:getcontentlength><d:getetag>"e"</d:getetag>%s<d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href, size, lm)
	}
	var b strings.Builder
	switch p := r.URL.Path; {
	case p == "/dav/":
		b.WriteString(dir("/dav/"))
		for name := range d.folders {
			b.WriteString(dir("/dav/" + name + "/"))
		}
		for _, f := range d.files {
			b.WriteString(file("/dav/"+f, f))
		}
	case strings.HasPrefix(p, "/dav/"):
		name := strings.Trim(strings.TrimPrefix(p, "/dav/"), "/")
		list, ok := d.folders[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b.WriteString(dir(p))
		for _, f := range list {
			b.WriteString(file(p+f, name+"/"+f))
		}
	default:
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">%s</d:multistatus>`, b.String())
}

func TestResolveReferencePicksNewestCompleteEditionUpToTheRequestedMonth(t *testing.T) {
	srv, _ := editionsServer(t, map[string][]string{
		"2026-09": completeEdition, "2026-10": completeEdition, "2026-11": completeEdition,
		"2026-13": completeEdition, "Outros": completeEdition, "202612": completeEdition,
	}, []string{"cnpj.tar.gz", "2026-12"})
	c := fastConnector(t, srv.URL+"/dav/")
	for requested, want := range map[string]string{
		"2026-10-20": "2026-10-01", // 2026-11 is after the requested month
		"2026-11-12": "2026-11-01",
		"2026-12-31": "2026-11-01", // "2026-12" is a file, "2026-13" and "202612" are not months
		"2026-09-01": "2026-09-01",
	} {
		ref, _ := time.Parse(time.DateOnly, requested)
		got, err := c.ResolveReference(ctx, ref, nil)
		if err != nil {
			t.Fatalf("%s: %v", requested, err)
		}
		if got.ReferenceDate.Format(time.DateOnly) != want || got.ReferenceDate.Location() != time.UTC {
			t.Errorf("ResolveReference(%s) = %v, want %s UTC", requested, got, want)
		}
	}
}

func TestResolveReferenceSkipsAnIncompleteNewestMonth(t *testing.T) {
	srv, _ := editionsServer(t, map[string][]string{
		"2026-10": completeEdition,
		"2026-11": {"Empresas0.zip", "Estabelecimentos0.zip"}, // Socios not uploaded yet
	}, nil)
	got, err := fastConnector(t, srv.URL+"/dav/").ResolveReference(ctx, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), nil)
	if err != nil || !got.ReferenceDate.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v, %v; want 2026-10-01 (the November folder is half uploaded)", got, err)
	}
}

func TestResolveReferenceWithoutEditions(t *testing.T) {
	cases := map[string]map[string][]string{
		"no month folders":      {"Outros": completeEdition},
		"only later months":     {"2027-01": completeEdition},
		"only incomplete month": {"2026-11": {"Socios0.zip"}},
	}
	for name, folders := range cases {
		srv, _ := editionsServer(t, folders, []string{"cnpj.tar.gz"})
		_, err := fastConnector(t, srv.URL+"/dav/").ResolveReference(ctx, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), nil)
		if !errors.Is(err, connector.ErrNoEdition) {
			t.Errorf("%s: err = %v, want ErrNoEdition", name, err)
		}
	}
}

func TestResolveReferenceListingErrorIsNotNoEdition(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := fastConnector(t, srv.URL+"/dav/")
	_, err := c.ResolveReference(ctx, time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), nil)
	if err == nil || errors.Is(err, connector.ErrNoEdition) || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want the listing error", err)
	}
	if int(calls.Load()) != c.attempts {
		t.Errorf("listing attempts = %d, want %d (bounded retries)", calls.Load(), c.attempts)
	}
}

func TestResolveReferenceListsAFewMonthsAtMost(t *testing.T) {
	folders := map[string][]string{}
	for m := 1; m <= 12; m++ {
		folders[fmt.Sprintf("2026-%02d", m)] = []string{"Socios0.zip"} // all incomplete
	}
	srv, listings := editionsServer(t, folders, nil)
	_, err := fastConnector(t, srv.URL+"/dav/").ResolveReference(ctx, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), nil)
	if !errors.Is(err, connector.ErrNoEdition) {
		t.Errorf("err = %v", err)
	}
	if got := int(listings.Load()); got != 1+maxEditionChecks {
		t.Errorf("listings = %d, want %d (base + newest %d months)", got, 1+maxEditionChecks, maxEditionChecks)
	}
}
