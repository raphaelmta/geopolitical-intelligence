// Package camara is the connector of the Câmara dos Deputados open-data API
// (Dados Abertos, REST v2): who held a federal deputy's mandate, in which
// legislatures, for which party and state, and when. The API is described in
// docs/sources/camara.md.
package camara

import (
	"errors"
	"slices"
	"time"

	"geoint/internal/connector"
	"geoint/internal/connector/legis"
)

// Key is the source key.
const Key = "camara"

// sourceID is the id of the camara row in sources.All.
const sourceID int16 = 3

// DefaultBaseURL is the root of the API (CAMARA_BASE_URL overrides it).
const DefaultBaseURL = "https://dadosabertos.camara.leg.br/api/v2"

// DefaultLegislatures are the current legislature (57th, 2023-02-01 to
// 2027-01-31, confirmed on the API on 2026-11-12) and the two before it.
var DefaultLegislatures = []int{55, 56, 57}

// Body is the public_body entity of the chamber.
const (
	bodyKey  = "camara"
	bodyName = "Câmara dos Deputados"
)

// ErrScope: the Câmara is collected whole (Scope.Full only).
var ErrScope = errors.New("camara: only a full scope is supported")

// camara implements connector.Connector.
type camara struct {
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

// New returns the Câmara connector reading from baseURL (DefaultBaseURL when
// empty) for the given legislatures (DefaultLegislatures when empty).
func New(baseURL string, legislatures []int) connector.Connector {
	return newCamara(baseURL, legislatures)
}

func newCamara(baseURL string, legislatures []int) *camara {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if len(legislatures) == 0 {
		legislatures = DefaultLegislatures
	}
	legislatures = slices.Clone(legislatures)
	slices.Sort(legislatures)
	legislatures = slices.Compact(legislatures)
	return &camara{
		baseURL:      baseURL,
		legislatures: legislatures,
		pace:         500 * time.Millisecond,
		backoff:      2 * time.Second,
		maxBackoff:   2 * time.Minute,
		attempts:     6,
		maxBody:      16 << 20,
		// Three legislatures have about 2,000 distinct deputies, two
		// requests each, plus about 35 list pages.
		maxRequests: 12000,
		timeout:     6 * time.Hour,
	}
}

func (c *camara) Key() string { return Key }
