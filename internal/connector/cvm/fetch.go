package cvm

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
	"strings"
	"time"

	"geoint/internal/rawstore"
)

// userAgent is generic on purpose: no personal or project contact details.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

// headTimeout bounds the HEAD request that reads a file's size.
const headTimeout = 2 * time.Minute

// remoteName is the name of a year's FRE zip on the portal (deterministic,
// docs/sources/cvm.md), also the last part of its raw store key.
func remoteName(year int) string { return fmt.Sprintf("fre_cia_aberta_%d.zip", year) }

// rawPrefix is the raw store prefix of a collection whose server sent no
// validators: the request's reference date (UTC day). The CVM regenerates the
// same file names, so without validators each reference date keeps its own
// copy.
func rawPrefix(ref time.Time) string {
	return "raw/" + Key + "/" + ref.UTC().Format(time.DateOnly) + "/"
}

// rawKey is the raw store key of a year's zip. With validators (ETag or
// Last-Modified; the portal sends both) it is derived from the file's
// identity, raw/cvm/<year>/<validators id>/<zip>: a zip the CVM did not
// regenerate since the last collection is neither downloaded nor stored
// again, a regenerated one gets a new key (the old object stays). Without
// validators: rawPrefix(ref)/<zip>.
func rawKey(ref time.Time, year int, v rawstore.Validators) string {
	if !v.Known() {
		return rawPrefix(ref) + remoteName(year)
	}
	return fmt.Sprintf("raw/%s/%d/%s/%s", Key, year, v.ID(), remoteName(year))
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

// errNotPublished: the portal answered 404.
var errNotPublished = errors.New("not published (404)")

// errChangedDuringDownload: the server's file changed between the HEAD that
// chose its raw key and the download.
var errChangedDuringDownload = errors.New("the file changed on the server during the download (new ETag); collect again")

// errForeignRedirect: the server redirected to another host; never followed.
var errForeignRedirect = errors.New("refusing redirect to another host")

// Fetch downloads the FRE zip of every collected year (the reference
// date's year and DefaultYearsBack years before it) into raw and returns the
// stored files. The key comes from the server's validators, read by a HEAD
// (rawKey: raw/cvm/<year>/<validators id>/fre_cia_aberta_<year>.zip): a file
// already stored under its key with the server's size is neither downloaded
// nor stored again. Without validators the key is raw/cvm/<ref
// date>/fre_cia_aberta_<year>.zip, and a stored file of another size there is
// an error (the CVM regenerated it: collect it under another reference date). Downloads are capped in size, resume with
// HTTP Range after a failure and are retried with backoff. A 404 for the
// reference year's zip (not published yet, early January) is skipped and
// Load decides (checkYears); a 404 for an earlier year fails.
// fileURL is the URL of the CVM zip name (the one Fetch downloads it from),
// "" when the base URL does not parse.
func (t *cvm) fileURL(name string) string {
	u, err := url.Parse(t.baseURL)
	if err != nil {
		return ""
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + name
	u.RawPath = ""
	return u.String()
}

func (t *cvm) Fetch(ctx context.Context, ref time.Time, raw rawstore.Store) ([]rawstore.File, error) {
	base, err := url.Parse(t.baseURL)
	if err != nil {
		return nil, fmt.Errorf("cvm: base URL: %w", err)
	}
	var out []rawstore.File
	years := t.years(ref)
	for _, year := range years {
		name := remoteName(year)
		u := *base
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + name
		u.RawPath = ""
		v, err := t.head(ctx, &u)
		size, etag := v.Size, v.ETag
		if errors.Is(err, errNotPublished) && year == years[len(years)-1] {
			slog.Warn("cvm: the reference year is not published", "file", name)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cvm: %s: %w", name, err)
		}
		if size > t.maxDownloadBytes {
			return nil, fmt.Errorf("cvm: %s has %d bytes, over the %d-byte limit", name, size, t.maxDownloadBytes)
		}
		key := rawKey(ref, year, v)
		have, ok, err := raw.Stat(ctx, key)
		if err != nil {
			return nil, err
		}
		if ok {
			if have.Size != size {
				return nil, fmt.Errorf("cvm: %s is stored with %d bytes but the server has %d (republished?)", key, have.Size, size)
			}
			slog.Info("cvm: unchanged on the server, already stored", "key", key, "size", size)
			out = append(out, have)
			continue
		}
		stored, err := t.download(ctx, &u, name, size, etag, key, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, stored)
	}
	return out, nil
}

func (t *cvm) newRequest(ctx context.Context, method string, u *url.URL) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// head reads the validators of a file: its size (Content-Length,
// required), ETag and Last-Modified.
func (t *cvm) head(ctx context.Context, u *url.URL) (v rawstore.Validators, err error) {
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
		return permanent(errNotPublished)
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("unexpected status %s", resp.Status)
	default:
		return permanent(fmt.Errorf("unexpected status %s", resp.Status))
	}
}

// download fetches one file into a local partial file (kept across retries
// for resuming), then stores it in raw and removes the partial file.
func (t *cvm) download(ctx context.Context, u *url.URL, name string, size int64, etag, key string, raw rawstore.Store) (rawstore.File, error) {
	part := filepath.Join(t.spool(), "cvm-"+path.Base(path.Dir(key))+"-"+name+".part")
	start := time.Now()
	err := t.retry(ctx, "download "+name, func() error { return t.downloadOnce(ctx, u, size, etag, part) })
	if err != nil {
		return rawstore.File{}, fmt.Errorf("cvm: download %s: %w", name, err)
	}
	fh, err := os.Open(part)
	if err != nil {
		return rawstore.File{}, fmt.Errorf("cvm: %w", err)
	}
	stored, err := raw.Put(ctx, key, fh)
	fh.Close()
	if err != nil {
		return rawstore.File{}, err
	}
	if err := os.Remove(part); err != nil {
		slog.Warn("cvm: could not remove partial file", "path", part, "error", err)
	}
	slog.Info("cvm: downloaded", "key", key, "size", stored.Size, "elapsed", time.Since(start).Round(time.Second).String())
	return stored, nil
}

func (t *cvm) spool() string {
	if t.spoolDir != "" {
		return t.spoolDir
	}
	return os.TempDir()
}

// downloadOnce continues the partial file from its current size. Success
// means the partial file now has exactly size bytes; never more are written.
func (t *cvm) downloadOnce(ctx context.Context, u *url.URL, size int64, etag, part string) error {
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
func (t *cvm) retry(ctx context.Context, what string, fn func() error) error {
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
		slog.Warn("cvm: retrying", "what", what, "attempt", attempt, "in", delay.String(), "error", err)
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
