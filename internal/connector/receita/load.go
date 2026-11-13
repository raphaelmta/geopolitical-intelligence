package receita

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/identity"
	"geoint/internal/rawstore"
)

// loadLockKey is the session advisory lock held for a whole Load: the
// staging tables are shared, so two Receita loads never run at once.
const loadLockKey int64 = 0x7265636569746131 // "receita1"

// maxLoggedMalformed bounds the malformed-row warnings logged per file.
const maxLoggedMalformed = 5

// fileKind is one of the three CSV groups the loader reads.
type fileKind struct {
	name  string // Empresas, Estabelecimentos, Socios
	stat  string // stats prefix
	table string
	cols  int
	// columns of the staging table, in Values order.
	columns []string
	// newSource returns the COPY source for one file.
	newSource func(rr *recordReader, fileNo int16, fileName string) copySource
}

var kinds = map[string]*fileKind{
	"Empresas": {
		name: "Empresas", stat: "empresas", table: "receita_stg_empresa", cols: empresaCols,
		columns:   []string{"cnpj_basico", "name", "name_norm", "legal_nature", "size", "capital", "file_no", "line"},
		newSource: newEmpresaSource,
	},
	"Estabelecimentos": {
		name: "Estabelecimentos", stat: "estabelecimentos", table: "receita_stg_estab", cols: estabCols,
		columns:   []string{"cnpj_basico", "status", "uf", "city", "cnae", "file_no", "line"},
		newSource: newEstabSource,
	},
	"Socios": {
		name: "Socios", stat: "socios", table: "receita_stg_socio", cols: socioCols,
		columns: []string{"cnpj_basico", "partner_type", "name", "name_norm", "partner_base", "cpf_fragment", "foreign_key",
			"qual", "valid_from", "has_rep", "rep_name", "rep_name_norm", "rep_fragment", "rep_qual", "file_no", "line"},
		newSource: newSocioSource,
	},
}

// input is one raw file to load.
type input struct {
	file rawstore.File
	kind *fileKind
	no   int16
	col  int64  // the collection loading it
	url  string // where it was downloaded from (no credentials)
}

// Load reads the Empresas, Estabelecimentos and Socios zips among files
// (other files, such as the domain tables, are ignored) and writes the
// claims of the collection:
//
//  1. take the load lock, refuse an edition older than one already loaded,
//     even partly by a failed run (ErrOlderEdition), before writing anything, and truncate the staging
//     tables (migrations 0003, 0004); they are emptied again at the end,
//     also when the load fails;
//  2. per file, with bounded parallelism: spool the zip from raw to a local
//     file, check its SHA-256, record the file (raw key, checksum, size and
//     the inner CSV name that record_ref uses) in collection_file, stream its
//     CSV through the record reader,
//     validate each row in Go (normalized names, CPF fragments and CNPJ bases
//     are computed here, never in SQL) and COPY the valid rows into staging;
//     malformed rows are counted and skipped;
//  3. ANALYZE staging and, for a slice, keep only the reachable companies;
//  4. set-based SQL (upsert.go) writes entities, company versions, person
//     records and relationships with the natural keys of the claims model,
//     relationships in short per-bucket transactions under the identity lock;
//  5. ANALYZE the claim tables it wrote (analyzeClaims).
func (r *receita) Load(ctx context.Context, col collection.Collection, files []rawstore.File, raw rawstore.Store, pool *pgxpool.Pool) (_ connector.Stats, err error) {
	if col.SourceID != sourceID {
		return nil, fmt.Errorf("receita: collection %d belongs to source %d", col.ID, col.SourceID)
	}
	scope, err := scopeBases(col.Scope)
	if err != nil {
		return nil, err
	}
	inputs, err := classify(files)
	if err != nil {
		return nil, err
	}
	// The session lock keeps one connection for the whole Load; COPY and the
	// set-based statements need at least one more.
	if n := pool.Config().MaxConns; n < 2 {
		return nil, fmt.Errorf("receita: the pool allows %d connection(s), the load needs at least 2 (pool_max_conns)", n)
	}
	start := time.Now()

	lock, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("receita: acquire lock connection: %w", err)
	}
	defer lock.Release()
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, loadLockKey); err != nil {
		return nil, fmt.Errorf("receita: load lock: %w", err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if _, err := lock.Exec(uctx, `SELECT pg_advisory_unlock($1)`, loadLockKey); err != nil {
			// Closing the connection releases the lock; the pool drops closed connections.
			slog.Warn("receita: unlock failed, closing the connection", "error", err)
			_ = lock.Conn().Close(uctx)
		}
	}()

	// Nothing is written before this check.
	if err := checkEdition(ctx, pool, col); err != nil {
		return nil, err
	}
	if err := truncateStaging(ctx, pool); err != nil {
		return nil, err
	}
	// Staging holds partner data in UNLOGGED tables: empty it on failure too.
	defer func() {
		if err == nil {
			return
		}
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if terr := truncateStaging(tctx, pool); terr != nil {
			slog.Warn("receita: could not empty staging after a failed load", "error", terr)
		}
	}()
	stats := connector.Stats{"rows_read": 0, "rows_skipped": 0}
	for _, k := range kinds {
		stats[k.stat+"_rows"], stats[k.stat+"_skipped"] = 0, 0
	}
	month := editionMonth(col.ReferenceDate)
	for i := range inputs {
		inputs[i].col = col.ID
		inputs[i].url = r.fileURL(month, path.Base(inputs[i].file.Key))
	}
	if err := r.parseAll(ctx, pool, raw, inputs, stats); err != nil {
		return nil, err
	}
	slog.Info("receita: parse and copy done", "collection", col.ID, "files", len(inputs),
		"rows_read", stats["rows_read"], "rows_skipped", stats["rows_skipped"], "elapsed", since(start))

	if err := upsert(ctx, pool, col.ID, scope, col.Scope.Depth, stats); err != nil {
		return nil, err
	}
	if err := truncateStaging(ctx, pool); err != nil {
		return nil, err
	}
	slog.Info("receita: load done", "collection", col.ID, "elapsed", since(start))
	return stats, nil
}

// ErrOlderEdition: a collection with a later reference date already wrote
// (or may have written) claims. Loading an older edition after a newer one
// would close the newer company versions and mark old claims as the latest
// seen, so backfilling is refused. Re-collecting with the same or a later
// reference date is allowed. The rule compares exact reference dates, like
// claims.Store.CloseUnseen, so a load that passes it is never refused
// afterwards when closing unseen claims.
var ErrOlderEdition = errors.New("receita: a collection with a later reference date is already loaded")

// checkEdition refuses col when another collection of the source with a
// later reference date wrote claims: one that is done, or, whatever its
// status (failed, or running: killed mid-load), one that recorded a raw file
// (collection_file: the loader records every file before it writes any
// claim, so a collection without one wrote nothing). A failed newer load
// leaves partial newer data; writing an older edition over it would make
// spurious, permanent close/reopen pairs. The cure is to load that newer
// edition (or a later one) again.
func checkEdition(ctx context.Context, pool *pgxpool.Pool, col collection.Collection) error {
	var (
		laterID int64
		later   time.Time
		status  string
	)
	err := pool.QueryRow(ctx, `SELECT c.id, c.reference_date, c.status FROM collection c
		WHERE c.source_id = $1 AND c.id <> $3 AND c.reference_date > $2::date
		  AND (c.status = 'done' OR EXISTS (SELECT 1 FROM collection_file f WHERE f.collection_id = c.id))
		ORDER BY c.reference_date DESC, c.id DESC LIMIT 1`, sourceID, col.ReferenceDate, col.ID).Scan(&laterID, &later, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("receita: check edition: %w", err)
	}
	return fmt.Errorf("%w: collection %d has reference date %s, collection %d (%s) already wrote %s", ErrOlderEdition, col.ID,
		col.ReferenceDate.UTC().Format(time.DateOnly), laterID, status, later.Format(time.DateOnly))
}

// scopeBases returns the company bases a slice starts from; nil for a full load.
func scopeBases(s collection.Scope) ([]string, error) {
	if s.Full {
		return nil, nil
	}
	if len(s.CNPJs) == 0 {
		return nil, errors.New("receita: scope is neither full nor lists CNPJs")
	}
	if s.Depth < 0 {
		return nil, fmt.Errorf("receita: negative scope depth %d", s.Depth)
	}
	out := make([]string, 0, len(s.CNPJs))
	for _, c := range s.CNPJs {
		b, ok := identity.CNPJBase(c)
		if !ok {
			return nil, fmt.Errorf("receita: invalid CNPJ %q in scope", c)
		}
		out = append(out, b)
	}
	return out, nil
}

// classify picks the data files and numbers them; the largest go first so
// the big part 0 files do not end up alone at the end.
func classify(files []rawstore.File) ([]input, error) {
	var out []input
	seen := map[string]bool{}
	for _, f := range files {
		m := dataFile.FindStringSubmatch(path.Base(f.Key))
		if m == nil {
			continue
		}
		out = append(out, input{file: f, kind: kinds[m[1]]})
		seen[m[1]] = true
	}
	for name := range kinds {
		if !seen[name] {
			return nil, fmt.Errorf("receita: no %s file to load", name)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file.Size != out[j].file.Size {
			return out[i].file.Size > out[j].file.Size
		}
		return out[i].file.Key < out[j].file.Key
	})
	if len(out) > 1<<15-1 {
		return nil, fmt.Errorf("receita: too many files (%d)", len(out))
	}
	for i := range out {
		out[i].no = int16(i + 1)
	}
	return out, nil
}

func truncateStaging(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `TRUNCATE receita_stg_file, receita_stg_empresa, receita_stg_estab, receita_stg_socio,
		receita_stg_scope, receita_stg_company_src, receita_stg_company, receita_stg_person,
		receita_stg_person_new, receita_stg_partner, receita_stg_rel`)
	if err != nil {
		return fmt.Errorf("receita: truncate staging: %w", err)
	}
	return nil
}

// parseAll loads every input with up to r.workers files at once, each
// worker copying over its own connection. The first error cancels the rest.
func (r *receita) parseAll(ctx context.Context, pool *pgxpool.Pool, raw rawstore.Store, inputs []input, stats connector.Stats) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan input)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	workers := max(1, min(r.workers, len(inputs)))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for in := range jobs {
				c, err := r.loadFile(ctx, pool, raw, in)
				if err != nil {
					cancel(fmt.Errorf("receita: %s: %w", path.Base(in.file.Key), err))
					continue
				}
				mu.Lock()
				for k, v := range c {
					stats[k] += v
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, in := range inputs {
		select {
		case jobs <- in:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return nil
}

// loadFile spools one zip, checks it and copies its rows into staging.
func (r *receita) loadFile(ctx context.Context, pool *pgxpool.Pool, raw rawstore.Store, in input) (connector.Stats, error) {
	start := time.Now()
	spool, err := os.CreateTemp(r.spool(), "receita-load-*.zip")
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()
	src, err := raw.Open(ctx, in.file.Key)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(spool, h), src)
	src.Close()
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	// The checksum is always computed here, so the file is recorded with
	// one even when the raw store gave none.
	sum := hex.EncodeToString(h.Sum(nil))
	if in.file.SHA256 == "" {
		slog.Warn("receita: raw store gave no checksum, file not verified", "key", in.file.Key, "sha256", sum)
	} else if sum != in.file.SHA256 {
		return nil, fmt.Errorf("checksum %s, raw store recorded %s", sum, in.file.SHA256)
	}
	zr, err := zip.NewReader(spool, size)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	var entries []*zip.File
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() {
			entries = append(entries, f)
		}
	}
	if len(entries) != 1 {
		return nil, fmt.Errorf("zip has %d files, want exactly 1", len(entries))
	}
	entry := entries[0]
	name := path.Base(entry.Name)
	if _, err := pool.Exec(ctx, `INSERT INTO receita_stg_file (file_no, name) VALUES ($1, $2)`, in.no, name); err != nil {
		return nil, fmt.Errorf("register file: %w", err)
	}
	// name is the file part of record_ref (name || ':' || line), so a claim's
	// provenance finds the preserved zip and its checksum through this row.
	if err := collection.RecordSourceFile(ctx, pool, in.col, collection.File{Name: name, RawKey: in.file.Key, SHA256: sum,
		Size: size, SourceURL: in.url}); err != nil {
		return nil, err
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer rc.Close()

	cs := in.kind.newSource(newRecordReader(rc, in.kind.cols), in.no, name)
	copied, err := pool.CopyFrom(ctx, pgx.Identifier{in.kind.table}, in.kind.columns, cs)
	if err != nil {
		return nil, fmt.Errorf("copy %s: %w", name, err)
	}
	c := cs.counts()
	out := connector.Stats{
		"rows_read": c.read, "rows_skipped": c.skipped,
		in.kind.stat + "_rows": c.read, in.kind.stat + "_skipped": c.skipped,
	}
	for k, v := range c.extra {
		out[k] = v
	}
	slog.Info("receita: file copied", "file", name, "key", in.file.Key, "rows_read", c.read,
		"rows_skipped", c.skipped, "rows_copied", copied, "elapsed", since(start))
	return out, nil
}

// counts are the counters of one file.
type counts struct {
	read, skipped int64
	extra         map[string]int64
}

// copySource is a pgx.CopyFromSource over one CSV that also counts rows.
type copySource interface {
	pgx.CopyFromSource
	counts() counts
}

// rowSource drives the record reader; parse turns a record into the
// staging values, or reports it as skipped.
type rowSource struct {
	rr       *recordReader
	fileName string
	fileNo   int16
	values   []any
	err      error
	c        counts
	logged   int
	// parse fills values for rec and returns false when the row is skipped
	// (reason, for the log) or not loaded (reason "").
	parse func(rec *record) (ok bool, reason string)
}

func (s *rowSource) Next() bool {
	for {
		rec, bad, err := s.rr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			s.err = err
			return false
		}
		s.c.read++
		if bad != nil {
			s.skip(rec.Line, bad.Error())
			continue
		}
		ok, reason := s.parse(rec)
		if ok {
			return true
		}
		if reason != "" {
			s.skip(rec.Line, reason)
		}
	}
}

func (s *rowSource) skip(line int64, reason string) {
	s.c.skipped++
	if s.logged < maxLoggedMalformed {
		s.logged++
		// Never the row content: it holds personal data.
		slog.Warn("receita: skipping malformed row", "file", s.fileName, "line", line, "reason", reason)
	}
}

func (s *rowSource) Values() ([]any, error) { return s.values, nil }
func (s *rowSource) Err() error             { return s.err }
func (s *rowSource) counts() counts         { return s.c }

func (s *rowSource) add(key string) {
	if s.c.extra == nil {
		s.c.extra = map[string]int64{}
	}
	s.c.extra[key]++
}

func newEmpresaSource(rr *recordReader, fileNo int16, fileName string) copySource {
	s := &rowSource{rr: rr, fileNo: fileNo, fileName: fileName, values: make([]any, 8)}
	s.parse = func(rec *record) (bool, string) {
		row, ok := parseEmpresa(rec)
		if !ok {
			return false, "invalid cnpj_basico"
		}
		if row.CapitalOutOfRange {
			s.add("empresas_capital_out_of_range")
		}
		var capital any
		if row.Capital != nil {
			capital = *row.Capital
		}
		v := s.values
		v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7] = row.Base, row.Name, row.NameNorm, row.LegalNature, row.Size, capital, fileNo, rec.Line
		return true, ""
	}
	return s
}

func newEstabSource(rr *recordReader, fileNo int16, fileName string) copySource {
	s := &rowSource{rr: rr, fileNo: fileNo, fileName: fileName, values: make([]any, 7)}
	s.parse = func(rec *record) (bool, string) {
		row, ok := parseEstab(rec)
		if !ok {
			return false, "invalid cnpj_basico"
		}
		if !row.HeadOffice {
			s.add("estabelecimentos_branches")
			return false, ""
		}
		v := s.values
		v[0], v[1], v[2], v[3], v[4], v[5], v[6] = row.Base, row.Status, row.UF, row.City, row.CNAE, fileNo, rec.Line
		return true, ""
	}
	return s
}

func newSocioSource(rr *recordReader, fileNo int16, fileName string) copySource {
	s := &rowSource{rr: rr, fileNo: fileNo, fileName: fileName, values: make([]any, 16)}
	s.parse = func(rec *record) (bool, string) {
		row, outcome := parseSocio(rec)
		switch outcome {
		case socioNoCompany:
			return false, "invalid cnpj_basico"
		case socioUnidentified:
			s.add("socios_unidentified_partner")
			return false, "partner cannot be identified"
		}
		var validFrom any
		if row.ValidFrom != nil {
			validFrom = *row.ValidFrom
		}
		v := s.values
		v[0], v[1], v[2], v[3] = row.Base, row.Type, row.Name, row.NameNorm
		v[4], v[5], v[6] = nullable(row.PartnerBase), row.CPFFragment, nullable(row.ForeignKey)
		v[7], v[8], v[9] = row.Qual, validFrom, row.HasRep
		v[10], v[11], v[12], v[13] = row.RepName, row.RepNameNorm, row.RepFragment, row.RepQual
		v[14], v[15] = fileNo, rec.Line
		return true, ""
	}
	return s
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func since(t time.Time) string { return time.Since(t).Round(time.Millisecond).String() }
