// Package senado is the connector of the Senado Federal open-data API
// (Dados Abertos Legislativos): who held a senator's mandate, in which
// legislatures, for which party and state, and when. The API is described in
// docs/sources/senado.md.
package senado

import (
	"errors"
	"slices"
	"time"

	"geoint/internal/connector"
	"geoint/internal/connector/legis"
)

// Key is the source key.
const Key = "senado"

// sourceID is the id of the senado row in sources.All.
const sourceID int16 = 4

// DefaultBaseURL is the root of the API (SENADO_BASE_URL overrides it).
const DefaultBaseURL = "https://legis.senado.leg.br/dadosabertos"

// DefaultLegislatures are the current legislature (57th, 2023-02-01 to
// 2027-01-31, confirmed on both APIs on 2026-11-12) and the two before it.
var DefaultLegislatures = []int{55, 56, 57}

// Body is the public_body entity of the Senate.
const (
	bodyKey  = "senado"
	bodyName = "Senado Federal"
)

// ErrScope: the Senado is collected whole (Scope.Full only).
var ErrScope = errors.New("senado: only a full scope is supported")

// senado implements connector.Connector.
type senado struct {
	// fetched: the request URL of every file Fetch stored, for Load.
	fetched legis.URLBook

	baseURL      string
	legislatures []int

	// Client tuning; tests shorten it.
	pace, backoff, maxBackoff time.Duration
	attempts                  int
	maxBody                   int64
	maxRequests               int
	timeout                   time.Duration // the whole Fetch
}

// New returns the Senado connector reading from baseURL (DefaultBaseURL when
// empty) for the given legislatures (DefaultLegislatures when empty).
func New(baseURL string, legislatures []int) connector.Connector {
	return newSenado(baseURL, legislatures)
}

func newSenado(baseURL string, legislatures []int) *senado {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if len(legislatures) == 0 {
		legislatures = DefaultLegislatures
	}
	legislatures = slices.Clone(legislatures)
	slices.Sort(legislatures)
	legislatures = slices.Compact(legislatures)
	return &senado{
		baseURL:      baseURL,
		legislatures: legislatures,
		pace:         500 * time.Millisecond,
		backoff:      2 * time.Second,
		maxBackoff:   2 * time.Minute,
		attempts:     6,
		maxBody:      16 << 20,
		// Three lists and the affiliations of about 400 senators.
		maxRequests: 3000,
		timeout:     2 * time.Hour,
	}
}

func (c *senado) Key() string { return Key }
