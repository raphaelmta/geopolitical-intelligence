package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"geoint/internal/auth"
)

// reqInfo collects, for the access log, what inner layers learn about a
// request: the route pattern and the authenticated subject.
type reqInfo struct {
	mu      sync.Mutex
	pattern string
	sub     string
	gone    bool // the client went away before an answer was written
}

type infoKey struct{}

func info(ctx context.Context) *reqInfo {
	i, _ := ctx.Value(infoKey{}).(*reqInfo)
	return i
}

func (i *reqInfo) set(pattern, sub string) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if pattern != "" {
		i.pattern = pattern
	}
	if sub != "" {
		i.sub = sub
	}
}

// clientGone records that the client left: nothing is written, and the
// access log shows 499.
func (i *reqInfo) clientGone() {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.gone = true
}

func (i *reqInfo) get() (pattern, sub string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.pattern, i.sub
}

// notePattern records the route pattern (never the raw path, which holds ids).
func notePattern(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info(r.Context()).set(pattern, "")
		next.ServeHTTP(w, r)
	})
}

// noteUser records the authenticated subject; it runs right after auth.Middleware.
func noteUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := auth.FromContext(r.Context()); ok {
			info(r.Context()).set("", u.Sub)
		}
		next.ServeHTTP(w, r)
	})
}

// respWriter is the writer of the outermost layer: it sets the security
// headers on every response, whoever writes it, and remembers the status.
type respWriter struct {
	http.ResponseWriter
	status int
}

func (w *respWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	h := w.ResponseWriter.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json; charset=utf-8")
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusClientClosed is logged when the client left before any answer
// (nginx's convention; never sent).
const statusClientClosed = 499

// handlerPanic carries a panic out of the timeout goroutine with its stack.
type handlerPanic struct {
	value any
	stack []byte
}

// outermost logs one line per request and turns a panic into a 500
// {"error":"internal"}. The log has the method, the route pattern, status,
// duration and user subject; never the raw path, the query string, the body
// or any header.
func outermost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &reqInfo{pattern: "unmatched"}
		rw := &respWriter{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), infoKey{}, ri))
		defer func() {
			p := recover()
			if hp, ok := p.(handlerPanic); ok && hp.value == http.ErrAbortHandler { //nolint:errorlint // sentinel value
				p = hp.value
			}
			if p == http.ErrAbortHandler { //nolint:errorlint // sentinel value
				panic(p)
			}
			pattern, sub := ri.get()
			status := rw.status
			ri.mu.Lock()
			if status == 0 && ri.gone {
				status = statusClientClosed
			}
			ri.mu.Unlock()
			if p != nil {
				stack := debug.Stack()
				if hp, ok := p.(handlerPanic); ok {
					p, stack = hp.value, hp.stack
				}
				slog.ErrorContext(r.Context(), "api: panic serving request", "method", r.Method, "route", pattern,
					"panic", fmt.Sprint(p), "stack", string(stack))
				if rw.status == 0 {
					writeError(rw, http.StatusInternalServerError, "internal")
				}
			}
			if p != nil {
				status = rw.status
			}
			slog.InfoContext(r.Context(), "http request", "method", r.Method, "route", pattern, "status", status,
				"duration_ms", time.Since(start).Milliseconds(), "user", sub)
		}()
		next.ServeHTTP(rw, r)
	})
}

// cors allows the single configured origin, never "*", without credentials
// (tokens travel in the Authorization header). Preflight requests from that
// origin are answered here with 204, before auth. Anything from another
// origin gets no CORS headers and goes on like any request (a preflight then
// meets 405 or 401), so the browser refuses it.
func cors(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Vary", "Origin")
		o := r.Header.Get("Origin")
		allowed := o != "" && o == origin
		if allowed {
			h.Set("Access-Control-Allow-Origin", origin)
		}
		if allowed && r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody answers 413 for a declared body over MaxBodyBytes and caps the
// body reader for the others (decodeBody turns the cap into 413 too).
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// withTimeout runs next with a deadline of d, buffering its response; when
// the deadline passes first the client gets 504 {"error":"timeout"} and
// whatever next writes afterwards is discarded. A panic in next is re-raised
// in the caller's goroutine, so the recovery layer sees it.
func withTimeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		tw := &bufferedWriter{h: make(http.Header)}
		done := make(chan struct{})
		panicked := make(chan handlerPanic, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					panicked <- handlerPanic{value: p, stack: debug.Stack()}
					return
				}
				close(done)
			}()
			next.ServeHTTP(tw, r.WithContext(ctx))
		}()
		select {
		case p := <-panicked:
			panic(p)
		case <-done:
			tw.mu.Lock()
			defer tw.mu.Unlock()
			if tw.code == 0 && tw.buf.Len() == 0 && r.Context().Err() != nil {
				info(r.Context()).clientGone() // the handler saw the client leave
				return
			}
			dst := w.Header()
			for k, v := range tw.h {
				dst[k] = v
			}
			if tw.code == 0 {
				tw.code = http.StatusOK
			}
			w.WriteHeader(tw.code)
			_, _ = w.Write(tw.buf.Bytes())
		case <-ctx.Done():
			tw.mu.Lock()
			tw.timedOut = true
			tw.mu.Unlock()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && r.Context().Err() == nil {
				writeError(w, http.StatusGatewayTimeout, "timeout")
			} else {
				info(r.Context()).clientGone() // nobody to answer
			}
			// The handler goes on until it notices; a panic it raises now
			// has no caller to reach, so it is logged here.
			pattern, _ := info(r.Context()).get()
			go func() {
				select {
				case p := <-panicked:
					slog.Error("api: panic after the response was given up", "route", pattern,
						"panic", fmt.Sprint(p.value), "stack", string(p.stack))
				case <-done:
				}
			}()
		}
	})
}

type bufferedWriter struct {
	mu       sync.Mutex
	h        http.Header
	code     int
	buf      bytes.Buffer
	timedOut bool
}

func (w *bufferedWriter) Header() http.Header { return w.h }

func (w *bufferedWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timedOut || w.code != 0 {
		return
	}
	w.code = code
}

func (w *bufferedWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timedOut {
		return 0, http.ErrHandlerTimeout
	}
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.buf.Write(b)
}
