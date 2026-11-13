package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"geoint/internal/claims"
	"geoint/internal/graph"
)

// Job types of the graph queries.
const (
	TypePaths        = "paths"
	TypeControlGroup = "control_group"
)

// maxPathDepth is the deepest path search a job may ask for.
const maxPathDepth = 6

// PathsParams are the params of a "paths" job. MaxDepth 0 means the default.
type PathsParams struct {
	A        int64    `json:"a"`
	B        int64    `json:"b"`
	MaxDepth int      `json:"max_depth"`
	RelTypes []string `json:"rel_types"`
	At       string   `json:"at"` // YYYY-MM-DD; empty means no date filter
}

// ControlGroupParams are the params of a "control_group" job.
type ControlGroupParams struct {
	CompanyID int64  `json:"company_id"`
	Mode      string `json:"mode"` // documented | corporate_links
}

// The JSON keys of PathsParams and ControlGroupParams
// (TestParamsFieldListsMatchTheStructs keeps them in step).
var (
	pathsFields        = []string{"a", "b", "max_depth", "rel_types", "at"}
	controlGroupFields = []string{"company_id", "mode"}
)

var knownRelTypes = map[claims.RelType]bool{
	claims.PartnerOf: true, claims.AdministratorOf: true, claims.LegalRepOf: true, claims.DonatedTo: true,
	claims.CandidateFor: true, claims.HoldsMandate: true, claims.Controls: true, claims.ShareholderOf: true,
}

// ValidateParams checks the params of a job type the way its handler will, so
// the API can answer 400 before enqueueing. Errors wrap ErrBadParams, or
// ErrUnknownType for a type this package does not know.
func ValidateParams(typ string, raw json.RawMessage) error {
	switch typ {
	case TypePaths:
		_, _, err := parsePaths(raw)
		return err
	case TypeControlGroup:
		_, _, err := parseControlGroup(raw)
		return err
	}
	return fmt.Errorf("%w: %q", ErrUnknownType, typ)
}

// RegisterGraphTypes registers the graph job types with their validators and
// handlers that fail: for a process that only enqueues (the API), which
// validates params in Enqueue but never runs jobs.
func RegisterGraphTypes(s *Service) { registerGraph(s, nil) }

// RegisterGraphHandlers registers the paths and control_group job types,
// running them against g.
func RegisterGraphHandlers(s *Service, g *graph.Graph) { registerGraph(s, g) }

var errNoGraph = errors.New("jobs: this process does not run graph jobs")

func registerGraph(s *Service, g *graph.Graph) {
	s.RegisterValidated(TypePaths, func(ctx context.Context, raw json.RawMessage, progress func(float64)) (any, error) {
		p, o, err := parsePaths(raw)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, errNoGraph
		}
		res, err := g.Paths(ctx, p.A, p.B, o, progress)
		return res, graphError(err)
	}, func(raw json.RawMessage) error { return ValidateParams(TypePaths, raw) })
	s.RegisterValidated(TypeControlGroup, func(ctx context.Context, raw json.RawMessage, progress func(float64)) (any, error) {
		p, o, err := parseControlGroup(raw)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, errNoGraph
		}
		res, err := g.ControlGroup(ctx, p.CompanyID, o, progress)
		return res, graphError(err)
	}, func(raw json.RawMessage) error { return ValidateParams(TypeControlGroup, raw) })
}

// graphError gives a job failure a message fit for the user.
func graphError(err error) error {
	if errors.Is(err, graph.ErrNotFound) {
		return errors.New("entity not found")
	}
	return err
}

// decodeStrict decodes one JSON object into v, rejecting unknown fields,
// trailing data, duplicate keys and keys written in another case than v's
// fields (encoding/json would match those case-insensitively, and keep the
// last of two duplicates). fields are v's JSON keys.
func decodeStrict(raw json.RawMessage, v any, fields ...string) error {
	if err := checkKeys(raw, fields); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadParams, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data", ErrBadParams)
	}
	return nil
}

// checkKeys reads the keys of the top-level object of raw: a key seen twice,
// also when only the case differs, is refused, and so is a key that matches
// one of fields only case-insensitively. Other unknown keys and malformed
// JSON are left to the decoder.
func checkKeys(raw json.RawMessage, fields []string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil
		}
		folded := strings.ToLower(key)
		if seen[folded] {
			return badParam("duplicate key %q", key)
		}
		seen[folded] = true
		for _, f := range fields {
			if key != f && strings.EqualFold(key, f) {
				return badParam("key %q must be written %q", key, f)
			}
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil
		}
	}
	return nil
}

func badParam(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadParams, fmt.Sprintf(format, args...))
}

func parsePaths(raw json.RawMessage) (PathsParams, graph.PathOptions, error) {
	var p PathsParams
	var o graph.PathOptions
	if !isObject(raw) {
		return p, o, badParam("not a JSON object")
	}
	if err := decodeStrict(raw, &p, pathsFields...); err != nil {
		return p, o, err
	}
	if p.A <= 0 {
		return p, o, badParam("a must be a positive entity id")
	}
	if p.B <= 0 {
		return p, o, badParam("b must be a positive entity id")
	}
	if p.MaxDepth < 0 || p.MaxDepth > maxPathDepth {
		return p, o, badParam("max_depth must be between 0 and %d", maxPathDepth)
	}
	o.MaxDepth = p.MaxDepth
	for _, r := range p.RelTypes {
		if !knownRelTypes[claims.RelType(r)] {
			return p, o, badParam("rel_types has an unknown relationship type %q", r)
		}
		o.RelTypes = append(o.RelTypes, claims.RelType(r))
	}
	if p.At != "" {
		at, err := time.Parse(time.DateOnly, p.At)
		if err != nil {
			return p, o, badParam("at must be a date as YYYY-MM-DD")
		}
		o.At = &at
	}
	return p, o, nil
}

func parseControlGroup(raw json.RawMessage) (ControlGroupParams, graph.ControlOptions, error) {
	var p ControlGroupParams
	var o graph.ControlOptions
	if !isObject(raw) {
		return p, o, badParam("not a JSON object")
	}
	if err := decodeStrict(raw, &p, controlGroupFields...); err != nil {
		return p, o, err
	}
	if p.CompanyID <= 0 {
		return p, o, badParam("company_id must be a positive entity id")
	}
	switch m := graph.ControlMode(p.Mode); m {
	case graph.Documented, graph.CorporateLinks:
		o.Mode = m
	default:
		return p, o, badParam("mode must be %q or %q", graph.Documented, graph.CorporateLinks)
	}
	return p, o, nil
}
