package tse

import (
	"context"
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
	"slices"
	"strings"
	"time"

	"geoint/internal/rawstore"
)

// userAgent is generic on purpose: no personal or project contact details.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

// headTimeout bounds the HEAD request that reads a file's size.
const headTimeout = 2 * time.Minute

// remoteFile is one zip of the CDN: its path under the base URL and its
// base name (the raw store key's last part).
type remoteFile struct {
	path, name string
	year       int
}

// remoteFiles lists the zips of the years, in year order, candidacies first.
// The names are deterministic (docs/sources/tse.md), so no listing is needed.
func remoteFiles(years []int) []remoteFile {
	out := make([]remoteFile, 0, 2*len(years))
	for _, y := range slices.Sorted(slices.Values(years)) {
		cand := fmt.Sprintf("consulta_cand_%d.zip", y)
		rec := fmt.Sprintf("prestacao_de_contas_eleitorais_candidatos_%d.zip", y)
		out = append(out,
			remoteFile{path: "consulta_cand/" + cand, name: cand, year: y},
			remoteFile{path: "prestacao_contas/" + rec, name: rec, year: y})
	}
	return out
}

// fileURL is the CDN URL of the zip name (the one Fetch downloads it from),
// "" when the base URL does not parse or name is not a TSE zip.
func (t *tse) fileURL(name string) string {
	u, err := url.Parse(t.baseURL)
	if err != nil {
		return ""
	}
	folder := ""
	switch {
	case strings.HasPrefix(name, "consulta_cand_"):
		folder = "consulta_cand/"
	case strings.HasPrefix(name, "prestacao_de_contas_eleitorais_candidatos_"):
		folder = "prestacao_contas/"
	default:
		return ""
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + folder + name
	u.RawPath = ""
	return u.String()
}

// rawPrefix is the raw store prefix of a collection whose server sent no
// validators: the request's reference date (UTC day). The TSE republishes the
// same file names, so without validators each reference date keeps its own
// copy.
func rawPrefix(ref time.Time) string {
	return "raw/" + Key + "/" + ref.UTC().Format(time.DateOnly) + "/"
}

// rawKey is the raw store key of a year's file. With validators (ETag or
// Last-Modified, the CDN sends both) the key is derived from the file's
// identity, raw/tse/<year>/<validators id>/<zip>: a year the TSE did not
// regenerate is neither downloaded nor stored again by the weekly
// collection, and a regenerated one gets a new key (the old object stays).
// Without validators: rawPrefix(ref)/<zip>.
func rawKey(ref time.Time, f remoteFile, v rawstore.Validators) string {
	if !v.Known() {
		return rawPrefix(ref) + f.name
	}
	return fmt.Sprintf("raw/%s/%d/%s/%s", Key, f.year, v.ID(), f.name)
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

// errChangedDuringDownload: the server's file changed between the HEAD that
// chose its raw key and the download.
var errChangedDuringDownload = errors.New("the file changed on the server during the download (new ETag); collect again")

// errForeignRedirect: the server redirected to another host; never followed.
var errForeignRedirect = errors.New("refusing redirect to another host")

// Fetch downloads, for every configured year, the candidacies zip and the
// campaign-finance zip into raw and returns the stored files. The key comes
// from the server's validators (rawKey: raw/tse/<year>/<validators
// id>/<zip>), read by a HEAD before any download: a file already stored
// under its key with the server's size is neither downloaded nor stored
// again (the same object then serves several collections; each records it
// in collection_file). Without validators the key is raw/tse/<ref
// date>/<zip>, and a stored file of another size there is an error (the
// TSE republished it: collect it under another reference date). Downloads
// are capped in size, resume with HTTP Range after a failure and are
// retried with backoff; a year not published (404) fails the collection.
func (t *tse) Fetch(ctx context.Context, ref time.Time, raw rawstore.Store) ([]rawstore.File, error) {
	base, err := url.Parse(t.baseURL)
	if err != nil {
		return nil, fmt.Errorf("tse: base URL: %w", err)
	}
	var out []rawstore.File
	for _, f := range remoteFiles(t.years) {
		u := *base
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + f.path
		u.RawPath = ""
		v, err := t.head(ctx, &u)
		if err != nil {
			return nil, fmt.Errorf("tse: %s: %w", f.name, err)
		}
		size, etag := v.Size, v.ETag
		if size > t.maxDownloadBytes {
			return nil, fmt.Errorf("tse: %s has %d bytes, over the %d-byte limit", f.name, size, t.maxDownloadBytes)
		}
		key := rawKey(ref, f, v)
		have, ok, err := raw.Stat(ctx, key)
		if err != nil {
			return nil, err
		}
		if ok {
			if have.Size != size {
				return nil, fmt.Errorf("tse: %s is stored with %d bytes but the server has %d (republished?)", key, have.Size, size)
			}
			slog.Info("tse: unchanged on the server, already stored", "key", key, "size", size)
			out = append(out, have)
			continue
		}
		stored, err := t.download(ctx, &u, f, size, etag, key, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, stored)
	}
	return out, nil
}

func (t *tse) newRequest(ctx context.Context, method string, u *url.URL) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// head reads the validators of a file: its size (Content-Length,
// required), ETag and Last-Modified.
func (t *tse) head(ctx context.Context, u *url.URL) (v rawstore.Validators, err error) {
	err = t.retry(ctx, "head "+path.Base(u.Path), func() error {
		hctx, cancel := context.WithTimeout(ctx, headTimeout)
		defer cancel()
		req, err := t.newRequest(hctx, http.MethodHead, u)
		if err != nil {
			return permanent(err)
		}
		resp, err := t.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if err := statusError(resp, http.StatusOK); err != nil {
			return err
		}
		if resp.ContentLength < 0 {
			return permanent(errors.New("server sent no Content-Length"))
		}
		v = rawstore.Validators{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), Size: resp.ContentLength}
		return nil
	})
	return v, err
}

// statusError classifies an unexpected status: 404 and other client errors
// are permanent, 5xx and 429 are retried.
func statusError(resp *http.Response, want ...int) error {
	for _, w := range want {
		if resp.StatusCode == w {
			return nil
		}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return permanent(errors.New("not published (404)"))
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("unexpected status %s", resp.Status)
	default:
		return permanent(fmt.Errorf("unexpected status %s", resp.Status))
	}
}

// download fetches one file into a local partial file (kept across retries
// for resuming), then stores it in raw and removes the partial file.
func (t *tse) download(ctx context.Context, u *url.URL, f remoteFile, size int64, etag, key string, raw rawstore.Store) (rawstore.File, error) {
	part := filepath.Join(t.spool(), "tse-"+path.Base(path.Dir(key))+"-"+f.name+".part")
	start := time.Now()
	err := t.retry(ctx, "download "+f.name, func() error { return t.downloadOnce(ctx, u, size, etag, part) })
	if err != nil {
		return rawstore.File{}, fmt.Errorf("tse: download %s: %w", f.name, err)
	}
	fh, err := os.Open(part)
	if err != nil {
		return rawstore.File{}, fmt.Errorf("tse: %w", err)
	}
	stored, err := raw.Put(ctx, key, fh)
	fh.Close()
	if err != nil {
		return rawstore.File{}, err
	}
	if err := os.Remove(part); err != nil {
		slog.Warn("tse: could not remove partial file", "path", part, "error", err)
	}
	slog.Info("tse: downloaded", "key", key, "size", stored.Size, "elapsed", time.Since(start).Round(time.Second).String())
	return stored, nil
}

func (t *tse) spool() string {
	if t.spoolDir != "" {
		return t.spoolDir
	}
	return os.TempDir()
}

// downloadOnce continues the partial file from its current size. Success
// means the partial file now has exactly size bytes; never more are written.
func (t *tse) downloadOnce(ctx context.Context, u *url.URL, size int64, etag, part string) error {
	fh, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return permanent(err)
	}
	defer fh.Close()
	have, err := fh.Seek(0, io.SeekEnd)
	if err != nil {
		return permanent(err)
	}
	if have == size {
		return nil
	}
	if have > size {
		if err := fh.Truncate(0); err != nil {
			return permanent(err)
		}
		have = 0
	}

	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := t.newRequest(dctx, http.MethodGet, u)
	if err != nil {
		return permanent(err)
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		if etag != "" {
			// The server sends the whole file instead if it changed.
			req.Header.Set("If-Range", etag)
		}
	}
	resp, err := t.client.Do(req)
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
		// The raw key names the version the HEAD described: a body of
		// another version must not be stored under it. The next
		// collection reads the validators again.
		if got := resp.Header.Get("ETag"); etag != "" && got != "" && got != etag {
			return permanent(errChangedDuringDownload)
		}
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
		return statusError(resp)
	}
	body := &idleReader{r: resp.Body, d: t.idleTimeout, t: time.AfterFunc(t.idleTimeout, cancel)}
	defer body.t.Stop()
	n, err := io.Copy(fh, io.LimitReader(body, size-have+1))
	if err != nil {
		return fmt.Errorf("after %d bytes: %w", have+n, err)
	}
	if got := have + n; got != size {
		if got > size {
			_ = fh.Truncate(0)
		}
		return fmt.Errorf("got %d bytes, want %d", got, size)
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

// retry runs fn up to t.attempts times with exponential backoff, stopping on
// success, on a permanent error or when ctx ends.
func (t *tse) retry(ctx context.Context, what string, fn func() error) error {
	delay := t.backoff
	var err error
	for attempt := 1; ; attempt++ {
		err = fn()
		if errors.Is(err, errForeignRedirect) {
			break
		}
		var perm permanentError
		if err == nil || errors.As(err, &perm) || ctx.Err() != nil || attempt >= t.attempts {
			break
		}
		slog.Warn("tse: retrying", "what", what, "attempt", attempt, "in", delay.String(), "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, t.maxBackoff)
	}
	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		return errors.Join(ctxErr, err)
	}
	return err
}
