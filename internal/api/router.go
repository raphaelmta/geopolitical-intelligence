package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/audit"
	"geoint/internal/auth"
	"geoint/internal/freshness"
	"geoint/internal/graph"
	"geoint/internal/identity"
	"geoint/internal/jobs"
	"geoint/internal/queue"
	"geoint/internal/search"
	"geoint/internal/sources"
)

// Roles of the realm. Matching is exact; admin tokens also carry analyst
// because the realm makes admin a composite role.
const (
	roleAnalyst = "analyst"
	roleAdmin   = "admin"
)

const (
	// DefaultTimeout bounds every request, authentication included.
	DefaultTimeout = 10 * time.Second
	// MaxBodyBytes bounds request bodies.
	MaxBodyBytes = 1 << 20
	// DefaultWebOrigin is the only origin CORS allows when none is configured.
	DefaultWebOrigin = "http://localhost:5173"
)

// AuditFunc records an audited action (see audit.Log).
type AuditFunc func(ctx context.Context, user, action string, entityID *int64, detail map[string]any) error

// Deps carries the collaborators handlers need.
type Deps struct {
	Pool     *pgxpool.Pool
	Graph    *graph.Graph
	Search   *search.Service
	Jobs     *jobs.Service
	Identity *identity.Resolver
	Fresh    *freshness.Service
	Queue    *queue.Client
	Verifier auth.Verifier

	// WebOrigin is the one origin allowed by CORS; empty means DefaultWebOrigin.
	WebOrigin string
	// AllowedHosts are host names accepted in the Host header besides
	// DefaultAllowedHosts (any port); see ParseAllowedHosts.
	AllowedHosts []string
	// Timeout per request; zero means DefaultTimeout.
	Timeout time.Duration
	// Now is the clock of staleness checks; nil means time.Now.
	Now func() time.Time
	// Audit records audited actions; nil means audit.Log on Pool.
	Audit AuditFunc
	// Source looks a source up in the catalogue; nil means sources.ByKey.
	Source func(key string) (sources.Source, bool)
}

// server holds the dependencies of the handlers.
type server struct {
	Deps
}

// route is one authenticated API route. Every route of the table is
// registered behind auth.Middleware and auth.RequireRole(role); there is no
// way to register an /api route outside it.
type route struct {
	method, pattern, role string
	handle                func(*server, http.ResponseWriter, *http.Request)
}

var routeTable = []route{
	{"GET", "/api/v1/search", roleAnalyst, (*server).search},
	{"GET", "/api/v1/entities/{id}", roleAnalyst, (*server).entity},
	{"GET", "/api/v1/entities/{id}/relationships", roleAnalyst, (*server).relationships},
	{"GET", "/api/v1/relationships/{id}/provenance", roleAnalyst, (*server).provenance},
	{"POST", "/api/v1/jobs", roleAnalyst, (*server).enqueueJob},
	{"GET", "/api/v1/jobs/{id}", roleAnalyst, (*server).getJob},
	{"GET", "/api/v1/identity/proposals", roleAnalyst, (*server).proposals},
	{"POST", "/api/v1/identity/proposals/{id}/decision", roleAnalyst, (*server).decide},
	{"POST", "/api/v1/identity/merge", roleAnalyst, (*server).merge},
	{"POST", "/api/v1/identity/split", roleAnalyst, (*server).split},
	{"GET", "/api/v1/sources", roleAnalyst, (*server).sources},
	{"POST", "/api/v1/sources/{key}/collect", roleAdmin, (*server).collect},
}

// NewRouter builds the HTTP handler of the API. Outermost first: access log
// and panic recovery, security headers (applied by the response writer of
// that same layer, so they cover the recovery's own 500), the Host check
// (421 for a host not allowed, /healthz included), CORS (preflight answered
// there, before auth), then per route: timeout, body limit, auth.Middleware,
// role check, handler. /healthz is the only route without auth.
func NewRouter(deps Deps) http.Handler {
	if deps.Verifier == nil {
		panic("api: NewRouter needs a Verifier")
	}
	s := &server{Deps: deps}
	if s.WebOrigin == "" {
		s.WebOrigin = DefaultWebOrigin
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Audit == nil {
		pool := s.Pool
		s.Audit = func(ctx context.Context, user, action string, entityID *int64, detail map[string]any) error {
			return audit.Log(ctx, pool, user, action, entityID, detail)
		}
	}

	mux := http.NewServeMux()
	authn := auth.Middleware(s.Verifier)
	methods := map[string][]string{} // path pattern → allowed methods
	for _, rt := range routeTable {
		if rt.role != roleAnalyst && rt.role != roleAdmin {
			panic("api: route " + rt.pattern + " has no valid role")
		}
		var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rt.handle(s, w, r) })
		h = auth.RequireRole(rt.role)(h)
		h = noteUser(h)
		h = authn(h)
		h = limitBody(h)
		h = withTimeout(s.Timeout, h)
		mux.Handle(rt.method+" "+rt.pattern, notePattern(rt.method+" "+rt.pattern, h))
		methods[rt.pattern] = append(methods[rt.pattern], rt.method)
	}
	mux.Handle("GET /healthz", notePattern("GET /healthz", withTimeout(s.Timeout, http.HandlerFunc(healthz))))
	methods["/healthz"] = append(methods["/healthz"], "GET")
	// The same paths without a method answer 405; anything else 404.
	for pattern, ms := range methods {
		allow := strings.Join(append(slices.Sorted(slices.Values(ms)), "OPTIONS"), ", ")
		mux.Handle(pattern, notePattern("method_not_allowed", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", allow)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		})))
	}
	mux.Handle("/", notePattern("not_found", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found")
	})))

	return outermost(hostCheck(allowedHostSet(s.AllowedHosts), cors(s.WebOrigin, mux)))
}

// user returns the authenticated user; the route table guarantees one.
func user(r *http.Request) auth.User {
	u, _ := auth.FromContext(r.Context())
	return u
}
