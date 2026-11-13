package tse

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

// cdnServer serves the TSE CDN folder layout (HEAD and GET with Range).
type cdnServer struct {
	*httptest.Server
	files map[string][]byte // by path under /odsele/

	mu       sync.Mutex
	gets     map[string]int
	heads    map[string]int
	ranges   []string
	agents   []string
	cutFirst string // the first GET of this path is cut in the middle
	status   int    // when set, every request answers it
	// version is part of every ETag (a regenerated file gets a new one);
	// noValidators sends neither ETag nor Last-Modified.
	version      string
	noValidators bool
	// getVersion, when set, replaces version in the ETag of GET answers
	// only (the file changed between the HEAD and the GET).
	getVersion string
}

func newCDN(t *testing.T, files map[string][]byte) *cdnServer {
	t.Helper()
	// Each test's files get their own ETags, so tests never share raw keys.
	d := &cdnServer{files: files, gets: map[string]int{}, heads: map[string]int{}, version: t.Name() + "-"}
	d.Server = httptest.NewServer(http.HandlerFunc(d.handle))
	t.Cleanup(d.Close)
	return d
}

func (d *cdnServer) handle(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.agents = append(d.agents, r.UserAgent())
	status := d.status
	d.mu.Unlock()
	if status != 0 {
		http.Error(w, "status", status)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/odsele/")
	data, ok := d.files[p]
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.mu.Lock()
	first := false
	switch r.Method {
	case http.MethodHead:
		d.heads[p]++
	case http.MethodGet:
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

func (d *cdnServer) seen() (gets map[string]int, ranges, agents []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	gets = make(map[string]int, len(d.gets))
	for k, v := range d.gets {
		gets[k] = v
	}
	return gets, slices.Clone(d.ranges), slices.Clone(d.agents)
}

// cdnFiles lays the fixture zips out as the CDN does.
func cdnFiles(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for name, b := range fixtureZips(t, nil) {
		if strings.HasPrefix(name, "consulta_cand_") {
			out["consulta_cand/"+name] = b
		} else {
			out["prestacao_contas/"+name] = b
		}
	}
	return out
}

func fastConnector(t *testing.T, baseURL string, years []int) *tse {
	t.Helper()
	c := newTSE(baseURL, years)
	c.backoff, c.maxBackoff, c.idleTimeout = 10*time.Millisecond, 20*time.Millisecond, 5*time.Second
	c.spoolDir = t.TempDir()
	return c
}

func TestRemoteFilesAndRawPrefix(t *testing.T) {
	got := remoteFiles([]int{2024, 2022})
	var paths []string
	for _, f := range got {
		paths = append(paths, f.path)
	}
	want := "consulta_cand/consulta_cand_2022.zip,prestacao_contas/prestacao_de_contas_eleitorais_candidatos_2022.zip," +
		"consulta_cand/consulta_cand_2024.zip,prestacao_contas/prestacao_de_contas_eleitorais_candidatos_2024.zip"
	if joined(paths) != want {
		t.Errorf("paths = %v", paths)
	}
	if p := rawPrefix(time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)); p != "raw/tse/2026-11-11/" {
		t.Errorf("rawPrefix = %s", p)
	}
	if c := newTSE("", nil); c.baseURL != DefaultBaseURL || !slices.Equal(c.years, DefaultYears) {
		t.Errorf("defaults: %s %v", c.baseURL, c.years)
	}
}

func TestFetchDownloadsResumesAndStores(t *testing.T) {
	ref := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC) // own date: no other test stores these keys
	files := cdnFiles(t)
	d := newCDN(t, files)
	d.cutFirst = "prestacao_contas/prestacao_de_contas_eleitorais_candidatos_2022.zip"
	c := fastConnector(t, d.URL+"/odsele", fixtureYears)
	raw := newRaw(t)

	got, err := c.Fetch(ctx, ref, raw)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("files = %d, want 6", len(got))
	}
	byName := map[string]string{}
	for _, f := range got {
		name := path.Base(f.Key)
		byName[name] = f.Key
		// Keyed by the server's validators: raw/tse/<year>/<id>/<zip>.
		if parts := strings.Split(f.Key, "/"); len(parts) != 5 || parts[0] != "raw" || parts[1] != "tse" ||
			!strings.HasSuffix(name, "_"+parts[2]+".zip") || len(parts[3]) != 16 {
			t.Errorf("key %s is not raw/tse/<year>/<validators id>/<zip>", f.Key)
		}
		want, ok := files["consulta_cand/"+name]
		if !ok {
			want = files["prestacao_contas/"+name]
		}
		if want == nil || int64(len(want)) != f.Size {
			t.Errorf("%s: size %d, want %d", f.Key, f.Size, len(want))
		}
	}
	// The cut download was resumed where it stopped.
	half := len(files[d.cutFirst]) / 2
	_, ranges, agents := d.seen()
	if len(ranges) != 1 || ranges[0] != fmt.Sprintf("%s bytes=%d-", d.cutFirst, half) {
		t.Errorf("range requests = %v, want one resuming at %d", ranges, half)
	}
	rc, err := raw.Open(ctx, byName["prestacao_de_contas_eleitorais_candidatos_2022.zip"])
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
		if ua == "" || strings.Contains(ua, "@") || strings.Contains(strings.ToLower(ua), "geoint") {
			t.Errorf("User-Agent %q", ua)
		}
	}
	// A second Fetch for the same reference date downloads nothing.
	if _, err := c.Fetch(ctx, ref, raw); err != nil {
		t.Fatal(err)
	}
	if gets, _, _ := d.seen(); gets[d.cutFirst] != 2 || gets["consulta_cand/consulta_cand_2024.zip"] != 1 {
		t.Errorf("GETs after refetch = %v", gets)
	}
}

// The weekly reload: unchanged validators on a later reference date mean no
// GET of the body and no new object (the stored one is returned); a file the
// server regenerated (new ETag) is downloaded and stored under a new key,
// and the old object stays.
func TestFetchSkipsFilesTheServerDidNotChange(t *testing.T) {
	files := cdnFiles(t)
	d := newCDN(t, files)
	c := fastConnector(t, d.URL+"/odsele", fixtureYears)
	raw := &countingStore{Store: newRaw(t)}
	first, err := c.Fetch(ctx, time.Date(2032, 1, 1, 0, 0, 0, 0, time.UTC), raw)
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := d.seen()
	objects := raw.stored()
	if objects != len(first) {
		t.Fatalf("first fetch stored %d objects, want %d", objects, len(first))
	}

	second, err := c.Fetch(ctx, time.Date(2032, 1, 8, 0, 0, 0, 0, time.UTC), raw)
	if err != nil {
		t.Fatal(err)
	}
	after, _, _ := d.seen()
	if !maps.Equal(before, after) {
		t.Errorf("GETs changed on an unchanged server: %v -> %v", before, after)
	}
	if n := raw.stored(); n != objects {
		t.Errorf("objects stored %d -> %d, want no new object", objects, n)
	}
	for i := range first {
		if first[i].Key != second[i].Key || first[i].SHA256 != second[i].SHA256 {
			t.Errorf("file %d: %s/%s, then %s/%s", i, first[i].Key, first[i].SHA256, second[i].Key, second[i].SHA256)
		}
	}

	d.mu.Lock()
	d.version += "v2-"
	d.mu.Unlock()
	third, err := c.Fetch(ctx, time.Date(2032, 1, 15, 0, 0, 0, 0, time.UTC), raw)
	if err != nil {
		t.Fatal(err)
	}
	gets, _, _ := d.seen()
	for p, n := range gets {
		if n != before[p]+1 {
			t.Errorf("%s: %d GETs after a regeneration, want %d", p, n, before[p]+1)
		}
	}
	for i := range third {
		if third[i].Key == first[i].Key {
			t.Errorf("regenerated %s kept its key", third[i].Key)
		}
		if ok, err := raw.Exists(ctx, first[i].Key); err != nil || !ok {
			t.Errorf("old object %s gone: %v %v", first[i].Key, ok, err)
		}
	}
	if n := raw.stored(); n != 2*objects {
		t.Errorf("objects stored %d, want %d", n, 2*objects)
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
	ref := time.Date(2031, 2, 1, 0, 0, 0, 0, time.UTC)
	d := newCDN(t, cdnFiles(t))
	d.noValidators = true // keyed by reference date
	raw := newRaw(t)
	if _, err := raw.Put(ctx, "raw/tse/2031-02-01/consulta_cand_2022.zip", bytes.NewReader([]byte("older publication"))); err != nil {
		t.Fatal(err)
	}
	_, err := fastConnector(t, d.URL+"/odsele/", fixtureYears).Fetch(ctx, ref, raw)
	if err == nil || !strings.Contains(err.Error(), "consulta_cand_2022.zip") || !strings.Contains(err.Error(), "republished") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchUnpublishedYearFails(t *testing.T) {
	d := newCDN(t, cdnFiles(t))
	c := fastConnector(t, d.URL+"/odsele/", []int{2022, 2028})
	_, err := c.Fetch(ctx, time.Date(2031, 3, 1, 0, 0, 0, 0, time.UTC), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "consulta_cand_2028.zip") || !strings.Contains(err.Error(), "not published") {
		t.Errorf("err = %v", err)
	}
	if gets, _, _ := d.seen(); gets["consulta_cand/consulta_cand_2028.zip"] != 0 {
		t.Error("a 404 was retried as a download")
	}
}

func TestFetchRefusesOversizedFile(t *testing.T) {
	d := newCDN(t, cdnFiles(t))
	c := fastConnector(t, d.URL+"/odsele/", fixtureYears)
	c.maxDownloadBytes = 1000
	_, err := c.Fetch(ctx, time.Date(2031, 4, 1, 0, 0, 0, 0, time.UTC), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v", err)
	}
	if gets, _, _ := d.seen(); len(gets) != 0 {
		t.Errorf("GETs = %v: nothing may be downloaded over the limit", gets)
	}
}

func TestFetchRetriesServerErrorsThenFails(t *testing.T) {
	d := newCDN(t, cdnFiles(t))
	d.status = http.StatusServiceUnavailable
	c := fastConnector(t, d.URL+"/odsele/", []int{2022})
	c.attempts = 3
	_, err := c.Fetch(ctx, time.Date(2031, 5, 1, 0, 0, 0, 0, time.UTC), newRaw(t))
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
	_, err := fastConnector(t, d.URL+"/odsele/", []int{2022}).Fetch(ctx, time.Date(2031, 6, 1, 0, 0, 0, 0, time.UTC), newRaw(t))
	if err == nil || !strings.Contains(err.Error(), "another host") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchThenLoad(t *testing.T) {
	e := newEnv(t)
	ref := time.Date(2031, 7, 1, 0, 0, 0, 0, time.UTC)
	d := newCDN(t, cdnFiles(t))
	c := fastConnector(t, d.URL+"/odsele/", fixtureYears)
	files, err := c.Fetch(ctx, ref, e.raw)
	if err != nil {
		t.Fatal(err)
	}
	e.c = c
	col := e.begin(t, ref)
	stats, err := c.Load(ctx, col, files, e.raw, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	if stats["rows_read"] != fixtureRows || stats["relationships_inserted"] != fixtureRels {
		t.Errorf("stats %v", stats)
	}
}

// The weekly reload of unchanged files: the second collection reuses the
// stored objects (same raw keys) and still records every file it read in
// collection_file, with the same key and checksum as the first.
func TestReloadOfUnchangedFilesRecordsEveryFile(t *testing.T) {
	e := newEnv(t)
	d := newCDN(t, cdnFiles(t))
	c := fastConnector(t, d.URL+"/odsele/", fixtureYears)
	e.c = c
	var cols []int64
	for _, ref := range []time.Time{time.Date(2033, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2033, 3, 8, 0, 0, 0, 0, time.UTC)} {
		files, err := c.Fetch(ctx, ref, e.raw)
		if err != nil {
			t.Fatal(err)
		}
		col := e.begin(t, ref)
		if _, err := c.Load(ctx, col, files, e.raw, e.pool); err != nil {
			t.Fatal(err)
		}
		if err := collection.Finish(ctx, e.pool, col.ID, nil); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, col.ID)
	}
	n1 := e.count(t, `SELECT count(*) FROM collection_file WHERE collection_id = $1`, cols[0])
	n2 := e.count(t, `SELECT count(*) FROM collection_file WHERE collection_id = $1`, cols[1])
	if n1 == 0 || n1 != n2 {
		t.Fatalf("collection_file rows: %d then %d", n1, n2)
	}
	same := e.count(t, `SELECT count(*) FROM collection_file a JOIN collection_file b
		ON b.collection_id = $2 AND b.name = a.name AND b.raw_key = a.raw_key AND b.sha256 = a.sha256
		WHERE a.collection_id = $1`, cols[0], cols[1])
	if same != n1 {
		t.Errorf("%d of %d files recorded with the same raw key and checksum", same, n1)
	}
}

// A file that changes between the HEAD (which chose its key) and the GET is
// not stored under the old version's key: the collection fails.
func TestFetchRefusesAFileChangedDuringTheDownload(t *testing.T) {
	d := newCDN(t, cdnFiles(t))
	d.getVersion = "regenerated-"
	raw := &countingStore{Store: newRaw(t)}
	_, err := fastConnector(t, d.URL+"/odsele/", fixtureYears).Fetch(ctx, time.Date(2034, 1, 1, 0, 0, 0, 0, time.UTC), raw)
	if !errors.Is(err, errChangedDuringDownload) {
		t.Fatalf("err = %v, want errChangedDuringDownload", err)
	}
	if raw.stored() != 0 {
		t.Errorf("%d objects stored", raw.stored())
	}
}
