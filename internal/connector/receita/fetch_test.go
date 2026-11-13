package receita

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEditionMonthMapping(t *testing.T) {
	cases := []struct {
		ref, month, prefix string
	}{
		{"2026-10-01", "2026-10", "raw/receita/2026-10-01/"},
		{"2026-10-31", "2026-10", "raw/receita/2026-10-01/"},
		{"2026-11-11", "2026-11", "raw/receita/2026-11-01/"},
		{"2027-01-15", "2027-01", "raw/receita/2027-01-01/"},
	}
	for _, c := range cases {
		ref, _ := time.Parse("2006-01-02", c.ref)
		if got := editionMonth(ref); got != c.month {
			t.Errorf("editionMonth(%s) = %s, want %s", c.ref, got, c.month)
		}
		if got := rawPrefix(ref); got != c.prefix {
			t.Errorf("rawPrefix(%s) = %s, want %s", c.ref, got, c.prefix)
		}
	}
}

// davServer serves a WebDAV-like edition folder: PROPFIND lists the files,
// GET serves them with Range support.
type davServer struct {
	*httptest.Server
	month string
	files map[string][]byte

	mu        sync.Mutex
	gets      map[string]int
	ranges    []string
	cutFirst  string // first GET of this file is cut in the middle
	agents    []string
	authUsers []string
}

func newDAV(t *testing.T, month string, files map[string][]byte) *davServer {
	t.Helper()
	d := &davServer{month: month, files: files, gets: map[string]int{}}
	d.Server = httptest.NewServer(http.HandlerFunc(d.handle))
	t.Cleanup(d.Close)
	return d
}

func (d *davServer) handle(w http.ResponseWriter, r *http.Request) {
	user, _, _ := r.BasicAuth()
	d.mu.Lock()
	d.agents = append(d.agents, r.UserAgent())
	d.authUsers = append(d.authUsers, user)
	d.mu.Unlock()
	dir := "/dav/" + d.month + "/"
	switch {
	case r.Method == "PROPFIND" && r.URL.Path == dir && r.Header.Get("Depth") == "1":
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
		fmt.Fprintf(&b, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, dir)
		names := make([]string, 0, len(d.files))
		for n := range d.files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, `<d:response><d:href>%s%s</d:href><d:propstat><d:prop><d:getcontentlength>%d</d:getcontentlength><d:getetag>"%s"</d:getetag><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`,
				dir, n, len(d.files[n]), "etag-"+n)
		}
		b.WriteString(`</d:multistatus>`)
		_, _ = w.Write([]byte(b.String()))
	case r.Method == "PROPFIND":
		http.NotFound(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, dir):
		name := strings.TrimPrefix(r.URL.Path, dir)
		data, ok := d.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		d.mu.Lock()
		d.gets[name]++
		first := d.gets[name] == 1
		if rg := r.Header.Get("Range"); rg != "" {
			d.ranges = append(d.ranges, name+" "+rg)
		}
		d.mu.Unlock()
		if first && name == d.cutFirst {
			// Promise the whole file, send half, drop the connection.
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data[:len(data)/2])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.Header().Set("ETag", `"etag-`+name+`"`)
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	default:
		http.Error(w, "unexpected", http.StatusMethodNotAllowed)
	}
}

// seen returns copies of the GET counts and Range headers, read under the lock.
func (d *davServer) seen() (map[string]int, []string, []string, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	gets := make(map[string]int, len(d.gets))
	for k, v := range d.gets {
		gets[k] = v
	}
	return gets, slices.Clone(d.ranges), slices.Clone(d.agents), slices.Clone(d.authUsers)
}

func fastConnector(t *testing.T, baseURL string) *receita {
	t.Helper()
	c := newReceita(baseURL)
	c.backoff, c.maxBackoff, c.idleTimeout = 10*time.Millisecond, 20*time.Millisecond, 5*time.Second
	// The fixture editions have one part per kind and no dates; the
	// completeness rules are tested with their own settings.
	c.minParts, c.settle = 1, 0
	c.spoolDir = t.TempDir()
	return c
}

func editionZips(t *testing.T) map[string][]byte {
	t.Helper()
	files := fixtureZips(t, nil)
	files["Qualificacoes.zip"] = zipOf(t, "Qualificacoes.csv", []byte("\"49\";\"S\xf3cio-Administrador\"\n"))
	files["Simples.zip"] = []byte("not fetched")
	return files
}

func TestFetchDownloadsResumesAndStores(t *testing.T) {
	ref := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC) // own month: no other test stores these keys
	files := editionZips(t)
	d := newDAV(t, "2031-01", files)
	d.cutFirst = "Socios0.zip"
	c := fastConnector(t, strings.Replace(d.URL, "http://", "http://sharetoken@", 1)+"/dav")
	raw := newRaw(t)

	got, err := c.Fetch(ctx, ref, raw)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	var keys []string
	for _, f := range got {
		keys = append(keys, f.Key)
		if want := files[strings.TrimPrefix(f.Key, "raw/receita/2031-01-01/")]; int64(len(want)) != f.Size {
			t.Errorf("%s: size %d, want %d", f.Key, f.Size, len(want))
		}
	}
	want := "raw/receita/2031-01-01/Empresas0.zip,raw/receita/2031-01-01/Estabelecimentos0.zip," +
		"raw/receita/2031-01-01/Qualificacoes.zip,raw/receita/2031-01-01/Socios0.zip"
	if strings.Join(keys, ",") != want {
		t.Errorf("keys = %v", keys)
	}
	// The cut download was resumed from where it stopped, not restarted.
	half := len(files["Socios0.zip"]) / 2
	_, ranges, agents, users := d.seen()
	if len(ranges) != 1 || ranges[0] != fmt.Sprintf("Socios0.zip bytes=%d-", half) {
		t.Errorf("range requests = %v, want one resuming at %d", ranges, half)
	}
	rc, err := raw.Open(ctx, "raw/receita/2031-01-01/Socios0.zip")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rc)
	rc.Close()
	if !bytes.Equal(buf.Bytes(), files["Socios0.zip"]) {
		t.Error("stored Socios0.zip differs from the served file")
	}
	for i, ua := range agents {
		if strings.Contains(ua, "@") || strings.Contains(strings.ToLower(ua), "geoint") || ua == "" {
			t.Errorf("User-Agent %q", ua)
		}
		if users[i] != "sharetoken" {
			t.Errorf("request %d sent Basic user %q, want the share token", i, users[i])
		}
	}
}

func TestFetchSkipsFilesAlreadyStoredWithSameSize(t *testing.T) {
	ref := time.Date(2031, 2, 1, 0, 0, 0, 0, time.UTC)
	files := editionZips(t)
	d := newDAV(t, "2031-02", files)
	c := fastConnector(t, d.URL+"/dav/")
	raw := newRaw(t)
	if _, err := raw.Put(ctx, "raw/receita/2031-02-01/Empresas0.zip", bytes.NewReader(files["Empresas0.zip"])); err != nil {
		t.Fatal(err)
	}
	got, err := c.Fetch(ctx, ref, raw)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("files = %d, want 4", len(got))
	}
	if gets, _, _, _ := d.seen(); gets["Empresas0.zip"] != 0 || gets["Socios0.zip"] != 1 {
		t.Errorf("GETs = %v: the stored file must not be downloaded again", gets)
	}
	// Second Fetch downloads nothing.
	if _, err := c.Fetch(ctx, ref, raw); err != nil {
		t.Fatal(err)
	}
	if gets, _, _, _ := d.seen(); gets["Socios0.zip"] != 1 {
		t.Errorf("GETs after refetch = %v", gets)
	}
}

func TestFetchUnpublishedMonthFails(t *testing.T) {
	d := newDAV(t, "2031-03", editionZips(t))
	c := fastConnector(t, d.URL+"/dav/")
	_, err := c.Fetch(ctx, time.Date(2031, 4, 2, 0, 0, 0, 0, time.UTC), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "2031-04") || !strings.Contains(err.Error(), "not published") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchRefusesIncompleteEdition(t *testing.T) {
	files := editionZips(t)
	files["Socios2.zip"] = files["Socios0.zip"] // gap: no Socios1
	d := newDAV(t, "2031-05", files)
	_, err := fastConnector(t, d.URL+"/dav/").Fetch(ctx, time.Date(2031, 5, 1, 0, 0, 0, 0, time.UTC), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "Socios") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchRefusesRedirectToAnotherHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed a redirect to another host")
	}))
	defer other.Close()
	d := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer d.Close()
	_, err := fastConnector(t, d.URL+"/dav/").Fetch(ctx, time.Date(2031, 6, 1, 0, 0, 0, 0, time.UTC), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "another host") {
		t.Errorf("err = %v", err)
	}
}
