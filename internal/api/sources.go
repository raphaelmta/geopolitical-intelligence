package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
	"unicode/utf8"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/identity"
	"geoint/internal/search"
	"geoint/internal/sources"
)

// Limits of a collection slice requested through the API.
const (
	maxCollectCNPJs = 1000
	maxCollectDepth = 3
)

type sourceStatusJSON struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// Collectable: a collection of this source can be requested (it has a connector).
	Collectable bool `json:"collectable"`
	// Editions: the source is published as dated editions (sources.Source.Editions,
	// today only the Receita): the reference date names the edition and
	// staleness counts from last_observed_date.
	Editions          bool       `json:"editions"`
	Status            string     `json:"status"`
	LastCollectedAt   *time.Time `json:"last_collected_at"`
	NextExpectedAt    *time.Time `json:"next_expected_at"`
	LastReferenceDate *date      `json:"last_reference_date"`
	// LastObservedDate: the date by which the data of the last done
	// full-scope collection was observed (for an edition source such as the
	// Receita, its publication date; otherwise the day the collection
	// started). Staleness of an edition source counts from it.
	LastObservedDate *date `json:"last_observed_date"`
	// LastCheckedAt: the last time the source was confirmed up to date (a
	// collection finished, or a check found nothing new to collect).
	LastCheckedAt *time.Time `json:"last_checked_at"`
	// LastError is shown to admins only, for every source ("" when the
	// source is not failed): it is raw connector text.
	LastError *string          `json:"last_error,omitempty"`
	LastStats map[string]int64 `json:"last_stats"`
	// PendingRequestAt: when the full collection still waiting to start was
	// requested (freshness.SourceStatus.PendingRequestAt), else null.
	PendingRequestAt *time.Time `json:"pending_request_at"`
}

// maxErrorRunes bounds last_error.
const maxErrorRunes = 300

// sources: GET /api/v1/sources → [SourceStatus].
func (s *server) sources(w http.ResponseWriter, r *http.Request) {
	list, err := s.Fresh.List(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	admin := user(r).Has(roleAdmin)
	out := make([]sourceStatusJSON, len(list))
	for i, st := range list {
		stats := st.LastStats
		if stats == nil {
			stats = map[string]int64{}
		}
		src, _ := s.source(st.Key)
		out[i] = sourceStatusJSON{Key: st.Key, Name: st.Name, Collectable: src.HasConnector, Editions: src.Editions, Status: string(st.Status),
			LastCollectedAt: utc(st.LastCollectedAt), NextExpectedAt: utc(st.NextExpectedAt),
			LastReferenceDate: toDate(st.LastReferenceDate), LastObservedDate: toDate(st.LastObservedDate), LastCheckedAt: utc(st.LastCheckedAt), LastStats: stats,
			PendingRequestAt: utc(st.PendingRequestAt)}
		if admin {
			e := truncateRunes(search.MaskCPF(st.LastError), maxErrorRunes)
			out[i].LastError = &e
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// source looks key up in the catalogue (Deps.Source, default sources.ByKey).
func (s *server) source(key string) (sources.Source, bool) {
	if s.Source != nil {
		return s.Source(key)
	}
	return sources.ByKey(key)
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// collect: POST /api/v1/sources/{key}/collect (admin). Only an absent body
// or {} asks for a full collection; {"cnpjs":[...],"depth":n} asks for a
// slice (null values and an empty list are refused, so a mistake never turns
// into a national collection). A source without a connector is 409. The
// request is audited, then recorded and queued for the worker through
// freshness.Service.Request, the scheduler's own path → 202 {}. A full
// request while a collection of the source runs or another full request
// waits to start is 409 (audited as an attempt, nothing queued).
func (s *server) collect(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	src, ok := s.source(key)
	if !ok {
		fail(w, r, errNotFound)
		return
	}
	var body struct {
		CNPJs json.RawMessage `json:"cnpjs"`
		Depth json.RawMessage `json:"depth"`
	}
	if err := decodeBody(r, &body, true); err != nil {
		fail(w, r, err)
		return
	}
	scope, err := collectScope(body.CNPJs, body.Depth)
	if err != nil {
		fail(w, r, err)
		return
	}
	if !src.HasConnector {
		writeError(w, http.StatusConflict, "conflict")
		return
	}
	if !scope.Full && !src.SupportsSlice {
		fail(w, r, errBadRequest) // this source only collects in full
		return
	}
	req := connector.Request{Source: key, ReferenceDate: s.Now().UTC().Truncate(24 * time.Hour), Scope: scope}
	if !s.auditAttempt(w, r, "source.collect", nil, map[string]any{"source": key, "full": scope.Full,
		"cnpjs": len(scope.CNPJs), "depth": scope.Depth}) {
		return
	}
	if err := s.Fresh.Request(r.Context(), s.Queue, src, req, user(r).Sub); err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{})
}

// collectScope turns the collect body fields (absent: nil) into a scope.
func collectScope(rawCNPJs, rawDepth json.RawMessage) (collection.Scope, error) {
	isNull := func(b json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(b), []byte("null")) }
	if isNull(rawCNPJs) || isNull(rawDepth) {
		return collection.Scope{}, errBadRequest
	}
	if rawCNPJs == nil {
		if rawDepth != nil {
			return collection.Scope{}, errBadRequest // depth needs cnpjs
		}
		return collection.Scope{Full: true}, nil
	}
	var cnpjs []string
	if err := json.Unmarshal(rawCNPJs, &cnpjs); err != nil || len(cnpjs) == 0 || len(cnpjs) > maxCollectCNPJs {
		return collection.Scope{}, errBadRequest
	}
	var scope collection.Scope
	if rawDepth != nil {
		if err := json.Unmarshal(rawDepth, &scope.Depth); err != nil || scope.Depth < 0 || scope.Depth > maxCollectDepth {
			return collection.Scope{}, errBadRequest
		}
	}
	seen := map[string]bool{}
	for _, c := range cnpjs {
		base, ok := identity.CNPJBase(c)
		if !ok {
			return collection.Scope{}, errBadRequest
		}
		if !seen[base] {
			seen[base] = true
			scope.CNPJs = append(scope.CNPJs, base)
		}
	}
	return scope, nil
}
