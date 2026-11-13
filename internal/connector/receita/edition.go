package receita

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/connector"
)

// monthFolder is the allow-listed name of an edition folder.
var monthFolder = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)

// maxEditionChecks bounds how many month folders ResolveReference lists
// (newest first) looking for a complete edition.
const maxEditionChecks = 3

// dataKinds are the file groups the loader reads. The loader reads no
// domain table (codes are stored as published), so none is required.
var dataKinds = []string{"Empresas", "Estabelecimentos", "Socios"}

var _ connector.EditionResolver = (*receita)(nil)

// ResolveReference returns the newest edition published on or before
// requested: its reference date is the first day (UTC) of the greatest month
// folder YYYY-MM not after requested's month whose listing is complete (see
// complete); its observed date is the edition's publication date, the UTC
// date of the newest Last-Modified among its data parts (zero when no part
// has a parseable date: the collection's start day is used then). A month
// that fails the rules (still being uploaded) is skipped in favour of the
// previous one; at most maxEditionChecks months are listed. No complete
// edition gives connector.ErrNoEdition. The data parts of the chosen month
// are remembered for Fetch.
//
// A requested date names the newest edition to accept, which is how an
// edition is pinned: a request for 2026-10-31 loads edition 2026-10 even
// when 2026-11 is already published.
func (r *receita) ResolveReference(ctx context.Context, requested time.Time, pool *pgxpool.Pool) (connector.Edition, error) {
	required, from, err := r.requiredParts(ctx, pool)
	if err != nil {
		return connector.Edition{}, err
	}
	base, user, err := r.folderURL("")
	if err != nil {
		return connector.Edition{}, err
	}
	entries, err := r.propfind(ctx, base, user)
	if err != nil {
		return connector.Edition{}, fmt.Errorf("receita: list editions: %w", err)
	}
	limit := editionMonth(requested)
	var months []string
	for _, e := range entries {
		if e.Dir && monthFolder.MatchString(e.Name) && e.Name <= limit {
			months = append(months, e.Name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months)))
	for i, m := range months {
		if i == maxEditionChecks {
			break
		}
		u, user, err := r.folderURL(m)
		if err != nil {
			return connector.Edition{}, err
		}
		listing, err := r.list(ctx, u, user)
		if err != nil {
			return connector.Edition{}, fmt.Errorf("receita: list edition %s: %w", m, err)
		}
		parts, published, err := r.complete(listing, required)
		if err != nil {
			var short *shortKindError
			if errors.As(err, &short) {
				// Counts only: which month and kind, how many parts were
				// listed and how many the rule asks for.
				slog.Warn("receita: month folder skipped: fewer parts than required", "edition", m,
					"kind", short.kind, "found", short.found, "required", short.required, "required_parts_from", from)
			} else {
				slog.Info("receita: edition not complete, trying the previous month", "edition", m,
					"rule", err.Error(), "required_parts_from", from)
			}
			continue
		}
		t, err := time.Parse("2006-01", m)
		if err != nil {
			return connector.Edition{}, err
		}
		r.mu.Lock()
		r.judged[m] = parts
		r.mu.Unlock()
		if i > 0 {
			slog.Info("receita: using an older edition", "edition", m, "newest_folder", months[0])
		}
		ed := connector.Edition{ReferenceDate: t.UTC()}
		if !published.IsZero() {
			p := published.UTC()
			ed.ObservedDate = time.Date(p.Year(), p.Month(), p.Day(), 0, 0, 0, 0, time.UTC)
		}
		return ed, nil
	}
	if len(months) == 0 {
		return connector.Edition{}, fmt.Errorf("receita: %w (%s)", connector.ErrNoEdition, limit)
	}
	return connector.Edition{}, fmt.Errorf("receita: %w: none of the newest %d month folders up to %s is complete",
		connector.ErrNoEdition, min(len(months), maxEditionChecks), limit)
}

// shortKindError: a data kind of a month has fewer parts than required.
type shortKindError struct {
	kind            string
	found, required int
}

func (e *shortKindError) Error() string {
	return fmt.Sprintf("%s has %d parts, at least %d required", e.kind, e.found, e.required)
}

// complete applies the completeness rules to a month's listing and returns
// its data parts and their newest Last-Modified instant (zero when none is
// parseable). A month is complete only if:
//   - each data kind has parts numbered 0..n-1 without gaps (selectFiles);
//   - each kind has at least required[kind] parts (see requiredParts): an
//     upload in name order looks gap-free at every prefix, so the count is
//     what catches it (a *shortKindError);
//   - it is settled: its newest data file was modified at least r.settle ago
//     (getlastmodified, RFC 1123 GMT, parsed strictly); a file without a
//     parseable date is not settled. r.settle 0 disables this rule.
func (r *receita) complete(listing []remoteFile, required map[string]int) ([]remoteFile, time.Time, error) {
	if _, err := selectFiles(listing); err != nil {
		return nil, time.Time{}, err
	}
	var parts []remoteFile
	count := map[string]int{}
	for _, f := range listing {
		if m := dataFile.FindStringSubmatch(f.Name); m != nil {
			count[m[1]]++
			parts = append(parts, f)
		}
	}
	for _, k := range dataKinds {
		if count[k] < required[k] {
			return nil, time.Time{}, &shortKindError{kind: k, found: count[k], required: required[k]}
		}
	}
	var newest time.Time
	for _, f := range parts {
		t, err := time.Parse(http.TimeFormat, f.LastModified)
		if err != nil {
			if r.settle > 0 {
				return nil, time.Time{}, fmt.Errorf("%s has no valid last-modified date", f.Name)
			}
			continue
		}
		if t.After(newest) {
			newest = t
		}
	}
	if r.settle > 0 {
		if age := r.now().Sub(newest); age < r.settle {
			return nil, time.Time{}, fmt.Errorf("not settled: newest file modified %s ago, %s required", age.Round(time.Minute), r.settle)
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Name < parts[j].Name })
	return parts, newest, nil
}

// requiredParts returns the minimum part count per data kind: minParts per
// kind when RECEITA_MIN_PARTS was set explicitly (it overrides the history:
// the owner's way out when the Receita legitimately publishes fewer parts);
// otherwise the counts of the last done full-scope Receita collection (from
// collection_file), or minParts per kind when there is none or it recorded
// no files. from says which ("RECEITA_MIN_PARTS", "collection <id>" or
// "default").
func (r *receita) requiredParts(ctx context.Context, pool *pgxpool.Pool) (map[string]int, string, error) {
	required := map[string]int{}
	for _, k := range dataKinds {
		required[k] = r.minParts
	}
	if r.minPartsExplicit {
		return required, "RECEITA_MIN_PARTS", nil
	}
	if pool == nil {
		return required, "default", nil
	}
	rows, err := pool.Query(ctx, `SELECT c.id, f.raw_key FROM collection c JOIN collection_file f ON f.collection_id = c.id
		WHERE c.id = (SELECT id FROM collection WHERE source_id = $1 AND status = 'done'
			AND (scope ->> 'full')::boolean IS TRUE ORDER BY reference_date DESC, id DESC LIMIT 1)`, sourceID)
	if err != nil {
		return nil, "", fmt.Errorf("receita: parts of the last collection: %w", err)
	}
	defer rows.Close()
	count := map[string]int{}
	var colID int64
	for rows.Next() {
		var key string
		if err := rows.Scan(&colID, &key); err != nil {
			return nil, "", fmt.Errorf("receita: parts of the last collection: %w", err)
		}
		if m := dataFile.FindStringSubmatch(path.Base(key)); m != nil {
			count[m[1]]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("receita: parts of the last collection: %w", err)
	}
	if colID == 0 {
		return required, "default", nil
	}
	for _, k := range dataKinds {
		if count[k] > 0 {
			required[k] = count[k]
		}
	}
	return required, fmt.Sprintf("collection %d", colID), nil
}

// errEditionChanged: the edition listing differs from the one judged complete.
var errEditionChanged = errors.New("receita: edition changed since it was judged complete")

// sameAsJudged checks that the data parts in want are exactly those
// ResolveReference judged complete for month (when it judged it).
func (r *receita) sameAsJudged(month string, want []remoteFile) error {
	r.mu.Lock()
	judged, ok := r.judged[month]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	var now []remoteFile
	for _, f := range want {
		if dataFile.MatchString(f.Name) {
			now = append(now, f)
		}
	}
	sort.Slice(now, func(i, j int) bool { return now[i].Name < now[j].Name })
	describe := func(fs []remoteFile) string {
		var b strings.Builder
		for _, f := range fs {
			fmt.Fprintf(&b, "%s/%d/%s/%s;", f.Name, f.Size, f.ETag, f.LastModified)
		}
		return b.String()
	}
	if describe(now) != describe(judged) {
		return fmt.Errorf("%w (%s: %d data parts judged, %d listed now); not loading a partial edition",
			errEditionChanged, month, len(judged), len(now))
	}
	return nil
}
