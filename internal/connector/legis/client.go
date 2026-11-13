// Package legis holds what the Câmara and Senado connectors share: a polite
// client for their open-data REST APIs that keeps every response in the raw
// store, the party key, and the writer of holds_mandate claims.
package legis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"geoint/internal/rawstore"
)

// UserAgent is generic on purpose: no personal or project contact details.
const UserAgent = "geoint-dev"

// Errors of the client.
var (
	// ErrForeignRedirect: the server redirected to another host or scheme
	// (https to http); never followed.
	ErrForeignRedirect = errors.New("refusing redirect to another host")
	// ErrTooLarge: a response body is over the size limit.
	ErrTooLarge = errors.New("response over the size limit")
	// ErrTooManyRequests: the collection reached its request budget.
	ErrTooManyRequests = errors.New("request budget exhausted")
)

// Client calls one API. It is not safe for concurrent use: requests are
// sequential and spaced by MinInterval.
type Client struct {
	source string
	base   *url.URL
	http   *http.Client

	// Accept is sent with every request.
	Accept string
	// MinInterval is the minimum time between the start of two requests.
	MinInterval time.Duration
	// Attempts bounds the tries of one request (429, 5xx and network
	// errors are retried; other statuses are not).
	Attempts int
	// Backoff is the first retry delay, doubled up to MaxBackoff. A
	// Retry-After header on a 429 or 503 replaces it, capped at MaxBackoff.
	Backoff, MaxBackoff time.Duration
	// RequestTimeout bounds one request, body included.
	RequestTimeout time.Duration
	// MaxBody is the size limit of one response body.
	MaxBody int64
	// MaxRequests bounds the requests of one client (retries included).
	MaxRequests int

	requests int
	last     time.Time
}

// NewClient returns a client of the API at baseURL with production limits.
func NewClient(source, baseURL string) (*Client, error) {
	base, err := url.Parse(strings.TrimSuffix(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("%s: base URL: %w", source, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("%s: base URL must be an absolute http(s) URL", source)
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          2,
	}
	return &Client{
		source: source,
		base:   base,
		http: &http.Client{
			Transport:     tr,
			CheckRedirect: checkRedirect,
		},
		Accept:         "application/json",
		MinInterval:    500 * time.Millisecond, // at most 2 requests per second
		Attempts:       6,
		Backoff:        2 * time.Second,
		MaxBackoff:     2 * time.Minute,
		RequestTimeout: 2 * time.Minute,
		MaxBody:        32 << 20,
		MaxRequests:    20000,
	}, nil
}

// checkRedirect follows at most 5 redirects, only to the same host and
// scheme as the first request (never https to http).
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
		return fmt.Errorf("%w (%s://%s)", ErrForeignRedirect, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

// Requests is the number of requests sent so far.
func (c *Client) Requests() int { return c.requests }

// URL returns the absolute URL of path (relative to the base) and query.
func (c *Client) URL(path string, query url.Values) string {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(path, "/")
	u.RawPath = ""
	u.RawQuery = query.Encode()
	return u.String()
}

// StatusError is an unexpected HTTP status.
type StatusError struct {
	Code   int
	Status string
}

func (e *StatusError) Error() string { return "unexpected status " + e.Status }

// ClientError reports whether err is a permanent client error (4xx other
// than 429): the resource is not available, retrying will not help.
func ClientError(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code >= 400 && se.Code < 500 && se.Code != http.StatusTooManyRequests
}

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// retryAfterError asks for a specific delay before the next try.
type retryAfterError struct {
	error
	wait time.Duration
}

func (e retryAfterError) Unwrap() error { return e.error }

// Get returns the body of a 200 response to GET path?query, retrying 429,
// 5xx and network errors with exponential backoff (honouring Retry-After).
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	target := c.URL(path, query)
	delay := c.Backoff
	var err error
	for attempt := 1; ; attempt++ {
		var body []byte
		body, err = c.once(ctx, target)
		if err == nil {
			return body, nil
		}
		var perm permanentError
		if errors.As(err, &perm) || errors.Is(err, ErrForeignRedirect) || ctx.Err() != nil || attempt >= c.Attempts {
			break
		}
		wait := delay
		var ra retryAfterError
		if errors.As(err, &ra) && ra.wait > 0 {
			wait = ra.wait
		}
		wait = min(wait, c.MaxBackoff)
		slog.Warn(c.source+": retrying", "path", path, "attempt", attempt, "in", wait.String(), "error", err)
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), err)
		case <-time.After(wait):
		}
		delay = min(delay*2, c.MaxBackoff)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(ctxErr, err)
	}
	return nil, fmt.Errorf("%s: GET %s: %w", c.source, path, err)
}

func (c *Client) once(ctx context.Context, target string) ([]byte, error) {
	if c.requests >= c.MaxRequests {
		return nil, permanentError{fmt.Errorf("%w (%d)", ErrTooManyRequests, c.MaxRequests)}
	}
	if wait := c.MinInterval - time.Since(c.last); !c.last.IsZero() && wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	c.last = time.Now()
	c.requests++

	rctx, cancel := context.WithTimeout(ctx, c.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, permanentError{err}
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", c.Accept)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, retryAfterError{&StatusError{resp.StatusCode, resp.Status}, retryAfter(resp.Header.Get("Retry-After"))}
	default:
		return nil, permanentError{&StatusError{resp.StatusCode, resp.Status}}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.MaxBody {
		return nil, permanentError{fmt.Errorf("%w (%d bytes)", ErrTooLarge, c.MaxBody)}
	}
	return body, nil
}

// retryAfter reads a Retry-After header: seconds or an HTTP date.
func retryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if s, err := strconv.Atoi(h); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(0, time.Until(t))
	}
	return 0
}

// Fetcher keeps every response in the raw store under Prefix + name, and
// reuses a response already stored there: re-running a failed collection
// of the same reference date never asks the API again for what it has.
// A response is checked (validate) BEFORE it is stored, so a malformed 200
// never becomes the reusable copy of a reference date.
type Fetcher struct {
	Client *Client
	Raw    rawstore.Store
	Prefix string

	files []rawstore.File
	seen  map[string]bool
	urls  map[string]string // raw key -> request URL
}

// URLs maps the raw key of every stored file (markers included) to the URL
// of the request it answers.
func (f *Fetcher) URLs() map[string]string {
	out := make(map[string]string, len(f.urls))
	for k, v := range f.urls {
		out[k] = v
	}
	return out
}

func (f *Fetcher) note(key, url string) {
	if f.urls == nil {
		f.urls = map[string]string{}
	}
	f.urls[key] = url
}

// Get returns the body stored under name, fetching path?query first when
// the raw store does not have it yet. validate, when set, must accept the
// body before it is stored; a stored body it rejects is an error too.
func (f *Fetcher) Get(ctx context.Context, name, path string, query url.Values, validate func([]byte) error) ([]byte, error) {
	key := f.Prefix + name
	if f.seen[key] {
		return nil, fmt.Errorf("%s: %s requested twice", f.Client.source, name)
	}
	have, ok, err := f.Raw.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	var body []byte
	if ok {
		body, err = ReadRaw(ctx, f.Raw, have, f.Client.MaxBody)
		if err != nil {
			return nil, err
		}
		if validate != nil {
			if err := validate(body); err != nil {
				return nil, fmt.Errorf("%s: stored %s: %w", f.Client.source, key, err)
			}
		}
	} else {
		body, err = f.Client.Get(ctx, path, query)
		if err != nil {
			return nil, err
		}
		if validate != nil {
			if err := validate(body); err != nil {
				return nil, err
			}
		}
		have, err = f.Raw.Put(ctx, key, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	f.seen[key] = true
	f.files = append(f.files, have)
	f.note(have.Key, f.Client.URL(path, query))
	return body, nil
}

// UnavailableName is the marker stored instead of name when the API
// answers a permanent client error (404...) for that one item.
func UnavailableName(name string) string {
	return strings.TrimSuffix(name, ".json") + ".unavailable.json"
}

// GetItem is Get for the files of one parliamentarian: a permanent client
// error (4xx other than 429) does not fail the collection. A small marker
// ({"status": <code>}, no content of the API) is stored under
// UnavailableName(name) and returned among the files, so a load replayed
// from the raw store knows the item was unavailable and skips that
// parliamentarian; ok is false then. A marker already stored is reused.
func (f *Fetcher) GetItem(ctx context.Context, name, path string, query url.Values, validate func([]byte) error) (body []byte, ok bool, err error) {
	marker := UnavailableName(name)
	if m, found, err := f.Raw.Stat(ctx, f.Prefix+marker); err != nil {
		return nil, false, err
	} else if found {
		f.files = append(f.files, m)
		f.note(m.Key, f.Client.URL(path, query))
		return nil, false, nil
	}
	body, err = f.Get(ctx, name, path, query, validate)
	if err == nil {
		return body, true, nil
	}
	if !ClientError(err) {
		return nil, false, err
	}
	var se *StatusError
	errors.As(err, &se)
	m, perr := f.Raw.Put(ctx, f.Prefix+marker, strings.NewReader(fmt.Sprintf(`{"status": %d}`+"\n", se.Code)))
	if perr != nil {
		return nil, false, perr
	}
	slog.Warn(f.Client.source+": item unavailable", "path", path, "status", se.Code)
	f.files = append(f.files, m)
	f.note(m.Key, f.Client.URL(path, query))
	return nil, false, nil
}

// Files are the stored files, in request order.
func (f *Fetcher) Files() []rawstore.File { return f.files }

// URLBook keeps the request URLs of the files a connector fetched (by raw
// key) until its Load records them: Fetch and Load of one collection run in
// one process on one connector value. Safe for concurrent use.
type URLBook struct {
	mu   sync.Mutex
	urls map[string]string
}

// Add remembers urls (raw key -> request URL).
func (b *URLBook) Add(urls map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.urls == nil {
		b.urls = map[string]string{}
	}
	for k, v := range urls {
		b.urls[k] = v
	}
}

// Map returns a copy of what was remembered.
func (b *URLBook) Map() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]string, len(b.urls))
	for k, v := range b.urls {
		out[k] = v
	}
	return out
}
