// Package receita is the connector of the Receita Federal open CNPJ data:
// companies (Empresas), their head offices (Estabelecimentos) and partners
// (Socios), published monthly as zip files of headerless Latin-1 CSVs.
// The format is described in docs/sources/receita.md.
package receita

import (
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"geoint/internal/claims"
	"geoint/internal/connector"
)

// Key is the source key.
const Key = "receita"

// sourceID is the id of the receita row in sources.All.
const sourceID int16 = 1

// DefaultBaseURL is the public WebDAV folder of the CNPJ data (one subfolder
// per month). The user part is the token of the public share, sent as the
// Basic auth user with an empty password; see docs/sources/receita.md.
const DefaultBaseURL = "https://gn672Ad4CF8N6TK@arquivos.receitafederal.gov.br/public.php/webdav/Dados/Cadastros/CNPJ/"

// adminQualifications are the qualificacao_socio codes that make a partner
// row also an administrator_of relationship (table "Qualificações que
// indicam administração" in docs/sources/receita.md). Temporary
// administration (11 Interventor, 13 Liquidante, 64 Administrador Judicial)
// is not included.
var adminQualifications = []string{
	"05", // Administrador
	"08", // Conselheiro de Administração
	"10", // Diretor
	"16", // Presidente
	"28", // Sócio-Gerente
	"49", // Sócio-Administrador
	"50", // Empresário (titular of a sole proprietorship)
	"65", // Titular Pessoa Física Residente ou Domiciliado no Brasil
	"70", // Administrador Residente ou Domiciliado no Exterior
	"71", // Conselheiro de Administração Residente ou Domiciliado no Exterior
	"72", // Diretor Residente ou Domiciliado no Exterior
	"73", // Presidente Residente ou Domiciliado no Exterior
	"74", // Sócio-Administrador Residente ou Domiciliado no Exterior
}

// receita implements connector.Connector.
type receita struct {
	baseURL  string
	client   *http.Client
	spoolDir string

	// Download tuning; tests shorten them.
	attempts    int
	backoff     time.Duration // first retry delay, doubled up to maxBackoff
	maxBackoff  time.Duration
	idleTimeout time.Duration // a download that receives nothing for this long is retried

	// workers bounds how many files are parsed and copied at once.
	workers int

	// Edition completeness (edition.go). minParts: parts required per data
	// kind when no full collection was loaded before (default 10), or
	// always when RECEITA_MIN_PARTS is set explicitly (minPartsExplicit: the
	// owner's override when the Receita legitimately publishes fewer parts
	// than before). settle: how old the newest file of an edition must be
	// (RECEITA_SETTLE_HOURS, default 6; 0 disables). now is the clock.
	minParts         int
	minPartsExplicit bool
	settle           time.Duration
	now              func() time.Time

	// judged holds, per month, the data parts ResolveReference found
	// complete; Fetch refuses a listing that differs from it.
	mu     sync.Mutex
	judged map[string][]remoteFile
}

// Defaults of the completeness knobs.
const (
	defaultMinParts    = 10
	defaultSettleHours = 6
)

// New returns the Receita connector reading from baseURL (DefaultBaseURL
// when empty). It is registered by the worker, which reads RECEITA_BASE_URL.
func New(baseURL string) connector.Connector { return newReceita(baseURL) }

func newReceita(baseURL string) *receita {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	minParts, explicit := envIntSet("RECEITA_MIN_PARTS", defaultMinParts, 1)
	return &receita{
		baseURL:          baseURL,
		client:           newHTTPClient(),
		spoolDir:         os.Getenv("RAW_SPOOL_DIR"),
		attempts:         6,
		backoff:          5 * time.Second,
		maxBackoff:       2 * time.Minute,
		idleTimeout:      2 * time.Minute,
		workers:          4,
		minParts:         minParts,
		minPartsExplicit: explicit,
		settle:           time.Duration(envInt("RECEITA_SETTLE_HOURS", defaultSettleHours, 0)) * time.Hour,
		now:              time.Now,
		judged:           map[string][]remoteFile{},
	}
}

// envInt reads a non-negative integer setting; empty, malformed or below min
// gives def (logged, so a typo is visible).
func envInt(name string, def, minimum int) int {
	n, _ := envIntSet(name, def, minimum)
	return n
}

// envIntSet is envInt that also reports whether a valid value was set.
func envIntSet(name string, def, minimum int) (int, bool) {
	v := os.Getenv(name)
	if v == "" {
		return def, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minimum {
		slog.Warn("receita: ignoring invalid setting", "name", name, "value", v, "default", def)
		return def, false
	}
	return n, true
}

func (r *receita) Key() string { return Key }

// RelTypes are the relationship types this connector writes, each with
// whether it may join two entity-only ends (no person record at either
// end). No two connectors may write the same type between entity-only ends:
// such rows share one open natural key across sources, and CloseUnseen
// lets only the source that wrote a row first close it
// (internal/connector reltypes_test.go checks both rules).
var RelTypes = map[claims.RelType]bool{claims.PartnerOf: true, claims.AdministratorOf: true, claims.LegalRepOf: false}
