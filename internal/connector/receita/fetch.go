package receita

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"geoint/internal/rawstore"
)

// userAgent is generic on purpose: no personal or project contact details.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

// listTimeout bounds the directory listing request.
const listTimeout = 2 * time.Minute

// dataFile matches the files the loader reads, e.g. Socios0.zip.
var dataFile = regexp.MustCompile(`^(Empresas|Estabelecimentos|Socios)(\d+)\.zip$`)

// domainTables give meaning to the codes the loader stores (qualification,
// legal nature, city, CNAE...). They are kept in the raw store; Load ignores them.
var domainTables = []string{"Cnaes.zip", "Motivos.zip", "Municipios.zip", "Naturezas.zip", "Paises.zip", "Qualificacoes.zip"}

// editionMonth maps a reference date to the folder of the monthly edition it
// names: any day of month M selects folder "YYYY-MM" of M (UTC). There is no
// fallback to an earlier month: a month not published yet is an error.
func editionMonth(ref time.Time) string { return ref.UTC().Format("2006-01") }

// rawPrefix is the raw store prefix of an edition. The date is the first day
// of the edition's month, so every reference date of one month shares the files.
func rawPrefix(ref time.Time) string {
	t := ref.UTC()
	return "raw/" + Key + "/" + time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02") + "/"
}

func newHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          4,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("%w (%s)", errForeignRedirect, req.URL.Host)
			}
			return nil
		},
	}
}

// errForeignRedirect: the server redirected to another host; never followed.
var errForeignRedirect = errors.New("refusing redirect to another host")

// remoteFile is one file of the edition folder.
type remoteFile struct {
	Name         string
	Size         int64
	ETag         string
	LastModified string // getlastmodified as listed (RFC 1123, GMT)
}

// Fetch downloads the edition named by ref into raw (keys
// raw/receita/<YYYY-MM-01>/<file>) and returns the stored files: the
// Empresas, Estabelecimentos and Socios parts and the domain tables. A file
// already stored with the server's size is not downloaded again. Downloads
// resume with HTTP Range after a failure and are retried with backoff.
// When ResolveReference judged this month complete, the data parts listed now
// must be exactly the ones it judged (names, sizes, ETags, dates); otherwise
// Fetch fails before downloading anything.
func (r *receita) Fetch(ctx context.Context, ref time.Time, raw rawstore.Store) ([]rawstore.File, error) {
	month := editionMonth(ref)
	base, user, err := r.folderURL(month)
	if err != nil {
		return nil, err
	}
	listing, err := r.list(ctx, base, user)
	if err != nil {
		return nil, fmt.Errorf("receita: list edition %s: %w", month, err)
	}
	want, err := selectFiles(listing)
	if err != nil {
		return nil, fmt.Errorf("receita: edition %s: %w", month, err)
	}
	// Download exactly the file set ResolveReference judged complete.
	if err := r.sameAsJudged(month, want); err != nil {
		return nil, err
	}
	prefix := rawPrefix(ref)
	out := make([]rawstore.File, 0, len(want))
	for _, f := range want {
		key := prefix + f.Name
		have, ok, err := raw.Stat(ctx, key)
		if err != nil {
			return nil, err
		}
		if ok {
			if have.Size != f.Size {
				return nil, fmt.Errorf("receita: %s is stored with %d bytes but the server has %d (edition republished?)", key, have.Size, f.Size)
			}
			slog.Info("receita: already stored", "key", key, "size", f.Size)
			out = append(out, have)
			continue
		}
		stored, err := r.download(ctx, base, user, month, f, key, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, stored)
	}
	return out, nil
}

// folderURL returns the URL of the base folder (sub "") or of a subfolder,
// without user info, and the share token taken from the base URL's user part.
func (r *receita) folderURL(sub string) (*url.URL, *url.Userinfo, error) {
	u, err := url.Parse(r.baseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("receita: base URL: %w", err)
	}
	user := u.User
	u.User = nil
	u.Path = strings.TrimSuffix(u.Path, "/") + "/"
	if sub != "" {
		u.Path += sub + "/"
	}
	u.RawPath = ""
	return u, user, nil
}

// fileURL is the WebDAV URL of a file of the month folder, without user
// information (the public share token travels as Basic auth, never in a
// stored URL); "" when the base URL does not parse.
func (r *receita) fileURL(month, name string) string {
	u, _, err := r.folderURL(month)
	if err != nil {
		return ""
	}
	u.Path += name
	return u.String()
}

func (r *receita) newRequest(ctx context.Context, method string, u *url.URL, user *url.Userinfo, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if user != nil {
		pw, _ := user.Password()
		req.SetBasicAuth(user.Username(), pw)
	}
	return req, nil
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:getcontentlength/><d:getetag/><d:getlastmodified/><d:resourcetype/></d:prop></d:propfind>`

type multistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				Length       string `xml:"getcontentlength"`
				ETag         string `xml:"getetag"`
				LastModified string `xml:"getlastmodified"`
				ResourceType struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// davEntry is one entry of a WebDAV folder listing.
type davEntry struct {
	Name         string
	Dir          bool
	Size         int64 // files only
	ETag         string
	LastModified string
}

// list asks the WebDAV folder for its files (PROPFIND, Depth 1).
func (r *receita) list(ctx context.Context, base *url.URL, user *url.Userinfo) ([]remoteFile, error) {
	entries, err := r.propfind(ctx, base, user)
	if err != nil {
		return nil, err
	}
	var files []remoteFile
	for _, e := range entries {
		if !e.Dir {
			files = append(files, remoteFile{Name: e.Name, Size: e.Size, ETag: e.ETag, LastModified: e.LastModified})
		}
	}
	return files, nil
}

// propfind lists a WebDAV folder (PROPFIND, Depth 1): its files with size and
// ETag, and its subfolders. The folder itself is listed too (as a folder).
// The response is capped at 16 MiB; failures are retried with backoff.
func (r *receita) propfind(ctx context.Context, base *url.URL, user *url.Userinfo) ([]davEntry, error) {
	var entries []davEntry
	err := r.retry(ctx, "list "+base.Path, func() error {
		lctx, cancel := context.WithTimeout(ctx, listTimeout)
		defer cancel()
		req, err := r.newRequest(lctx, "PROPFIND", base, user, strings.NewReader(propfindBody))
		if err != nil {
			return permanent(err)
		}
		req.Header.Set("Depth", "1")
		req.Header.Set("Content-Type", "application/xml")
		resp, err := r.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return permanent(errors.New("not published (404)"))
		case resp.StatusCode != http.StatusMultiStatus:
			err := fmt.Errorf("unexpected status %s", resp.Status)
			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
				return err
			}
			return permanent(err)
		}
		var ms multistatus
		if err := xml.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&ms); err != nil {
			return fmt.Errorf("decode listing: %w", err)
		}
		entries = entries[:0]
		for _, resp := range ms.Responses {
			href, err := url.PathUnescape(resp.Href)
			if err != nil {
				continue
			}
			name := path.Base(strings.TrimSuffix(href, "/"))
			for _, ps := range resp.Propstat {
				if !strings.Contains(ps.Status, " 200 ") {
					continue
				}
				if ps.Prop.ResourceType.Collection != nil {
					entries = append(entries, davEntry{Name: name, Dir: true})
					continue
				}
				n, err := strconv.ParseInt(strings.TrimSpace(ps.Prop.Length), 10, 64)
				if err != nil {
					continue
				}
				entries = append(entries, davEntry{Name: name, Size: n, ETag: ps.Prop.ETag,
					LastModified: strings.TrimSpace(ps.Prop.LastModified)})
			}
		}
		return nil
	})
	return entries, err
}

// selectFiles keeps the data parts and domain tables. Each data kind must be
// present with parts numbered 0..n-1 without gaps: loading a partial edition
// would make a snapshot source look like companies disappeared.
func selectFiles(listing []remoteFile) ([]remoteFile, error) {
	parts := map[string][]int{}
	var out []remoteFile
	domain := map[string]bool{}
	for _, d := range domainTables {
		domain[d] = true
	}
	for _, f := range listing {
		if m := dataFile.FindStringSubmatch(f.Name); m != nil {
			n, _ := strconv.Atoi(m[2])
			parts[m[1]] = append(parts[m[1]], n)
			out = append(out, f)
		} else if domain[f.Name] {
			out = append(out, f)
		}
	}
	for _, kind := range []string{"Empresas", "Estabelecimentos", "Socios"} {
		ns := parts[kind]
		if len(ns) == 0 {
			return nil, fmt.Errorf("no %s files in the listing", kind)
		}
		sort.Ints(ns)
		for i, n := range ns {
			if n != i {
				return nil, fmt.Errorf("%s parts are not numbered 0..%d without gaps: %v", kind, len(ns)-1, ns)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// download fetches one file into a local partial file (kept across retries
// for resuming), then stores it in raw and removes the partial file.
func (r *receita) download(ctx context.Context, base *url.URL, user *url.Userinfo, month string, f remoteFile, key string, raw rawstore.Store) (rawstore.File, error) {
	part := filepath.Join(r.spool(), "receita-"+month+"-"+f.Name+".part")
	u := *base
	u.Path += f.Name
	start := time.Now()
	err := r.retry(ctx, "download "+f.Name, func() error { return r.downloadOnce(ctx, &u, user, f, part) })
	if err != nil {
		return rawstore.File{}, fmt.Errorf("receita: download %s: %w", f.Name, err)
	}
	fh, err := os.Open(part)
	if err != nil {
		return rawstore.File{}, fmt.Errorf("receita: %w", err)
	}
	stored, err := raw.Put(ctx, key, fh)
	fh.Close()
	if err != nil {
		return rawstore.File{}, err
	}
	if err := os.Remove(part); err != nil {
		slog.Warn("receita: could not remove partial file", "path", part, "error", err)
	}
	slog.Info("receita: downloaded", "key", key, "size", stored.Size, "elapsed", time.Since(start).Round(time.Second).String())
	return stored, nil
}

func (r *receita) spool() string {
	if r.spoolDir != "" {
		return r.spoolDir
	}
	return os.TempDir()
}

// downloadOnce continues the partial file from its current size. Success
// means the partial file now has exactly f.Size bytes.
func (r *receita) downloadOnce(ctx context.Context, u *url.URL, user *url.Userinfo, f remoteFile, part string) error {
	fh, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return permanent(err)
	}
	defer fh.Close()
	have, err := fh.Seek(0, io.SeekEnd)
	if err != nil {
		return permanent(err)
	}
	if have == f.Size {
		return nil
	}
	if have > f.Size {
		if err := fh.Truncate(0); err != nil {
			return permanent(err)
		}
		have = 0
	}

	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := r.newRequest(dctx, http.MethodGet, u, user, nil)
	if err != nil {
		return permanent(err)
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		if f.ETag != "" {
			// The server sends the whole file instead if it changed.
			req.Header.Set("If-Range", f.ETag)
		}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", have)) {
			return fmt.Errorf("unexpected Content-Range %q", resp.Header.Get("Content-Range"))
		}
	case http.StatusOK:
		if err := fh.Truncate(0); err != nil {
			return permanent(err)
		}
		if _, err := fh.Seek(0, io.SeekStart); err != nil {
			return permanent(err)
		}
		have = 0
	case http.StatusRequestedRangeNotSatisfiable:
		// The partial file is not a prefix of the current file: start over.
		if err := fh.Truncate(0); err != nil {
			return permanent(err)
		}
		return errors.New("range not satisfiable, restarting")
	default:
		err := fmt.Errorf("unexpected status %s", resp.Status)
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return err
		}
		return permanent(err)
	}
	body := &idleReader{r: resp.Body, d: r.idleTimeout, t: time.AfterFunc(r.idleTimeout, cancel)}
	defer body.t.Stop()
	n, err := io.Copy(fh, io.LimitReader(body, f.Size-have+1))
	if err != nil {
		return fmt.Errorf("after %d bytes: %w", have+n, err)
	}
	if got := have + n; got != f.Size {
		if got > f.Size {
			_ = fh.Truncate(0)
		}
		return fmt.Errorf("got %d bytes, want %d", got, f.Size)
	}
	return nil
}

// idleReader cancels the request (through its timer) when no byte arrives
// for d; every successful read pushes the deadline forward.
type idleReader struct {
	r io.Reader
	d time.Duration
	t *time.Timer
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.t.Reset(ir.d)
	}
	return n, err
}

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// permanent marks an error that retrying cannot fix.
func permanent(err error) error { return permanentError{err} }

// retry runs fn up to r.attempts times with exponential backoff, stopping on
// success, on a permanent error or when ctx ends.
func (r *receita) retry(ctx context.Context, what string, fn func() error) error {
	delay := r.backoff
	var err error
	for attempt := 1; ; attempt++ {
		err = fn()
		if errors.Is(err, errForeignRedirect) {
			break
		}
		var perm permanentError
		if err == nil || errors.As(err, &perm) || ctx.Err() != nil || attempt >= r.attempts {
			break
		}
		slog.Warn("receita: retrying", "what", what, "attempt", attempt, "in", delay.String(), "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, r.maxBackoff)
	}
	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		return errors.Join(ctxErr, err)
	}
	return err
}
