package cvm

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
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/rawstore"
)

// loadLockKey is the session advisory lock held for a whole Load, so two CVM
// loads never interleave their per-company writes.
const loadLockKey int64 = 0x63766d3030303031 // "cvm00001"

// ErrScope: the CVM connector only collects everything (Scope.Full); its
// claims are a snapshot, so a partial load would make no sense.
var ErrScope = errors.New("cvm: only a full scope is supported")

// errTooLarge: the shareholding entry decompresses to more than the limit
// (zip bomb guard).
var errTooLarge = errors.New("uncompressed size over the limit")

// zipName matches the raw files Load reads.
var zipName = regexp.MustCompile(`^fre_cia_aberta_([0-9]{4})\.zip$`)

// input is one raw zip to load.
type input struct {
	file rawstore.File
	year int
}

// statKeys are always present in the stats, so a zero is visible.
var statKeys = []string{
	"rows_read", "rows_skipped", "malformed_line", "malformed_columns", "malformed_cnpj", "malformed_date",
	"malformed_version", "malformed_document", "malformed_shareholder_id", "malformed_percent", "latest_form_lines_skipped",
	"superseded_rows", "aggregate_outros", "aggregate_tesouraria", "aggregate_total",
	"unidentified_shareholder", "self_holding", "duplicate_shareholder", "duplicate_conflict", "duplicate_shareholder_id",
	"indirect_parent_missing", "indirect_parent_not_company", "indirect_too_deep", "indirect_cycle",
	"indirect_duplicate_description", "indirect_duplicate_conflict", "indirect_superseded_by_own_form", "indirect_superseded", "indirect_conflict",
	"forms_loaded", "companies_from_previous_year", "holdings_described", "shareholders_loaded", "indirect_shareholders_loaded",
	"relationships_inserted", "relationships_seen", "relationships_replaced", "year_not_published",
}

// Load reads the FRE zips among files (one per year; other files are
// ignored) and writes the claims of the collection (docs/sources/cvm.md):
//
//  1. take the load lock;
//  2. per zip, in year order: spool it from raw, check its SHA-256, record
//     its shareholding entry in collection_file and parse that entry,
//     without extracting, reading columns by header name; CPFs are reduced
//     to fragments in the parser; malformed lines are counted and skipped;
//  3. keep, for every listed company, only the lines of its latest form
//     (latest reference date, then highest version) across the years;
//  4. per company, in CNPJ order: upsert the listed company, its direct
//     shareholders (companies, foreign holders, person records) and, in one
//     short transaction, their shareholder_of and controls claims;
//  5. per holding described by indirect lines, from the one form chosen to
//     describe it (describeHoldings): the same, into the holding.
//
// A claim whose percentage changed closes the open row and opens a new one;
// claims no longer declared are left to CloseUnseen.
func (c *cvm) Load(ctx context.Context, col collection.Collection, files []rawstore.File, raw rawstore.Store, pool *pgxpool.Pool) (_ connector.Stats, err error) {
	if col.SourceID != sourceID {
		return nil, fmt.Errorf("cvm: collection %d belongs to source %d", col.ID, col.SourceID)
	}
	if !col.Scope.Full || len(col.Scope.CNPJs) > 0 {
		return nil, ErrScope
	}
	inputs, err := classify(files)
	if err != nil {
		return nil, err
	}
	missing, err := c.checkYears(ctx, pool, col, inputs)
	if err != nil {
		return nil, err
	}
	if n := pool.Config().MaxConns; n < 2 {
		return nil, fmt.Errorf("cvm: the pool allows %d connection(s), the load needs at least 2 (pool_max_conns)", n)
	}
	start := time.Now()

	lock, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("cvm: acquire lock connection: %w", err)
	}
	defer lock.Release()
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, loadLockKey); err != nil {
		return nil, fmt.Errorf("cvm: load lock: %w", err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if _, err := lock.Exec(uctx, `SELECT pg_advisory_unlock($1)`, loadLockKey); err != nil {
			slog.Warn("cvm: unlock failed, closing the connection", "error", err)
			_ = lock.Conn().Close(uctx)
		}
	}()

	stats := connector.Stats{}
	for _, k := range statKeys {
		stats[k] = 0
	}
	if missing != 0 {
		stats["year_not_published"] = int64(missing)
		slog.Warn("cvm: the reference year's FRE is not published yet; loading the previous year only", "year", missing)
	}
	var rows []holderRow
	for _, in := range inputs {
		r, err := c.readZip(ctx, pool, col.ID, raw, in, stats)
		if err != nil {
			return nil, fmt.Errorf("cvm: %s: %w", path.Base(in.file.Key), err)
		}
		rows = append(rows, r...)
	}
	forms := latestForms(rows, stats)
	slog.Info("cvm: parse done", "collection", col.ID, "zips", len(inputs), "companies", len(forms),
		"rows_read", stats["rows_read"], "rows_skipped", stats["rows_skipped"], "elapsed", since(start))
	if err := write(ctx, pool, col, forms, stats); err != nil {
		return nil, err
	}
	slog.Info("cvm: load done", "collection", col.ID, "forms", stats["forms_loaded"],
		"inserted", stats["relationships_inserted"], "seen", stats["relationships_seen"],
		"replaced", stats["relationships_replaced"], "elapsed", since(start))
	return stats, nil
}

// classify picks the FRE zips, in year order.
func classify(files []rawstore.File) ([]input, error) {
	var out []input
	seen := map[int]bool{}
	for _, f := range files {
		m := zipName.FindStringSubmatch(path.Base(f.Key))
		if m == nil {
			continue
		}
		year, _ := strconv.Atoi(m[1])
		if seen[year] {
			return nil, fmt.Errorf("cvm: two FRE files for %d", year)
		}
		seen[year] = true
		out = append(out, input{file: f, year: year})
	}
	if len(out) == 0 {
		return nil, errors.New("cvm: no file to load")
	}
	slices.SortFunc(out, func(a, b input) int { return a.year - b.year })
	return out, nil
}

// checkYears checks that inputs hold every collected year. Only the
// reference year's zip may be absent (Fetch got a 404: not published yet,
// as in early January); it returns that year. But if an earlier done CVM
// collection already recorded that year's shareholding entry, the absence
// is a publication glitch, not a new year: loading would revert every
// company to its previous-year form and close claims irreversibly, so it
// fails.
func (c *cvm) checkYears(ctx context.Context, pool *pgxpool.Pool, col collection.Collection, inputs []input) (int, error) {
	have := map[int]bool{}
	for _, in := range inputs {
		have[in.year] = true
	}
	years := c.years(col.ReferenceDate)
	refYear := years[len(years)-1]
	for _, y := range years[:len(years)-1] {
		if !have[y] {
			return 0, fmt.Errorf("cvm: no FRE file for %d", y)
		}
	}
	if have[refYear] {
		return 0, nil
	}
	var seen bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM collection_file f JOIN collection c ON c.id = f.collection_id
		WHERE c.source_id = $1 AND c.status = 'done' AND c.id <> $2 AND f.name = $3)`, sourceID, col.ID, entryName(refYear)).Scan(&seen)
	if err != nil {
		return 0, fmt.Errorf("cvm: check collected years: %w", err)
	}
	if seen {
		return 0, fmt.Errorf("cvm: the FRE of %d is missing but an earlier collection loaded it; refusing to fall back to %d", refYear, refYear-1)
	}
	return refYear, nil
}

// readZip spools one zip, checks it, records its shareholding entry and
// parses it.
func (c *cvm) readZip(ctx context.Context, pool *pgxpool.Pool, colID int64, raw rawstore.Store, in input, stats connector.Stats) ([]holderRow, error) {
	spool, err := os.CreateTemp(c.spool(), "cvm-load-*.zip")
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
	size, err := io.Copy(io.MultiWriter(spool, h), io.LimitReader(src, c.maxDownloadBytes+1))
	src.Close()
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	if size > c.maxDownloadBytes {
		return nil, fmt.Errorf("over the %d-byte limit", c.maxDownloadBytes)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if in.file.SHA256 == "" {
		slog.Warn("cvm: raw store gave no checksum, file not verified", "key", in.file.Key, "sha256", sum)
	} else if sum != in.file.SHA256 {
		return nil, fmt.Errorf("checksum %s, raw store recorded %s", sum, in.file.SHA256)
	}
	zr, err := zip.NewReader(spool, size)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	name := entryName(in.year)
	var entry *zip.File
	for _, f := range zr.File {
		if f.Name != name {
			continue // every other dataset of the FRE is ignored
		}
		if entry != nil {
			return nil, fmt.Errorf("entry %s appears twice", name)
		}
		entry = f
	}
	if entry == nil {
		return nil, fmt.Errorf("no %s entry", name)
	}
	if entry.UncompressedSize64 > uint64(c.maxEntryBytes) {
		return nil, fmt.Errorf("%s: declares %d bytes: %w (%d)", name, entry.UncompressedSize64, errTooLarge, c.maxEntryBytes)
	}
	// name is the file part of record_ref (name || ':' || line): a claim's
	// provenance finds the preserved zip and its checksum through this row.
	if err := collection.RecordSourceFile(ctx, pool, colID, collection.File{Name: name, RawKey: in.file.Key, SHA256: sum,
		Size: size, SourceURL: c.fileURL(remoteName(in.year))}); err != nil {
		return nil, err
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("%s: open: %w", name, err)
	}
	defer rc.Close()
	rows, malformed, err := parseEntry(&capReader{r: rc, limit: c.maxEntryBytes}, name, in.year)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var bad int64
	for k, v := range malformed {
		stats[k] += v
		bad += v
	}
	stats["rows_read"] += int64(len(rows)) + bad
	stats["rows_skipped"] += bad
	slog.Info("cvm: file parsed", "file", name, "key", in.file.Key, "rows_read", int64(len(rows))+bad, "rows_skipped", bad)
	return rows, nil
}

// capReader fails with errTooLarge once more than limit bytes were read:
// declared sizes are not trusted.
type capReader struct {
	r     io.Reader
	limit int64
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.limit -= int64(n)
	if c.limit < 0 {
		return n, errTooLarge
	}
	return n, err
}

// latestForms keeps, for every listed company, the lines of its latest form
// across all years read (formKey.newer); lines of older forms and versions
// are counted as superseded_rows. Lines keep their read order (year, line).
func latestForms(rows []holderRow, stats connector.Stats) map[string][]holderRow {
	best := map[string]formKey{}
	for _, r := range rows {
		if b, ok := best[r.Base]; !ok || r.Form.newer(b) {
			best[r.Base] = r.Form
		}
	}
	out := make(map[string][]holderRow, len(best))
	for _, r := range rows {
		if r.Form != best[r.Base] {
			stats["superseded_rows"]++
			continue
		}
		out[r.Base] = append(out[r.Base], r)
	}
	return out
}

func since(t time.Time) string { return time.Since(t).Round(time.Millisecond).String() }
