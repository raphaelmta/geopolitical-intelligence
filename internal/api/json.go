package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"geoint/internal/freshness"
	"geoint/internal/graph"
	"geoint/internal/identity"
	"geoint/internal/jobs"
	"geoint/internal/search"
)

// writeJSON encodes v first and only then writes status and body, so an
// encoding failure is still a clean 500.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		slog.Error("api: encode response", "error", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// writeError writes {"error": code}.
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + code + `"}` + "\n"))
}

// errBadRequest is a malformed request: ids, parameters or body.
var errBadRequest = errors.New("bad request")

// errBodyTooLarge is a body over MaxBodyBytes.
var errBodyTooLarge = errors.New("body too large")

// decodeBody decodes exactly one JSON object into v: unknown fields, trailing
// data and syntax errors are errBadRequest, an oversized body errBodyTooLarge.
// With allowEmpty, an empty body leaves v untouched and returns nil.
func decodeBody(r *http.Request, v any, allowEmpty bool) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
		return errBadRequest
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		if allowEmpty {
			return nil
		}
		return errBadRequest
	}
	if trimmed[0] != '{' { // every body is an object; null would decode as {}
		return errBadRequest
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errBadRequest
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errBadRequest
	}
	return nil
}

// fail maps err to a status and a fixed code. Unexpected errors are logged
// (server side only) and answered 500 {"error":"internal"}; no error text
// ever reaches the client.
//
// A client that went away (its request context cancelled) is not an error:
// nothing is written, a debug line is logged and the access log shows 499.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	pattern, _ := info(r.Context()).get()
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		slog.DebugContext(r.Context(), "api: client went away", "route", pattern)
		info(r.Context()).clientGone()
		return
	}
	status, code := classify(err)
	if status == http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "api: request failed", "route", pattern, "error", err)
	}
	writeError(w, status, code)
}

func classify(err error) (int, string) {
	switch {
	case errors.Is(err, errBodyTooLarge):
		return http.StatusRequestEntityTooLarge, "body_too_large"
	case errors.Is(err, search.ErrQueryTooShort):
		return http.StatusBadRequest, "query_too_short"
	case errors.Is(err, search.ErrInvalidCPF):
		return http.StatusBadRequest, "invalid_cpf"
	case errors.Is(err, search.ErrMultipleCPFs):
		return http.StatusBadRequest, "multiple_cpfs"
	case errors.Is(err, errBadRequest), errors.Is(err, graph.ErrBadCursor), errors.Is(err, graph.ErrBadOptions),
		errors.Is(err, search.ErrBadKind), errors.Is(err, jobs.ErrUnknownType), errors.Is(err, jobs.ErrBadParams),
		errors.Is(err, identity.ErrTooFewRecords):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, errNotFound), errors.Is(err, graph.ErrNotFound), errors.Is(err, jobs.ErrNotFound),
		errors.Is(err, identity.ErrProposalNotFound), errors.Is(err, identity.ErrRecordNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, identity.ErrProposalStale), errors.Is(err, identity.ErrProposalNotPending),
		errors.Is(err, identity.ErrExpectationFailed), errors.Is(err, freshness.ErrRequestBlocked):
		return http.StatusConflict, "conflict"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout"
	}
	return http.StatusInternalServerError, "internal"
}

// errNotFound is a missing entity, relationship or source.
var errNotFound = errors.New("not found")

// date is a date-only value, encoded as "YYYY-MM-DD".
type date time.Time

func (d date) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(d).Format(time.DateOnly) + `"`), nil
}

func toDate(t *time.Time) *date {
	if t == nil {
		return nil
	}
	d := date(*t)
	return &d
}

// utc returns t in UTC (RFC 3339 with Z when encoded); nil stays nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// entityJSON is an entity as the API returns it: its name passes through
// search.MaskCPF, like every person or company name leaving the API.
type entityJSON struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	NaturalKey string `json:"natural_key"`
}

func toEntity(e graph.EntityRef) entityJSON {
	return entityJSON{ID: e.ID, Kind: string(e.Kind), Name: search.MaskCPF(e.Name), NaturalKey: e.NaturalKey}
}

func toEntities(refs []graph.EntityRef) []entityJSON {
	out := make([]entityJSON, len(refs))
	for i, r := range refs {
		out[i] = toEntity(r)
	}
	return out
}

// edgeJSON is a relationship claim. Fact versus inference is always
// explicit: kind, and confidence (null for a fact).
type edgeJSON struct {
	RelationshipID     int64          `json:"relationship_id"`
	Type               string         `json:"type"`
	Src                entityJSON     `json:"src"`
	Dst                entityJSON     `json:"dst"`
	Attrs              map[string]any `json:"attrs"`
	Kind               string         `json:"kind"`
	Confidence         *float64       `json:"confidence"`
	IdentityConfidence *float64       `json:"identity_confidence,omitempty"`
	ValidFrom          *date          `json:"valid_from"`
	ValidToAfter       *date          `json:"valid_to_after"`
	ValidToBefore      *date          `json:"valid_to_before"`
	SourceKey          string         `json:"source_key"`
	CollectionID       int64          `json:"collection_id"`
	Stale              bool           `json:"stale"`
	// Closed: the claim is closed (closed_collection_id set); clients need
	// not infer it from the valid_to dates.
	Closed bool `json:"closed"`
	// IdentityBasis: what the identity of the claim's person ends rests on,
	// "none" | "auto" | "analyst" (graph.Edge.IdentityBasis); "unknown" only
	// for a job result stored before the field existed whose edge cites a
	// person record.
	IdentityBasis string `json:"identity_basis"`
}

// identityUnknown is the identity basis of an edge of a job result stored
// before identity_basis existed that cites a person record.
const identityUnknown = "unknown"

// attrKind says how an edge attribute is checked and shown.
type attrKind int

const (
	// attrText: free text that could embed a CPF; passed through MaskCPF.
	attrText attrKind = iota + 1
	// attrID: a code or identifier (receipt ids reach 20 digits), never
	// masked: masking an 11-digit id would corrupt it.
	attrID
	// attrRole: the natural-key role, never masked. Wider than attrID:
	// mandate roles hold the party sigla, which may have spaces and accents
	// ("57:UNIÃO:SP:exercicio").
	attrRole
	// attrDate: YYYY-MM-DD.
	attrDate
	// attrDecimal: an exact decimal the connectors store as a string; stays a string.
	attrDecimal
	// attrNumber: an integer.
	attrNumber
	// attrEnum: a short enumerated value (status).
	attrEnum
	// attrUF: a two-letter state code.
	attrUF
)

// edgeAttrs is the one table of the attribute keys an edge may show, with
// their kind: exactly the keys the connectors write
// (TestEdgeAttrsCoverConnectors keeps it in step with internal/connector).
// Any other key is dropped, so a source can never leak a name or document
// through attrs by adding one. Identifiers and codes are never masked; free
// text goes through MaskCPF.
//
//	receita  partner_of, administrator_of, legal_rep_of: role
//	tse      candidate_for: year, office, office_code, uf, city, city_name, result, ballot_name, role
//	tse      donated_to: year, amount, date, origin, origin_code, receipt_id, items, role
//	camara, senado  holds_mandate: legislature, uf, party, status, role
//	cvm      shareholder_of, controls: role, reference_date, form_version,
//	         percent, percent_on, percent_pn, described_by
var edgeAttrs = map[string]attrKind{
	"role": attrRole,
	// tse
	"year": attrNumber, "office": attrText, "office_code": attrID, "uf": attrUF, "city": attrID,
	"city_name": attrText, "result": attrText, "ballot_name": attrText, "amount": attrDecimal,
	"date": attrDate, "origin": attrText, "origin_code": attrID, "receipt_id": attrID, "items": attrNumber,
	// camara, senado
	"legislature": attrNumber, "party": attrText, "status": attrEnum,
	// cvm
	"reference_date": attrDate, "form_version": attrNumber, "percent": attrDecimal, "percent_on": attrDecimal,
	"percent_pn": attrDecimal, "described_by": attrID,
}

// deniedAttrs are keys a connector writes that are deliberately not shown.
// None today; a key belongs either here or in edgeAttrs.
var deniedAttrs = map[string]bool{}

var (
	idValue      = regexp.MustCompile(`^[A-Za-z0-9:._#/-]{1,64}$`)
	roleValue    = regexp.MustCompile(`^[\p{L}\p{N} :._#/-]{1,96}$`)
	dateValue    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	decimalValue = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
	integerValue = regexp.MustCompile(`^-?[0-9]{1,20}$`)
	enumValue    = regexp.MustCompile(`^[A-Za-z_]{1,32}$`)
	ufValue      = regexp.MustCompile(`^[A-Z]{2}$`)
)

// attrValue returns the value an attribute of kind k is shown with, and
// false when it is dropped: null (absent and null both mean "not informed"),
// objects, arrays, and any value whose shape does not fit its kind.
// Integers are kept as written (json.Number; a float64 from the database
// driver is written back in its shortest exact form).
func attrValue(k attrKind, v any) (any, bool) {
	if k == attrNumber {
		var n string
		switch x := v.(type) {
		case json.Number:
			n = x.String()
		case float64:
			if x != math.Trunc(x) || math.IsInf(x, 0) {
				return nil, false
			}
			n = strconv.FormatFloat(x, 'f', -1, 64)
		case string:
			n = x
		default:
			return nil, false
		}
		if !integerValue.MatchString(n) {
			return nil, false
		}
		return json.Number(canonicalInteger(n)), true
	}
	str, ok := v.(string)
	if !ok {
		return nil, false
	}
	switch k {
	case attrText:
		return search.MaskCPF(str), true
	case attrID:
		ok = idValue.MatchString(str)
	case attrRole:
		ok = utf8.ValidString(str) && roleValue.MatchString(str)
	case attrDate:
		_, err := time.Parse(time.DateOnly, str)
		ok = dateValue.MatchString(str) && err == nil
	case attrDecimal:
		ok = decimalValue.MatchString(str)
	case attrEnum:
		ok = enumValue.MatchString(str)
	case attrUF:
		ok = ufValue.MatchString(str)
	default:
		ok = false
	}
	if !ok {
		return nil, false
	}
	return str, true
}

// canonicalInteger writes an integer that matched integerValue the way JSON
// requires: no leading zeros ("007" → 7) and no negative zero ("-0" → 0).
func canonicalInteger(n string) string {
	neg := strings.HasPrefix(n, "-")
	digits := strings.TrimLeft(strings.TrimPrefix(n, "-"), "0")
	switch {
	case digits == "":
		return "0"
	case neg:
		return "-" + digits
	}
	return digits
}

func toEdge(e graph.Edge) edgeJSON {
	attrs := map[string]any{}
	for k, v := range e.Attrs {
		kind, known := edgeAttrs[k]
		if !known {
			continue
		}
		if out, ok := attrValue(kind, v); ok {
			attrs[k] = out
		}
	}
	out := edgeJSON{
		RelationshipID: e.RelationshipID, Type: string(e.Type), Src: toEntity(e.Src), Dst: toEntity(e.Dst),
		Attrs: attrs, Kind: string(e.Kind), Confidence: e.Confidence, IdentityConfidence: e.IdentityConfidence,
		ValidFrom: toDate(e.ValidFrom), ValidToAfter: toDate(e.ValidToAfter), ValidToBefore: toDate(e.ValidToBefore),
		SourceKey: e.SourceKey, CollectionID: e.CollectionID, Stale: e.Stale,
		Closed: e.Closed, IdentityBasis: e.IdentityBasis,
	}
	if out.IdentityBasis == "" {
		// A job result stored before these fields existed: the identity
		// basis is known only when no person record is cited, and closed is
		// what the dates say.
		out.IdentityBasis = identityUnknown
		if e.IdentityConfidence == nil {
			out.IdentityBasis = graph.IdentityNone
		}
		out.Closed = e.ValidToAfter != nil || e.ValidToBefore != nil
	}
	return out
}

func toEdges(edges []graph.Edge) []edgeJSON {
	out := make([]edgeJSON, len(edges))
	for i, e := range edges {
		out[i] = toEdge(e)
	}
	return out
}
