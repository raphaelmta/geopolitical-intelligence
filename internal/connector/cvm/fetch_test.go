package cvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"geoint/internal/collection"
	"geoint/internal/rawstore"
)

// portal serves the CVM open-data folder layout (HEAD and GET with Range).
type portal struct {
	*httptest.Server
	files map[string][]byte // by name under /dados/

	mu       sync.Mutex
	gets     map[string]int
	ranges   []string
	agents   []string
	cutFirst string // the first GET of this name is cut in the middle
	status   int    // when set, every request answers it
	// version is part of every ETag (a regenerated file gets a new one);
	// noValidators sends neither ETag nor Last-Modified.
	version      string
	noValidators bool
	// getVersion, when set, replaces version in the ETag of GET answers only.
	getVersion string
}

func newPortal(t *testing.T, files map[string][]byte) *portal {
	t.Helper()
	// Each test's files get their own ETags, so tests never share raw keys.
	d := &portal{files: files, gets: map[string]int{}, version: t.Name() + "-"}
	d.Server = httptest.NewServer(http.HandlerFunc(d.handle))
	t.Cleanup(d.Close)
	return d
}

func (d *portal) handle(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.agents = append(d.agents, r.UserAgent())
	status := d.status
	d.mu.Unlock()
	if status != 0 {
		http.Error(w, "status", status)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/dados/")
	data, ok := d.files[p]
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.mu.Lock()
	first := false
	if r.Method == http.MethodGet {
		d.gets[p]++
		first = d.gets[p] == 1
		if rg := r.Header.Get("Range"); rg != "" {
			d.ranges = append(d.ranges, p+" "+rg)
		}
	}
	d.mu.Unlock()
	if r.Method == http.MethodGet && first && p == d.cutFirst {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[:len(data)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
		return
	}
	d.mu.Lock()
	version, noValidators := d.version, d.noValidators
	if r.Method == http.MethodGet && d.getVersion != "" {
		version = d.getVersion
	}
	d.mu.Unlock()
	if !noValidators {
		w.Header().Set("ETag", `"etag-`+version+p+`"`)
	}
	http.ServeContent(w, r, p, time.Time{}, bytes.NewReader(data))
}

func (d *portal) seen() (gets map[string]int, ranges, agents []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	gets = make(map[string]int, len(d.gets))
	for k, v := range d.gets {
		gets[k] = v
	}
	return gets, slices.Clone(d.ranges), slices.Clone(d.agents)
}

func fastConnector(t *testing.T, baseURL string) *cvm {
	t.Helper()
	c := newCVM(baseURL)
	c.backoff, c.maxBackoff, c.idleTimeout = 10*time.Millisecond, 20*time.Millisecond, 5*time.Second
	c.spoolDir = t.TempDir()
	return c
}

// fetchRef returns a reference date in 2026 (so the fixture years are
// fetched) that no other fetch test uses: raw keys depend on it.
func fetchRef(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }

func TestYearsAndRawPrefix(t *testing.T) {
	c := newCVM("")
	if c.baseURL != DefaultBaseURL {
		t.Errorf("base URL %s", c.baseURL)
	}
	if got := c.years(time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)); !slices.Equal(got, fixtureYears) {
		t.Errorf("years = %v, want %v", got, fixtureYears)
	}
	if got := remoteName(2026); got != "fre_cia_aberta_2026.zip" {
		t.Errorf("remote name %s", got)
	}
	if p := rawPrefix(time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)); p != "raw/cvm/2026-11-11/" {
		t.Errorf("rawPrefix = %s", p)
	}
}

func TestFetchDownloadsResumesAndStores(t *testing.T) {
	ref := fetchRef(1)
	files := fixtureZips(t, nil)
	d := newPortal(t, files)
	d.cutFirst = "fre_cia_aberta_2026.zip"
	c := fastConnector(t, d.URL+"/dados")
	raw := newRaw(t)

	got, err := c.Fetch(ctx, ref, raw)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("files = %d, want 2", len(got))
	}
	byName := map[string]string{}
	for _, f := range got {
		name := path.Base(f.Key)
		byName[name] = f.Key
		if parts := strings.Split(f.Key, "/"); len(parts) != 5 || parts[1] != "cvm" || name != "fre_cia_aberta_"+parts[2]+".zip" || len(parts[3]) != 16 {
			t.Errorf("key %s is not raw/cvm/<year>/<validators id>/<zip>", f.Key)
		}
		if want := files[name]; want == nil || int64(len(want)) != f.Size {
			t.Errorf("%s: size %d, want %d", f.Key, f.Size, len(want))
		}
	}
	half := len(files[d.cutFirst]) / 2
	_, ranges, agents := d.seen()
	if len(ranges) != 1 || ranges[0] != fmt.Sprintf("%s bytes=%d-", d.cutFirst, half) {
		t.Errorf("range requests = %v, want one resuming at %d", ranges, half)
	}
	rc, err := raw.Open(ctx, byName["fre_cia_aberta_2026.zip"])
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rc)
	rc.Close()
	if !bytes.Equal(buf.Bytes(), files[d.cutFirst]) {
		t.Error("stored zip differs from the served file")
	}
	for _, ua := range agents {
		if ua == "" || strings.Contains(ua, "@") {
			t.Errorf("User-Agent %q", ua)
		}
	}
	// A second Fetch for the same reference date downloads nothing.
	if _, err := c.Fetch(ctx, ref, raw); err != nil {
		t.Fatal(err)
	}
	if gets, _, _ := d.seen(); gets[d.cutFirst] != 2 || gets["fre_cia_aberta_2025.zip"] != 1 {
		t.Errorf("GETs after refetch = %v", gets)
	}
}

// The weekly reload: unchanged validators on a later reference date mean no
// GET and no new object; a regenerated zip (new ETag) gets a new key and the
// old object stays.
func TestFetchSkipsFilesTheServerDidNotChange(t *testing.T) {
	d := newPortal(t, fixtureZips(t, nil))
	c := fastConnector(t, d.URL+"/dados")
	raw := &countingStore{Store: newRaw(t)}
	first, err := c.Fetch(ctx, fetchRef(10), raw)
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := d.seen()
	if raw.stored() != len(first) {
		t.Fatalf("stored %d, want %d", raw.stored(), len(first))
	}
	second, err := c.Fetch(ctx, fetchRef(17), raw)
	if err != nil {
		t.Fatal(err)
	}
	if after, _, _ := d.seen(); !maps.Equal(before, after) {
		t.Errorf("GETs changed on an unchanged portal: %v -> %v", before, after)
	}
	if raw.stored() != len(first) {
		t.Errorf("stored %d objects, want no new one", raw.stored()-len(first))
	}
	for i := range first {
		if first[i].Key != second[i].Key {
			t.Errorf("key %s then %s", first[i].Key, second[i].Key)
		}
	}
	d.mu.Lock()
	d.version += "v2-"
	d.mu.Unlock()
	third, err := c.Fetch(ctx, fetchRef(24), raw)
	if err != nil {
		t.Fatal(err)
	}
	if raw.stored() != 2*len(first) {
		t.Errorf("stored %d, want %d after a regeneration", raw.stored(), 2*len(first))
	}
	for i := range third {
		if third[i].Key == first[i].Key {
			t.Errorf("regenerated %s kept its key", third[i].Key)
		}
		if ok, err := raw.Exists(ctx, first[i].Key); err != nil || !ok {
			t.Errorf("old object %s gone: %v %v", first[i].Key, ok, err)
		}
	}
}

// countingStore counts the objects stored through it.
type countingStore struct {
	rawstore.Store
	mu   sync.Mutex
	puts int
}

func (c *countingStore) Put(ctx context.Context, key string, r io.Reader) (rawstore.File, error) {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return c.Store.Put(ctx, key, r)
}

func (c *countingStore) stored() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts
}

func TestFetchRefusesStoredFileWithOtherSize(t *testing.T) {
	d := newPortal(t, fixtureZips(t, nil))
	d.noValidators = true // keyed by reference date
	raw := newRaw(t)
	if _, err := raw.Put(ctx, "raw/cvm/2026-01-02/fre_cia_aberta_2025.zip", bytes.NewReader([]byte("older publication"))); err != nil {
		t.Fatal(err)
	}
	_, err := fastConnector(t, d.URL+"/dados/").Fetch(ctx, fetchRef(2), raw)
	if err == nil || !strings.Contains(err.Error(), "fre_cia_aberta_2025.zip") || !strings.Contains(err.Error(), "republished") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchSkipsUnpublishedReferenceYear(t *testing.T) {
	files := fixtureZips(t, nil)
	delete(files, "fre_cia_aberta_2026.zip")
	d := newPortal(t, files)
	got, err := fastConnector(t, d.URL+"/dados/").Fetch(ctx, fetchRef(3), newRaw(t))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0].Key, "/fre_cia_aberta_2025.zip") {
		t.Errorf("files = %+v, want only the 2025 zip", got)
	}
}

func TestFetchUnpublishedPreviousYearFails(t *testing.T) {
	files := fixtureZips(t, nil)
	delete(files, "fre_cia_aberta_2025.zip")
	d := newPortal(t, files)
	_, err := fastConnector(t, d.URL+"/dados/").Fetch(ctx, fetchRef(8), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "fre_cia_aberta_2025.zip") || !strings.Contains(err.Error(), "not published") {
		t.Errorf("err = %v", err)
	}
	if gets, _, _ := d.seen(); gets["fre_cia_aberta_2025.zip"] != 0 {
		t.Error("a 404 was retried as a download")
	}
}

func TestFetchRefusesOversizedFile(t *testing.T) {
	d := newPortal(t, fixtureZips(t, nil))
	c := fastConnector(t, d.URL+"/dados/")
	c.maxDownloadBytes = 100
	_, err := c.Fetch(ctx, fetchRef(4), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v", err)
	}
	if gets, _, _ := d.seen(); len(gets) != 0 {
		t.Errorf("GETs = %v: nothing may be downloaded over the limit", gets)
	}
}

func TestFetchRetriesServerErrorsThenFails(t *testing.T) {
	d := newPortal(t, fixtureZips(t, nil))
	d.status = http.StatusServiceUnavailable
	c := fastConnector(t, d.URL+"/dados/")
	c.attempts = 3
	_, err := c.Fetch(ctx, fetchRef(5), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v", err)
	}
	if _, _, agents := d.seen(); len(agents) != 3 {
		t.Errorf("requests = %d, want 3 attempts", len(agents))
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
	_, err := fastConnector(t, d.URL+"/dados/").Fetch(ctx, fetchRef(6), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "another host") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchThenLoad(t *testing.T) {
	e := newEnv(t)
	ref := fetchRef(7)
	d := newPortal(t, fixtureZips(t, nil))
	c := fastConnector(t, d.URL+"/dados/")
	files, err := c.Fetch(ctx, ref, e.raw)
	if err != nil {
		t.Fatal(err)
	}
	e.c = c
	col, err := collection.Begin(ctx, e.pool, sourceID, ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := c.Load(ctx, col, files, e.raw, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	if stats["rows_read"] != fixtureRows || stats["relationships_inserted"] != fixtureRels {
		t.Errorf("stats %v", stats)
	}
}

// A zip that changes between the HEAD and the GET is not stored under the
// old version's key.
func TestFetchRefusesAFileChangedDuringTheDownload(t *testing.T) {
	d := newPortal(t, fixtureZips(t, nil))
	d.getVersion = "regenerated-"
	raw := &countingStore{Store: newRaw(t)}
	_, err := fastConnector(t, d.URL+"/dados").Fetch(ctx, fetchRef(12), raw)
	if !errors.Is(err, errChangedDuringDownload) {
		t.Fatalf("err = %v, want errChangedDuringDownload", err)
	}
	if raw.stored() != 0 {
		t.Errorf("%d objects stored", raw.stored())
	}
}
