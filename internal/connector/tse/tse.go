// Package tse is the connector of the Tribunal Superior Eleitoral open data:
// candidacies (consulta_cand) and the receipts of candidates' campaign
// finance (prestação de contas), published per election year as zips of
// Latin-1 CSVs, one per UF. It links candidates to parties and donors
// (people, companies, parties, other candidates) to candidates. The format
// is described in docs/sources/tse.md.
package tse

import (
	"net/http"
	"os"
	"slices"
	"time"

	"geoint/internal/claims"
	"geoint/internal/connector"
)

// Key is the source key.
const Key = "tse"

// sourceID is the id of the tse row in sources.All.
const sourceID int16 = 2

// DefaultBaseURL is the CDN folder of the TSE open data (dados abertos).
const DefaultBaseURL = "https://cdn.tse.jus.br/estatistica/sead/odsele/"

// DefaultYears are the election years collected when New gets none.
var DefaultYears = []int{2018, 2020, 2022, 2024, 2026}

// Size limits. The largest file in 2026-11 was a 1.37 GB zip whose largest
// loaded CSV entry had 274 MB; the limits leave room and stop a zip bomb.
const (
	defaultMaxDownloadBytes int64 = 8 << 30  // one zip
	defaultMaxEntryBytes    int64 = 4 << 30  // one CSV entry, uncompressed
	defaultMaxTotalBytes    int64 = 16 << 30 // the loaded entries of one zip, uncompressed
)

// tse implements connector.Connector.
type tse struct {
	baseURL  string
	years    []int
	client   *http.Client
	spoolDir string

	// Download tuning; tests shorten them.
	attempts    int
	backoff     time.Duration // first retry delay, doubled up to maxBackoff
	maxBackoff  time.Duration
	idleTimeout time.Duration // a download that receives nothing for this long is retried

	maxDownloadBytes int64
	maxEntryBytes    int64
	maxTotalBytes    int64

	// workers bounds how many zips are parsed and copied at once.
	workers int
}

// New returns the TSE connector reading from baseURL (DefaultBaseURL when
// empty) for the given election years (DefaultYears when empty). It is
// registered by the worker, which reads TSE_BASE_URL.
func New(baseURL string, years []int) connector.Connector { return newTSE(baseURL, years) }

func newTSE(baseURL string, years []int) *tse {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if len(years) == 0 {
		years = DefaultYears
	}
	years = slices.Clone(years)
	slices.Sort(years)
	years = slices.Compact(years)
	return &tse{
		baseURL:          baseURL,
		years:            years,
		client:           newHTTPClient(),
		spoolDir:         os.Getenv("RAW_SPOOL_DIR"),
		attempts:         6,
		backoff:          5 * time.Second,
		maxBackoff:       2 * time.Minute,
		idleTimeout:      2 * time.Minute,
		maxDownloadBytes: defaultMaxDownloadBytes,
		maxEntryBytes:    defaultMaxEntryBytes,
		maxTotalBytes:    defaultMaxTotalBytes,
		workers:          4,
	}
}

func (t *tse) Key() string { return Key }

// RelTypes are the relationship types this connector writes, each with
// whether it may join two entity-only ends (no person record at either
// end). No two connectors may write the same type between entity-only ends:
// such rows share one open natural key across sources, and CloseUnseen
// lets only the source that wrote a row first close it
// (internal/connector reltypes_test.go checks both rules).
var RelTypes = map[claims.RelType]bool{claims.DonatedTo: false, claims.CandidateFor: false}
