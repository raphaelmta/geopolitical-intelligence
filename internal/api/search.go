package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"geoint/internal/claims"
)

// maxQueryRunes bounds the search text.
const maxQueryRunes = 200

// searchItemJSON is a search hit. match says how it was found when that is
// not the name or CNPJ: "cpf_fragment" for a person whose record's CPF
// fragment equals the 6 middle digits of the CPF in the query (a candidate,
// never a confirmation).
type searchItemJSON struct {
	entityJSON
	Match string `json:"match,omitempty"`
}

// searchJSON is the search answer. CPFFragmentSearch is set (only) when the
// query carried a CPF, so the client shows the caveat once.
type searchJSON struct {
	Items             []searchItemJSON `json:"items"`
	CPFFragmentSearch bool             `json:"cpf_fragment_search,omitempty"`
	// CPFFragmentTooCommon: the fragment is in too many records to be listed
	// alone; adding name words narrows it.
	CPFFragmentTooCommon bool `json:"cpf_fragment_too_common,omitempty"`
}

// search: GET /api/v1/search?q=&kind=&limit=. The text is never logged nor
// audited: the audit row holds only the kind, the number of results and,
// for a query carrying a CPF, the flags cpf_fragment_search and
// cpf_fragment_too_common (never the CPF or its fragment). The CPF is not echoed back either.
func (s *server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	text := q.Get("q")
	if utf8.RuneCountInString(text) > maxQueryRunes {
		fail(w, r, errBadRequest)
		return
	}
	limit := 0 // the search default (20); above 50 it clamps
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(w, r, errBadRequest)
			return
		}
		limit = n
	}
	kind := claims.EntityKind(q.Get("kind"))
	res, err := s.Search.Find(r.Context(), text, kind, limit)
	if err != nil {
		fail(w, r, err)
		return
	}
	detail := map[string]any{"kind": string(kind), "count": len(res.Items)}
	if res.CPFFragment {
		detail["cpf_fragment_search"] = true
	}
	if res.FragmentTooCommon {
		detail["cpf_fragment_too_common"] = true
	}
	if !s.audited(w, r, "search", nil, detail) {
		return
	}
	out := searchJSON{Items: make([]searchItemJSON, len(res.Items)), CPFFragmentSearch: res.CPFFragment,
		CPFFragmentTooCommon: res.FragmentTooCommon}
	for i, ref := range res.Items {
		out.Items[i] = searchItemJSON{entityJSON: toEntity(ref)}
		if i < res.FragmentHits {
			// The others (companies by the name words) were not matched by the CPF.
			out.Items[i].Match = "cpf_fragment"
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// audited writes the audit row of an action and reports whether it did; when
// it could not, the request fails with 500 and nothing else is sent.
func (s *server) audited(w http.ResponseWriter, r *http.Request, action string, entityID *int64, detail map[string]any) bool {
	if err := s.Audit(r.Context(), user(r).Sub, action, entityID, detail); err != nil {
		fail(w, r, fmt.Errorf("audit %s: %w", action, err))
		return false
	}
	return true
}

// auditWriteTimeout bounds the audit row written before a write action.
const auditWriteTimeout = 5 * time.Second

// auditAttempt writes the audit row of a write action BEFORE the action runs,
// so no action can happen unaudited. It uses its own context (not cancelled
// when the client leaves, bounded by auditWriteTimeout). When it cannot write
// the row, the request fails with 500 and the caller must not act. When the
// action then fails, the row stays as the record of the attempt.
func (s *server) auditAttempt(w http.ResponseWriter, r *http.Request, action string, entityID *int64, detail map[string]any) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), auditWriteTimeout)
	defer cancel()
	if err := s.Audit(ctx, user(r).Sub, action, entityID, detail); err != nil {
		pattern, _ := info(r.Context()).get()
		slog.ErrorContext(r.Context(), "api: audit failed, action not performed", "route", pattern, "action", action, "error", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return false
	}
	return true
}
