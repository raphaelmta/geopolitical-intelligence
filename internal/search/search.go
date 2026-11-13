// Package search finds entities by name or by CNPJ, implementing the design
// measured in docs/spikes/2026-11-receita-path-queries.md (block
// SEARCH_STRATEGY), which answers in well under 1 s on the national base.
// docs/search.md summarizes it and the decisions taken on top of it.
//
// Name search runs over derived tables, one per group of kinds, rebuilt from
// scratch after every collection (Rebuild) and swapped in by renaming:
//
//   - search_company: companies;
//   - search_person: persons that still have a person record (entities left
//     empty by identity merges are not searchable);
//   - search_other: foreign partners ('E'), parties ('A') and public bodies
//     ('O'), small kinds kept apart so they never skew the statistics of the
//     two big tables.
//
// Each row carries search_rank, the position of the entity in the order
// degree DESC, normalized name (byte order), entity id, numbered across all
// tables. Degree is the number of open relationships of the entity, all
// sources. The tables are written in search_rank order and a query relies on
// that physical order: with parallelism and synchronized scans off, a scan
// with LIMIT and no ORDER BY returns the best ranked rows first, which is what
// keeps frequent words fast.
//
// A query has two tiers. First, entities whose normalized name equals the
// normalized query. Then, if the limit is not reached, entities having every
// kept word of the query (tsvector, GIN), the last typed word matching as a
// prefix when it has at least 3 characters and is not a frequent word. Each
// tier is in search_rank order. Normalization is the SQL function search_norm
// (unaccent, upper case, collapsed spaces) on both sides. The words are
// decided once, by the SQL function search_words (migration 0008), for both
// the index and the query: runs of [a-z0-9], without 15 stopwords (de, da,
// do, das, dos, e, a, o, em, ltda, me, epp, eireli, s, sa), without (a) any
// word containing 5 or more consecutive digits and, (n) when the whole name
// (or query) has 9 or more ASCII digits in total, without every word that
// contains a digit. Guarantee: a name with 9 or more digits in total, which
// includes any name carrying a CPF (11) or CNPJ (14) however it is spelled,
// is not searchable by any of its words with digits, so no query with right
// or wrong digits can match it through them; it is found only by its
// digit-free words, never by typing it whole. The cost: digits in such names,
// and long numbers anywhere, are not searchable. Short numbers in other names
// still are (POSTO 15, C6 BANK, RUA 7 123). The exact tier is skipped for a
// query with a CPF shape (see MaskCPF) or 9 or more digits. At most 10 words
// of a query are used; a query of more than 200 characters is rejected.
// MaskCPF masks the CPF spellings it recognizes in displayed names; a CPF
// written in another way is shown as the source published it.
//
// A query carrying a CPF (11 digits, punctuated or not, or the masked form
// ***NNNNNN** the sources publish; see extractCPF) is a CPF-fragment search
// instead: the platform stores only the 6 middle digits of a CPF, so it
// lists the persons having a record with those 6 digits — candidates, never a
// confirmation — restricted by the other words of the query if any. A full
// CPF with wrong check digits is ErrInvalidCPF; two different CPFs,
// ErrMultipleCPFs. The CPF is taken out of the query before the word rules
// run, and names are still never matched by their digits.
//
// A query made only of digits (after removing . / - and spaces) and not
// 11 digits is a CNPJ lookup instead: 8 or 14 digits find the company with
// that base, 3 to 7 digits list companies whose base starts with them, other
// lengths find nothing. An alphanumeric CNPJ (8 characters, or 12 plus 2
// check digits, mixing letters and digits) is looked up by its base first
// and falls back to name search.
//
// Accepted losses, as in the spike: no substring match inside a word (BRAS
// does not find PETROBRAS), no tolerance for typos (OLIVERA does not find
// OLIVEIRA), no synonyms or abbreviations, a query of only stopwords or with
// fewer than 3 useful characters (OI, 3M) is rejected (such names are found
// only by CNPJ), names lose their long numeric sequences, a frequent last
// word matches only whole, word order is ignored, and results are ordered by
// degree, not by relevance. After a rebuild in another process, a Service may
// use the previous set of frequent words for up to 30 s.
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	unorm "golang.org/x/text/unicode/norm"

	"geoint/internal/claims"
	"geoint/internal/graph"
)

// Errors returned by Query.
var (
	// ErrQueryTooShort: fewer than 3 useful characters (letters or digits of
	// words that are searched; for a numeric query, digits).
	ErrQueryTooShort = errors.New("query too short")
	// ErrQueryTooLong: more than 200 characters.
	ErrQueryTooLong = errors.New("query too long")
	ErrBadKind      = errors.New("unknown kind")
)

const (
	defaultLimit = 20
	maxLimit     = 50
	minUseful    = 3
	maxRunes     = 200
	maxWords     = 10
	// DefaultFrequentThreshold: a word in at least this many names of all
	// search tables together is frequent (SEARCH_STRATEGY 6).
	DefaultFrequentThreshold = 100000
	// generationCheckEvery bounds how often a Service checks whether the
	// search tables were swapped by another process.
	generationCheckEvery = 30 * time.Second
)

// querySettings apply to one query transaction only (SEARCH_STRATEGY 10, plus
// a statement timeout): never set on the pooled session.
const querySettings = `SET LOCAL statement_timeout = '5s';
SET LOCAL plan_cache_mode = force_custom_plan;
SET LOCAL max_parallel_workers_per_gather = 0;
SET LOCAL synchronize_seqscans = off`

// searchTable is one search table. hasKind: the table holds several kinds,
// told apart by the "char" code in its kind column.
type searchTable struct {
	name    string
	hasKind bool
}

var (
	companyTable = searchTable{name: "search_company"}
	personTable  = searchTable{name: "search_person"}
	otherTable   = searchTable{name: "search_other", hasKind: true}
	allTables    = []searchTable{companyTable, personTable, otherTable}
)

// kindRoute says where a kind lives. code is a constant, never user input.
type kindRoute struct {
	table searchTable
	code  string
}

var routes = map[claims.EntityKind]kindRoute{
	claims.KindCompany:    {companyTable, "C"},
	claims.KindPerson:     {personTable, "P"},
	claims.KindForeign:    {otherTable, "E"},
	claims.KindParty:      {otherTable, "A"},
	claims.KindPublicBody: {otherTable, "O"},
}

// tierSQL holds, for one kind filter, the two tier queries.
type tierSQL struct{ exact, words string }

var tiers = buildTiers()

func buildTiers() map[claims.EntityKind]tierSQL {
	out := map[claims.EntityKind]tierSQL{}
	out[""] = tierSQL{exact: tierQuery(allTables, "", true), words: tierQuery(allTables, "", false)}
	for k, r := range routes {
		code := ""
		if r.table.hasKind {
			code = r.code
		}
		out[k] = tierSQL{exact: tierQuery([]searchTable{r.table}, code, true), words: tierQuery([]searchTable{r.table}, code, false)}
	}
	return out
}

// tierQuery builds a tier over tables (SEARCH_STRATEGY 12 and 13), joined to
// entity for the display name and natural key. Exact tier: $1 = normalized
// query, $2 = limit. Word tier: $1 = tsquery, $2 = normalized name to
// exclude, $3 = limit; its branches have no ORDER BY on purpose (see the
// package comment).
func tierQuery(tables []searchTable, code string, exact bool) string {
	branches := make([]string, len(tables))
	for i, t := range tables {
		where := "tokens @@ $1::tsquery AND name_norm <> $2"
		tail := "LIMIT $3"
		if exact {
			where = "name_norm = $1"
			tail = "ORDER BY search_rank LIMIT $2"
		}
		if code != "" {
			where += ` AND kind = '` + code + `'::"char"`
		}
		branches[i] = fmt.Sprintf("(SELECT search_rank, entity_id FROM %s WHERE %s %s)", t.name, where, tail)
	}
	limit := "$3"
	if exact {
		limit = "$2"
	}
	ranked := branches[0]
	if len(branches) > 1 {
		ranked = "(SELECT * FROM (" + strings.Join(branches, " UNION ALL ") + ") u ORDER BY search_rank LIMIT " + limit + ")"
	}
	return `SELECT e.id, e.kind, e.name, coalesce(e.natural_key, '')
		FROM ` + ranked + ` r JOIN entity e ON e.id = r.entity_id
		ORDER BY r.search_rank`
}

// Service answers searches and rebuilds the search tables. It is safe for
// concurrent use.
type Service struct {
	pool              *pgxpool.Pool
	frequentThreshold int64
	fragmentCap       int
	checkEvery        time.Duration
	swapLockTimeout   time.Duration
	swapAttempts      int
	swapBackoff       time.Duration
	// Test hooks, called by Rebuild while it holds the rebuild lock.
	afterLock  func()
	beforeSwap func(attempt int)

	mu       sync.Mutex
	loaded   bool
	gen      int64
	checked  time.Time
	frequent map[string]struct{}
	epoch    int64         // bumped by invalidate
	inflight chan struct{} // closed when the running reload ends
}

// New returns a Service on the pool.
func New(p *pgxpool.Pool) *Service {
	return &Service{pool: p, frequentThreshold: DefaultFrequentThreshold, fragmentCap: DefaultFragmentCap, checkEvery: generationCheckEvery,
		swapLockTimeout: defaultSwapLockTimeout, swapAttempts: defaultSwapAttempts, swapBackoff: defaultSwapBackoff}
}

// Result is the answer to a search.
type Result struct {
	Items []graph.EntityRef
	// CPFFragment: the query carried a CPF and was answered by matching its
	// 6 middle digits against the person records' CPF fragments. Every item
	// is then a candidate (a person whose fragment matches), never a
	// confirmed holder of that CPF.
	CPFFragment bool
	// FragmentHits: the first FragmentHits items are those candidates; the
	// items after them (a CPF query with name words and no kind) are
	// companies found by the words alone, as in name search — the CPF is
	// never used for them.
	FragmentHits int
	// FragmentTooCommon: the fragment has more than the cap's records
	// (DefaultFragmentCap) and the query has no name words to narrow it, so
	// nothing is listed.
	FragmentTooCommon bool
}

// DefaultFragmentCap bounds a CPF search started from the fragment: a
// fragment with more records than this (fillers such as 000000-like values
// the sources use; on the national base the median is 24 records and the 99th
// percentile 109, but a few fragments have thousands and one 465,539) is not
// listed. With name words, such a fragment is searched from the name side.
const DefaultFragmentCap = 2000

// SetFragmentCap sets the most records a CPF fragment may have to be listed
// from the fragment side (DefaultFragmentCap unless changed). Call it before
// the Service is shared.
func (s *Service) SetFragmentCap(n int) { s.fragmentCap = n }

// Query is Find returning only the items.
func (s *Service) Query(ctx context.Context, q string, kind claims.EntityKind, limit int) ([]graph.EntityRef, error) {
	r, err := s.Find(ctx, q, kind, limit)
	return r.Items, err
}

// Find searches q among entities of kind ("" for all kinds) and returns at
// most limit results (limit ≤ 0 means 20; more than 50 means 50). Display
// names have CPFs masked (MaskCPF). q is never logged.
//
// A query carrying a CPF (extractCPF) is a CPF-fragment search: persons with
// a record whose fragment is the CPF's 6 middle digits, restricted by the
// other words of the query when there are any (fragmentSQL). Only persons
// have records: kind company, party, public_body or foreign answers nothing.
// The CPF is taken out before the word rules run, so they apply to the other
// words unchanged; names are never matched by digits.
func (s *Service) Find(ctx context.Context, q string, kind claims.EntityKind, limit int) (Result, error) {
	sql, ok := tiers[kind]
	if !ok {
		return Result{}, ErrBadKind
	}
	if utf8.RuneCountInString(q) > maxRunes {
		return Result{}, ErrQueryTooLong
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)
	// PostgreSQL text holds neither NUL nor invalid UTF-8.
	q = strings.ReplaceAll(strings.ToValidUTF8(q, ""), "\x00", "")
	// Compatibility forms (fullwidth digits, asterisks and punctuation) are
	// folded before looking for a CPF, so it never escapes as "a word".
	cpf, isCPF, err := extractCPF(unorm.NFKC.String(q))
	if err != nil {
		return Result{}, err
	}
	if isCPF {
		return s.byFragment(ctx, cpf, kind, limit)
	}
	if digits, ok := numericQuery(q); ok {
		out, err := s.byCNPJ(ctx, digits, kind, limit)
		return Result{Items: out}, err
	}
	out, err := s.byName(ctx, q, sql, kind, limit)
	return Result{Items: out}, err
}

// byName is the name search (the two tiers), with the alphanumeric CNPJ
// lookup first.
func (s *Service) byName(ctx context.Context, q string, sql tierSQL, kind claims.EntityKind, limit int) ([]graph.EntityRef, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)

	if base, ok := alphanumericCNPJ(q); ok && (kind == "" || kind == claims.KindCompany) {
		out, err := collect(ctx, tx, nil, cnpjExactSQL, base)
		if err != nil {
			return nil, fmt.Errorf("search: cnpj: %w", err)
		}
		if len(out) > 0 {
			return out, commit(ctx, tx)
		}
	}

	norm, kept, lastSearched, err := words(ctx, tx, q)
	if err != nil {
		return nil, err
	}
	useful := 0
	for _, w := range kept {
		useful += len(w)
	}
	if norm == nil || useful < minUseful {
		return nil, ErrQueryTooShort
	}
	frequent, err := s.frequentTokens(ctx, tx)
	if err != nil {
		return nil, err
	}
	tsq := tsQuery(kept, lastSearched, frequent)

	out, err := nameTiers(ctx, tx, nil, sql, *norm, tsq, limit)
	if err != nil {
		return nil, err
	}
	return out, commit(ctx, tx)
}

// nameTiers appends to out the two name tiers for the normalized query norm
// and its tsquery, up to limit items in all.
func nameTiers(ctx context.Context, tx pgx.Tx, out []graph.EntityRef, sql tierSQL, norm, tsq string, limit int) ([]graph.EntityRef, error) {
	var err error
	// A query with a CPF in it, or with 9 or more digits in total, is not
	// looked up by its exact form, so the exact tier never confirms digits
	// that go with a name (such names have no searchable digit words
	// either): the word tier alone answers it, excluding nothing ("" equals
	// no normalized name).
	exclude := ""
	if !hasCPF(norm) && countDigits(norm) < longNumberDigits {
		if out, err = collect(ctx, tx, out, sql.exact, norm, limit-len(out)); err != nil {
			return nil, fmt.Errorf("search: exact names: %w", err)
		}
		exclude = norm
	}
	if len(out) < limit && tsq != "" {
		if out, err = collect(ctx, tx, out, sql.words, tsq, exclude, limit-len(out)); err != nil {
			return nil, fmt.Errorf("search: words: %w", err)
		}
	}
	return out, nil
}

// words normalizes q and returns its searched words. They come from
// search_words, the same SQL function the index is built with, so query and
// index can never apply different rules. Words after the 10th searched one
// are ignored: the 10th becomes the last typed word.
func words(ctx context.Context, tx pgx.Tx, q string) (norm *string, kept []string, lastSearched bool, err error) {
	err = tx.QueryRow(ctx, `SELECT n,
		coalesce((SELECT array_agg(word ORDER BY pos) FROM search_words(n) WHERE searched), '{}'),
		coalesce((SELECT searched FROM search_words(n) ORDER BY pos DESC LIMIT 1), false)
		FROM search_norm($1) AS n`, q).Scan(&norm, &kept, &lastSearched)
	if err != nil {
		return nil, nil, false, fmt.Errorf("search: normalize: %w", err)
	}
	if len(kept) > maxWords {
		kept, lastSearched = kept[:maxWords], true
	}
	return norm, kept, lastSearched, nil
}

// fragmentSQL answers a CPF-fragment search (Find). It starts from the
// fragment's records through person_record_cpf_fragment_entity (migration
// 0022): a fragment below the cap is shared by tens of persons (median 24
// records on the national base), so the candidate set is small and
// everything else is computed on it — never the ranked name search filtered
// afterwards. $1 = fragment, $2 = tsquery of the other words (empty for
// none), $3 = limit.
//
// Same order as the name tiers (search_rank): degree DESC, normalized name
// (byte order), id. The degree is counted here, from the live relationships,
// with the rebuild's definition (open relationships touching the entity, a
// relationship with itself once), so it can differ from the search tables'
// between two rebuilds. The words match the same tsvector the search tables
// index (search_tokens of search_norm of the name).
const fragmentSQL = `WITH c AS (
		SELECT DISTINCT pr.entity_id FROM person_record pr
		WHERE pr.cpf_fragment = $1 AND pr.cpf_fragment <> '' AND pr.entity_id IS NOT NULL),
	m AS (
		SELECT e.id, e.kind, e.name, coalesce(e.natural_key, '') AS natural_key,
			search_norm(e.name) COLLATE "C" AS name_norm
		FROM c JOIN entity e ON e.id = c.entity_id
		WHERE e.kind = 'person')
	SELECT m.id, m.kind, m.name, m.natural_key
	FROM m CROSS JOIN LATERAL (
		SELECT (SELECT count(*) FROM relationship r
				WHERE r.src_entity_id = m.id AND r.closed_collection_id IS NULL)
			+ (SELECT count(*) FROM relationship r
				WHERE r.dst_entity_id = m.id AND r.src_entity_id <> m.id AND r.closed_collection_id IS NULL) AS grau) d
	WHERE m.name_norm IS NOT NULL
		AND CASE WHEN $2::text = '' THEN true ELSE search_tokens(m.name_norm) @@ $2::text::tsquery END
	ORDER BY d.grau DESC, m.name_norm, m.id
	LIMIT $3`

// fragmentProbeSQL counts a fragment's records, stopping at $2 (the cap + 1):
// a bounded index-only probe, whatever the fragment.
const fragmentProbeSQL = `SELECT count(*) FROM (SELECT 1 FROM person_record
	WHERE cpf_fragment = $1 AND cpf_fragment <> '' AND entity_id IS NOT NULL LIMIT $2) x`

// fragmentByNameSQL answers a CPF search with name words on a fragment above
// the cap: from the name side (the word tier of search_person, in
// search_rank order, relying on the physical order like the word tier), each
// candidate checked for a record with the fragment through the same index
// (entity_id is in it). $1 = tsquery, $2 = fragment, $3 = limit.
const fragmentByNameSQL = `SELECT e.id, e.kind, e.name, coalesce(e.natural_key, '')
	FROM (SELECT search_rank, entity_id FROM search_person sp
		WHERE tokens @@ $1::tsquery
			AND EXISTS (SELECT 1 FROM person_record pr
				WHERE pr.cpf_fragment = $2 AND pr.cpf_fragment <> '' AND pr.entity_id = sp.entity_id)
		LIMIT $3) r
	JOIN entity e ON e.id = r.entity_id
	ORDER BY r.search_rank`

// byFragment is the CPF-fragment search: the other words of the query, if
// any, restrict the candidates (an intersection, never a ranking). Words
// that the rules drop (stopwords, digits) leave the fragment alone.
func (s *Service) byFragment(ctx context.Context, cpf cpfInQuery, kind claims.EntityKind, limit int) (Result, error) {
	res := Result{CPFFragment: true}
	if kind != "" && kind != claims.KindPerson {
		// Only persons have CPF fragments.
		return res, nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer rollback(ctx, tx)
	tsq, norm, useful := "", "", 0
	if strings.TrimSpace(cpf.rest) != "" {
		n, kept, lastSearched, err := words(ctx, tx, cpf.rest)
		if err != nil {
			return Result{}, err
		}
		if len(kept) > 0 {
			frequent, err := s.frequentTokens(ctx, tx)
			if err != nil {
				return Result{}, err
			}
			tsq = tsQuery(kept, lastSearched, frequent)
		}
		for _, w := range kept {
			useful += len(w)
		}
		if n != nil {
			norm = *n
		}
	}
	var records int
	if err := tx.QueryRow(ctx, fragmentProbeSQL, cpf.fragment, s.fragmentCap+1).Scan(&records); err != nil {
		return Result{}, fmt.Errorf("search: cpf fragment probe: %w", err)
	}
	switch {
	case records <= s.fragmentCap:
		res.Items, err = collect(ctx, tx, nil, fragmentSQL, cpf.fragment, tsq, limit)
	case tsq != "" && useful >= minUseful:
		res.Items, err = collect(ctx, tx, nil, fragmentByNameSQL, tsq, cpf.fragment, limit)
	default:
		res.FragmentTooCommon = true
	}
	if err != nil {
		return Result{}, fmt.Errorf("search: cpf fragment: %w", err)
	}
	res.FragmentHits = len(res.Items)
	// No kind and name words: companies are searched by the words alone (a
	// pasted company name that shows a masked CPF still finds the company).
	if kind == "" && tsq != "" && useful >= minUseful && len(res.Items) < limit {
		if res.Items, err = nameTiers(ctx, tx, res.Items, tiers[claims.KindCompany], norm, tsq, limit); err != nil {
			return Result{}, err
		}
	}
	return res, commit(ctx, tx)
}

// rollback ends a read-only transaction that was not committed (after a
// commit it does nothing).
func rollback(ctx context.Context, tx pgx.Tx) { _ = tx.Rollback(context.WithoutCancel(ctx)) }

func commit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("search: commit: %w", err)
	}
	return nil
}

// begin opens a read-only transaction with the search settings.
func (s *Service) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("search: begin: %w", err)
	}
	if _, err := tx.Exec(ctx, querySettings); err != nil {
		rollback(ctx, tx)
		return nil, fmt.Errorf("search: settings: %w", err)
	}
	return tx, nil
}

// collect appends the entities returned by sql to out, masking names.
func collect(ctx context.Context, tx pgx.Tx, out []graph.EntityRef, sql string, args ...any) ([]graph.EntityRef, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var r graph.EntityRef
		var kind string
		if err := rows.Scan(&r.ID, &kind, &r.Name, &r.NaturalKey); err != nil {
			return out, err
		}
		r.Kind = claims.EntityKind(kind)
		r.Name = MaskCPF(r.Name)
		out = append(out, r)
	}
	return out, rows.Err()
}

// stripCNPJ removes CNPJ punctuation (. / -) and spaces.
func stripCNPJ(q string) string {
	return strings.Map(func(r rune) rune {
		if r == '.' || r == '/' || r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, q)
}

// numericQuery reports whether q is only ASCII digits once CNPJ punctuation
// and spaces are removed, and returns the digits.
func numericQuery(q string) (string, bool) {
	d := stripCNPJ(q)
	return d, d != "" && allDigits(d)
}

// alphanumericCNPJ reports whether q, without CNPJ punctuation and spaces
// and upper-cased, looks like an alphanumeric CNPJ (valid since July 2026),
// and returns its base: 8 characters [0-9A-Z] with at least one letter and
// one digit, or 14 characters [0-9A-Z]{12}[0-9]{2} with at least one letter.
// Pure digits never get here (numericQuery comes first).
func alphanumericCNPJ(q string) (string, bool) {
	k := strings.ToUpper(stripCNPJ(q))
	if len(k) != 8 && len(k) != 14 {
		return "", false
	}
	letters, digits := 0, 0
	for i := 0; i < len(k); i++ {
		switch c := k[i]; {
		case c >= 'A' && c <= 'Z':
			letters++
		case c >= '0' && c <= '9':
			digits++
		default:
			return "", false
		}
	}
	if letters == 0 || (len(k) == 8 && digits == 0) || (len(k) == 14 && !allDigits(k[12:])) {
		return "", false
	}
	return k[:8], true
}

const (
	cnpjExactSQL = `SELECT id, kind, name, coalesce(natural_key, '') FROM entity
		WHERE kind = 'company' AND natural_key = $1`
	// The range operators and ORDER BY USING match entity_natural_key_prefix
	// (text_pattern_ops), so the scan walks that index in key order.
	cnpjPrefixSQL = `SELECT id, kind, name, coalesce(natural_key, '') FROM entity
		WHERE natural_key ~>=~ $1 AND natural_key ~<~ $2 AND kind = 'company'
		ORDER BY natural_key USING ~<~ LIMIT $3`
)

// byCNPJ looks companies up by CNPJ base: exact for 8 or 14 digits, prefix
// (in key order) for 3 to 7.
func (s *Service) byCNPJ(ctx context.Context, digits string, kind claims.EntityKind, limit int) ([]graph.EntityRef, error) {
	n := len(digits)
	if n < minUseful {
		return nil, ErrQueryTooShort
	}
	if (n != 8 && n != 14 && n >= 8) || (kind != "" && kind != claims.KindCompany) {
		// No CNPJ (11 digits, a CPF, never get here: extractCPF comes first).
		return nil, nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	var out []graph.EntityRef
	if n >= 8 {
		out, err = collect(ctx, tx, nil, cnpjExactSQL, digits[:8])
	} else {
		// Every key starting with digits sorts below digits with its last
		// byte incremented.
		upper := digits[:n-1] + string(digits[n-1]+1)
		out, err = collect(ctx, tx, nil, cnpjPrefixSQL, digits, upper, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("search: cnpj: %w", err)
	}
	return out, commit(ctx, tx)
}

// longNumberDigits: a name or query with this many ASCII digits in total
// has no searchable word with digits (search_words rule (n)) and skips the
// exact tier.
const longNumberDigits = 9

// countDigits counts the ASCII digits of s, as search_words does.
func countDigits(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			n++
		}
	}
	return n
}

func allDigits(w string) bool {
	for i := 0; i < len(w); i++ {
		if w[i] < '0' || w[i] > '9' {
			return false
		}
	}
	return true
}

// tsQuery joins the searched words with &. The last one gets a prefix match
// only if it is also the last typed word (lastSearched), has at least 3
// characters and is not frequent (SEARCH_STRATEGY 11). So a word dropped at
// the end (a stopword, a CPF) leaves the previous word matching whole, the
// same way whatever the dropped word was. Words are [a-z0-9] only: nothing to
// escape.
func tsQuery(kept []string, lastSearched bool, frequent map[string]struct{}) string {
	if len(kept) == 0 {
		return ""
	}
	parts := make([]string, len(kept))
	for i, w := range kept {
		parts[i] = "'" + w + "'"
	}
	last := kept[len(kept)-1]
	if _, isFrequent := frequent[last]; lastSearched && len(last) >= 3 && !isFrequent {
		parts[len(parts)-1] += ":*"
	}
	return strings.Join(parts, " & ")
}

// frequentTokens returns the cached frequent words, reloading them when the
// search tables were swapped. The generation is checked at most once per
// checkEvery, in the query's own transaction, by one query at a time and
// without holding the mutex during the round trip: meanwhile the others use
// the words already loaded, or wait (respecting ctx) when none are.
func (s *Service) frequentTokens(ctx context.Context, tx pgx.Tx) (map[string]struct{}, error) {
	for {
		s.mu.Lock()
		if s.loaded && (time.Since(s.checked) < s.checkEvery || s.inflight != nil) {
			f := s.frequent
			s.mu.Unlock()
			return f, nil
		}
		if wait := s.inflight; wait != nil {
			s.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		s.inflight = done
		known, epoch := int64(-1), s.epoch
		if s.loaded {
			known = s.gen
		}
		s.mu.Unlock()

		var gen int64
		var tokens []string
		err := tx.QueryRow(ctx, `SELECT generation,
			CASE WHEN generation <> $1 THEN ARRAY(SELECT token FROM search_frequent_token) END
			FROM search_state`, known).Scan(&gen, &tokens)

		s.mu.Lock()
		s.inflight = nil
		close(done)
		if err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("search: frequent words: %w", err)
		}
		if epoch == s.epoch {
			s.checked = time.Now()
			if gen != known {
				set := make(map[string]struct{}, len(tokens))
				for _, t := range tokens {
					set[t] = struct{}{}
				}
				s.frequent, s.gen, s.loaded = set, gen, true
			}
		}
		f := s.frequent
		if epoch != s.epoch && gen != known {
			// Invalidated while loading: nothing is stored (the next query
			// reloads), but this one uses what it read.
			f = make(map[string]struct{}, len(tokens))
			for _, t := range tokens {
				f[t] = struct{}{}
			}
		}
		s.mu.Unlock()
		return f, nil
	}
}

// invalidate makes the next query reload the frequent words.
func (s *Service) invalidate() {
	s.mu.Lock()
	s.loaded = false
	s.epoch++
	s.mu.Unlock()
}
