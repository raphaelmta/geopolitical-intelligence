package tse

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
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/rawstore"
)

// loadLockKey is the session advisory lock held for a whole Load: the
// staging tables are shared, so two TSE loads never run at once.
const loadLockKey int64 = 0x7473653030303031 // "tse00001"

// maxLoggedMalformed bounds the malformed-row warnings logged per entry.
const maxLoggedMalformed = 5

// entriesPerZip bounds the loaded entries of one zip; file_no is
// zip index * entriesPerZip + entry index + 1, so it fits a smallint.
const entriesPerZip = 256

// stagingTables are truncated before and after every Load (also on failure).
var stagingTables = []string{"tse_stg_file", "tse_stg_cand", "tse_stg_receipt", "tse_stg_candidate", "tse_stg_person",
	"tse_stg_person_new", "tse_stg_donation", "tse_stg_claim", "tse_stg_rel"}

// ErrScope: the TSE connector only collects whole years (Scope.Full); the
// request protocol cannot express a narrower TSE scope.
var ErrScope = errors.New("tse: only a full scope is supported")

// errTooLarge: a zip entry, or all loaded entries of a zip, decompress to
// more than the limit (zip bomb guard).
var errTooLarge = errors.New("uncompressed size over the limit")

// zipName matches the raw files Load reads.
var zipName = regexp.MustCompile(`^(consulta_cand|prestacao_de_contas_eleitorais_candidatos)_([0-9]{4})\.zip$`)

// input is one raw zip to load.
type input struct {
	file rawstore.File
	kind fileKind
	year int
	no   int // position in name order
	col  int64
}

// Load reads the candidacies and campaign-finance zips among files (one of
// each per year; other files are ignored) and writes the claims of the
// collection (docs/sources/tse.md):
//
//  1. take the load lock and truncate the staging tables (migration 0013);
//     they are emptied again at the end, also when the load fails;
//  2. per zip, with bounded parallelism: spool it from raw, check its
//     SHA-256, and for every CSV entry that is loaded (one per UF and BR,
//     never the _BRASIL aggregate) record it in collection_file and stream
//     it, without extracting, through the record reader, reading columns by
//     header name; CPFs are reduced to fragments in the parser; valid rows
//     are copied into staging; malformed rows are counted and skipped, and
//     rows not loaded by design are counted by reason;
//  3. set-based SQL (upsert.go) writes parties, candidate and donor person
//     records, donor companies and the candidate_for and donated_to
//     relationships, these in short per-bucket transactions under the
//     identity lock.
func (t *tse) Load(ctx context.Context, col collection.Collection, files []rawstore.File, raw rawstore.Store, pool *pgxpool.Pool) (_ connector.Stats, err error) {
	if col.SourceID != sourceID {
		return nil, fmt.Errorf("tse: collection %d belongs to source %d", col.ID, col.SourceID)
	}
	if !col.Scope.Full || len(col.Scope.CNPJs) > 0 {
		return nil, ErrScope
	}
	inputs, err := classify(files)
	if err != nil {
		return nil, err
	}
	if n := pool.Config().MaxConns; n < 2 {
		return nil, fmt.Errorf("tse: the pool allows %d connection(s), the load needs at least 2 (pool_max_conns)", n)
	}
	start := time.Now()

	lock, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("tse: acquire lock connection: %w", err)
	}
	defer lock.Release()
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, loadLockKey); err != nil {
		return nil, fmt.Errorf("tse: load lock: %w", err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if _, err := lock.Exec(uctx, `SELECT pg_advisory_unlock($1)`, loadLockKey); err != nil {
			slog.Warn("tse: unlock failed, closing the connection", "error", err)
			_ = lock.Conn().Close(uctx)
		}
	}()

	if err := truncateStaging(ctx, pool); err != nil {
		return nil, err
	}
	// Staging holds personal data (names, CPF fragments): empty it on failure too.
	defer func() {
		if err == nil {
			return
		}
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if terr := truncateStaging(tctx, pool); terr != nil {
			slog.Warn("tse: could not empty staging after a failed load", "error", terr)
		}
	}()
	stats := connector.Stats{
		"rows_read": 0, "rows_skipped": 0, "candidatos_rows": 0, "candidatos_skipped": 0, "receitas_rows": 0, "receitas_skipped": 0,
		"own_resources": 0, "crowdfunding": 0, "other_origin": 0, "no_receipt_id": 0, "party_unknown": 0,
		"unidentified_donor": 0, "candidate_unknown": 0,
		"donor_candidate_unpublished": 0, "donor_candidate_self": 0, "donor_candidate_not_in_file": 0,
		"relationships_asserted": 0, "relationships_seen": 0, "relationships_inserted": 0,
		"receipt_mixed_donor": 0, "receipt_conflict": 0, "party_number_mapped": 0,
	}
	for i := range inputs {
		inputs[i].col = col.ID
	}
	if err := t.parseAll(ctx, pool, raw, inputs, stats); err != nil {
		return nil, err
	}
	slog.Info("tse: parse and copy done", "collection", col.ID, "zips", len(inputs),
		"rows_read", stats["rows_read"], "rows_skipped", stats["rows_skipped"], "elapsed", since(start))

	if err := upsert(ctx, pool, col.ID, stats); err != nil {
		return nil, err
	}
	if stagingHook != nil {
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			stagingHook(ctx, tx, "after upsert")
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if err := truncateStaging(ctx, pool); err != nil {
		return nil, err
	}
	slog.Info("tse: load done", "collection", col.ID, "elapsed", since(start))
	return stats, nil
}

// classify picks the zips, in name order (candidacies of every year sort
// before the receipts, so first-seen names come from the candidacies), and
// checks that every year has both.
func classify(files []rawstore.File) ([]input, error) {
	var out []input
	have := map[int][2]bool{}
	for _, f := range files {
		m := zipName.FindStringSubmatch(path.Base(f.Key))
		if m == nil {
			continue
		}
		year, _ := strconv.Atoi(m[2])
		k := kindCand
		if m[1] != "consulta_cand" {
			k = kindReceipt
		}
		h := have[year]
		if h[k] {
			return nil, fmt.Errorf("tse: two %s files for %d", m[1], year)
		}
		h[k] = true
		have[year] = h
		out = append(out, input{file: f, kind: k, year: year})
	}
	if len(out) == 0 {
		return nil, errors.New("tse: no file to load")
	}
	for year, h := range have {
		if !h[kindCand] || !h[kindReceipt] {
			return nil, fmt.Errorf("tse: %d needs both the candidacies and the campaign-finance zip", year)
		}
	}
	sort.Slice(out, func(i, j int) bool { return path.Base(out[i].file.Key) < path.Base(out[j].file.Key) })
	if len(out)*entriesPerZip > 1<<15-1 {
		return nil, fmt.Errorf("tse: too many files (%d)", len(out))
	}
	for i := range out {
		out[i].no = i
	}
	return out, nil
}

func truncateStaging(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `TRUNCATE tse_stg_file, tse_stg_cand, tse_stg_receipt, tse_stg_candidate, tse_stg_person,
		tse_stg_person_new, tse_stg_donation, tse_stg_claim, tse_stg_rel`)
	if err != nil {
		return fmt.Errorf("tse: truncate staging: %w", err)
	}
	return nil
}

// parseAll loads every zip with up to t.workers at once. The first error
// cancels the rest.
func (t *tse) parseAll(ctx context.Context, pool *pgxpool.Pool, raw rawstore.Store, inputs []input, stats connector.Stats) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan input)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	workers := max(1, min(t.workers, len(inputs)))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for in := range jobs {
				c, err := t.loadZip(ctx, pool, raw, in)
				if err != nil {
					cancel(fmt.Errorf("tse: %s: %w", path.Base(in.file.Key), err))
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
	return context.Cause(ctx)
}

// loadZip spools one zip, checks it and copies the rows of its loaded
// entries into staging.
func (t *tse) loadZip(ctx context.Context, pool *pgxpool.Pool, raw rawstore.Store, in input) (connector.Stats, error) {
	start := time.Now()
	spool, err := os.CreateTemp(t.spool(), "tse-load-*.zip")
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
	sum := hex.EncodeToString(h.Sum(nil))
	if in.file.SHA256 == "" {
		slog.Warn("tse: raw store gave no checksum, file not verified", "key", in.file.Key, "sha256", sum)
	} else if sum != in.file.SHA256 {
		return nil, fmt.Errorf("checksum %s, raw store recorded %s", sum, in.file.SHA256)
	}
	zr, err := zip.NewReader(spool, size)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	entries, err := t.selectEntries(zr, in)
	if err != nil {
		return nil, err
	}
	total := &budget{left: t.maxTotalBytes}
	out := connector.Stats{}
	for i, entry := range entries {
		no := int16(in.no*entriesPerZip + i + 1)
		c, err := t.loadEntry(ctx, pool, in, entry, no, sum, size, total)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name, err)
		}
		for k, v := range c {
			out[k] += v
		}
	}
	slog.Info("tse: zip copied", "key", in.file.Key, "entries", len(entries), "elapsed", since(start))
	return out, nil
}

// selectEntries returns the entries of the zip that are loaded, in name
// order, after checking their declared sizes against the limits. Other
// entries (the _BRASIL aggregate, PDFs, other datasets) are ignored.
func (t *tse) selectEntries(zr *zip.Reader, in input) ([]*zip.File, error) {
	re := entryPattern(in.kind, in.year)
	var out []*zip.File
	seen := map[string]bool{}
	var declared uint64
	for _, f := range zr.File {
		if !re.MatchString(f.Name) {
			continue
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("entry %s appears twice", f.Name)
		}
		seen[f.Name] = true
		if f.UncompressedSize64 > uint64(t.maxEntryBytes) {
			return nil, fmt.Errorf("%s: declares %d bytes: %w (%d)", f.Name, f.UncompressedSize64, errTooLarge, t.maxEntryBytes)
		}
		declared += f.UncompressedSize64
		out = append(out, f)
	}
	if declared > uint64(t.maxTotalBytes) {
		return nil, fmt.Errorf("entries declare %d bytes: %w (%d)", declared, errTooLarge, t.maxTotalBytes)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no %s entry for %d", re.String(), in.year)
	}
	if len(out) >= entriesPerZip {
		return nil, fmt.Errorf("too many entries (%d)", len(out))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// budget is the uncompressed byte allowance left for one zip.
type budget struct{ left int64 }

// capReader fails with errTooLarge once more than limit bytes (or more than
// the zip's remaining budget) were read: declared sizes are not trusted.
type capReader struct {
	r     io.Reader
	limit int64
	total *budget
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.limit -= int64(n)
	c.total.left -= int64(n)
	if c.limit < 0 || c.total.left < 0 {
		return n, errTooLarge
	}
	return n, err
}

// loadEntry records one CSV entry and copies its rows into staging.
func (t *tse) loadEntry(ctx context.Context, pool *pgxpool.Pool, in input, entry *zip.File, no int16, sum string, size int64, total *budget) (connector.Stats, error) {
	name := entry.Name
	if _, err := pool.Exec(ctx, `INSERT INTO tse_stg_file (file_no, name) VALUES ($1, $2)`, no, name); err != nil {
		return nil, fmt.Errorf("register file: %w", err)
	}
	// name is the file part of record_ref (name || ':' || line): a claim's
	// provenance finds the preserved zip and its checksum through this row.
	if err := collection.RecordSourceFile(ctx, pool, in.col, collection.File{Name: name, RawKey: in.file.Key, SHA256: sum,
		Size: size, SourceURL: t.fileURL(path.Base(in.file.Key))}); err != nil {
		return nil, err
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer rc.Close()
	rr := newRecordReader(&capReader{r: rc, limit: t.maxEntryBytes, total: total}, 0)
	header, bad, err := rr.Next()
	if errors.Is(err, io.EOF) {
		return nil, errors.New("empty file (no header)")
	}
	if err != nil {
		return nil, err
	}
	if bad != nil {
		return nil, fmt.Errorf("header: %w", bad)
	}
	names := make([]string, header.NumFields())
	for i := range names {
		names[i] = header.Field(i)
	}
	layout, table, columns, stat := candLayout(in.year), "tse_stg_cand", candColumns, "candidatos"
	if in.kind == kindReceipt {
		layout, table, columns, stat = receiptLayout(in.year), "tse_stg_receipt", receiptColumns, "receitas"
	}
	idx, err := columnIndexes(names, layout)
	if err != nil {
		return nil, err
	}
	rr.cols = len(names)
	s := &rowSource{rr: rr, fileName: name, idx: idx, year: int16(in.year), fileNo: no, kind: in.kind, values: make([]any, len(columns))}
	copied, err := pool.CopyFrom(ctx, pgx.Identifier{table}, columns, s)
	if err != nil {
		return nil, fmt.Errorf("copy: %w", err)
	}
	out := connector.Stats{"rows_read": s.read, "rows_skipped": s.skipped, stat + "_rows": s.read, stat + "_skipped": s.skipped}
	for k, v := range s.notLoaded {
		out[k] += v
	}
	slog.Info("tse: file copied", "file", name, "key", in.file.Key, "rows_read", s.read, "rows_skipped", s.skipped, "rows_copied", copied)
	return out, nil
}

// Staging columns, in the order rowSource fills them.
var (
	candColumns = []string{"year", "sq", "turno", "election_date", "office_code", "office", "uf", "city", "city_name",
		"name", "name_norm", "ballot_name", "cpf_fragment", "party", "party_name", "party_name_norm", "result", "party_number", "file_no", "line"}
	receiptColumns = []string{"year", "receipt_id", "sq", "origin_code", "origin", "donor_kind", "donor_key", "donor_name",
		"donor_name_norm", "donor_fragment", "donor_base", "donor_party", "donor_party_name", "donor_party_name_norm",
		"donor_sq", "amount", "receipt_date", "donor_party_number", "file_no", "line"}
)

// rowSource is a pgx.CopyFromSource over one CSV entry that counts rows.
type rowSource struct {
	rr        *recordReader
	fileName  string
	idx       []int
	year      int16
	fileNo    int16
	kind      fileKind
	values    []any
	err       error
	read      int64
	skipped   int64
	notLoaded map[string]int64
	logged    int
	// unpublished: receipts (candidacy|receipt id) already counted as
	// donor_candidate_unpublished, so a multi-line receipt counts once.
	unpublished map[string]bool
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
		s.read++
		if bad != nil {
			s.skip(rec.Line, bad.Error())
			continue
		}
		f := fields{rec: rec, idx: s.idx}
		if s.kind == kindCand {
			row, reason := parseCand(f)
			if reason != "" {
				s.skip(rec.Line, reason)
				continue
			}
			s.candValues(row, rec.Line)
			return true
		}
		row, reason, notLoaded := parseReceipt(f)
		switch {
		case reason != "":
			s.skip(rec.Line, reason)
			continue
		case notLoaded != "":
			if s.notLoaded == nil {
				s.notLoaded = map[string]int64{}
			}
			if notLoaded == "donor_candidate_unpublished" {
				if s.unpublished == nil {
					s.unpublished = map[string]bool{}
				}
				k := row.SQ + "|" + row.ReceiptID
				if s.unpublished[k] {
					continue
				}
				s.unpublished[k] = true
			}
			s.notLoaded[notLoaded]++
			continue
		}
		s.receiptValues(row, rec.Line)
		return true
	}
}

func (s *rowSource) candValues(r candRow, line int64) {
	v := s.values
	v[0], v[1], v[2], v[3] = s.year, r.SQ, r.Turno, date(r.Date)
	v[4], v[5], v[6], v[7], v[8] = r.OfficeCode, r.Office, r.UF, r.City, r.CityName
	v[9], v[10], v[11], v[12] = r.Name, r.NameNorm, r.Ballot, r.Fragment
	v[13], v[14], v[15], v[16], v[17] = r.Party, r.PartyName, r.PartyNameNorm, r.Result, r.PartyNumber
	v[18], v[19] = s.fileNo, line
}

func (s *rowSource) receiptValues(r receiptRow, line int64) {
	v := s.values
	v[0], v[1], v[2], v[3], v[4] = s.year, r.ReceiptID, r.SQ, r.OriginCode, r.Origin
	v[5], v[6], v[7], v[8], v[9] = r.Kind, r.Key, r.DonorName, r.DonorNameNorm, r.Fragment
	v[10], v[11], v[12], v[13], v[14] = r.Base, r.Party, r.PartyName, r.PartyNameNorm, r.DonorSQ
	v[15], v[16], v[17], v[18], v[19] = r.Amount, date(r.Date), r.PartyNumber, s.fileNo, line
}

func (s *rowSource) skip(line int64, reason string) {
	s.skipped++
	if s.logged < maxLoggedMalformed {
		s.logged++
		// Never the row content: it holds personal data.
		slog.Warn("tse: skipping malformed row", "file", s.fileName, "line", line, "reason", reason)
	}
}

func (s *rowSource) Values() ([]any, error) { return s.values, nil }
func (s *rowSource) Err() error             { return s.err }

func date(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func since(t time.Time) string { return time.Since(t).Round(time.Millisecond).String() }
