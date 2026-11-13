// Package cvm is the connector of the Comissão de Valores Mobiliários open
// data: the shareholding position (posição acionária) that every listed
// company (companhia aberta) declares in its yearly Formulário de Referência
// (FRE), published as one zip of Latin-1 CSVs per year. It links each listed
// company to its direct shareholders with their percentage of the total
// capital (shareholder_of) and to the shareholders it declares as
// controllers (controls). The format is described in docs/sources/cvm.md.
package cvm

import (
	"net/http"
	"os"
	"time"

	"geoint/internal/claims"
	"geoint/internal/connector"
)

// Key is the source key.
const Key = "cvm"

// sourceID is the id of the cvm row in sources.All.
const sourceID int16 = 5

// DefaultBaseURL is the folder of the FRE zips on the CVM open-data portal.
const DefaultBaseURL = "https://dados.cvm.gov.br/dados/CIA_ABERTA/DOC/FRE/DADOS/"

// DefaultYearsBack is how many years before the reference date's year are
// collected too: the FRE of a year is delivered during that year, so the
// previous year's zip covers the companies that have not filed yet.
const DefaultYearsBack = 1

// Size limits. In 2026-11 a year's zip had 8 MB and its shareholding entry
// 7.2 MB uncompressed; the limits leave ample room and stop a zip bomb.
const (
	defaultMaxDownloadBytes int64 = 1 << 30 // one zip
	defaultMaxEntryBytes    int64 = 1 << 30 // the shareholding entry, uncompressed
)

// cvm implements connector.Connector.
type cvm struct {
	baseURL   string
	yearsBack int
	client    *http.Client
	spoolDir  string

	// Download tuning; tests shorten them.
	attempts    int
	backoff     time.Duration // first retry delay, doubled up to maxBackoff
	maxBackoff  time.Duration
	idleTimeout time.Duration // a download that receives nothing for this long is retried

	maxDownloadBytes int64
	maxEntryBytes    int64
}

// New returns the CVM connector reading from baseURL (DefaultBaseURL when
// empty). It collects the FRE of the reference date's year and of the
// DefaultYearsBack years before it. It is registered by the worker, which
// reads CVM_BASE_URL.
func New(baseURL string) connector.Connector { return newCVM(baseURL) }

func newCVM(baseURL string) *cvm {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &cvm{
		baseURL:          baseURL,
		yearsBack:        DefaultYearsBack,
		client:           newHTTPClient(),
		spoolDir:         os.Getenv("RAW_SPOOL_DIR"),
		attempts:         6,
		backoff:          5 * time.Second,
		maxBackoff:       2 * time.Minute,
		idleTimeout:      2 * time.Minute,
		maxDownloadBytes: defaultMaxDownloadBytes,
		maxEntryBytes:    defaultMaxEntryBytes,
	}
}

func (c *cvm) Key() string { return Key }

// years are the FRE years collected for a reference date, oldest first.
func (c *cvm) years(ref time.Time) []int {
	y := ref.UTC().Year()
	out := make([]int, 0, c.yearsBack+1)
	for i := c.yearsBack; i >= 0; i-- {
		out = append(out, y-i)
	}
	return out
}

// RelTypes are the relationship types this connector writes, each with
// whether it may join two entity-only ends (no person record at either
// end). No two connectors may write the same type between entity-only ends:
// such rows share one open natural key across sources, and CloseUnseen
// lets only the source that wrote a row first close it
// (internal/connector reltypes_test.go checks both rules).
var RelTypes = map[claims.RelType]bool{claims.ShareholderOf: true, claims.Controls: true}
