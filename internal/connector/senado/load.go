package senado

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/connector/legis"
	"geoint/internal/rawstore"
)

// loadLockKey is the session advisory lock held for a whole Load.
const loadLockKey int64 = 0x73656e61646f3031 // "senado01"

var listFile = regexp.MustCompile(`^senadores_([0-9]+)\.json$`)

// Load reads the files Fetch stored (never the network) and writes, for
// every senator in office in a fetched legislature, the person record
// (context "sen:<código>", NomeCompletoParlamentar; the Senado publishes no
// CPF) and one holds_mandate claim to the Senate per exercise of the
// mandate and party (docs/sources/senado.md). rows_read counts the senators
// of the legislature lists; a malformed one is skipped and counted by
// reason (skipped_<reason>). A response that is not shaped as documented
// fails the load.
func (c *senado) Load(ctx context.Context, col collection.Collection, files []rawstore.File, raw rawstore.Store, pool *pgxpool.Pool) (_ connector.Stats, err error) {
	if col.SourceID != sourceID {
		return nil, fmt.Errorf("senado: collection %d belongs to source %d", col.ID, col.SourceID)
	}
	if !col.Scope.Full || len(col.Scope.CNPJs) > 0 {
		return nil, ErrScope
	}
	start := time.Now()
	in, err := legis.NewInputs(files, raw, pool, col.ID, c.maxBody, c.fetched.Map())
	if err != nil {
		return nil, fmt.Errorf("senado: %w", err)
	}
	var legs []int
	for _, n := range in.Names() {
		if m := listFile.FindStringSubmatch(n); m != nil {
			l, _ := strconv.Atoi(m[1])
			legs = append(legs, l)
		}
	}
	if len(legs) == 0 {
		return nil, fmt.Errorf("senado: no legislature list among the %d files", len(files))
	}
	slices.Sort(legs)

	release, err := legis.LoadLock(ctx, pool, loadLockKey, Key)
	if err != nil {
		return nil, err
	}
	defer release()

	stats := connector.Stats{"rows_read": 0, "rows_skipped": 0, "mandates_current": 0, "mandates_ended": 0,
		"person_records_new": 0, "person_records_reused": 0, "never_in_office": 0, "exercises_partly_without_affiliation": 0,
		"unknown_leave_cause": 0, "party_without_name": 0, "closed_at_legislature_end": 0,
		"leave_without_return": 0, "leave_bounded_by_substitute": 0, "leave_open_within_substitute_gap": 0}
	w, err := legis.NewWriter(ctx, claims.NewStore(pool), sourceID, col.ID, bodyKey, bodyName, stats)
	if err != nil {
		return nil, err
	}
	l := &loader{in: in, w: w, stats: stats, ref: col.ReferenceDate, affs: map[string]cachedAffs{}}
	for _, n := range legs {
		if err := l.legislature(ctx, n); err != nil {
			return nil, err
		}
	}
	slog.Info("senado: load done", "collection", col.ID, "legislatures", len(legs), "rows_read", stats["rows_read"],
		"rows_skipped", stats["rows_skipped"], "elapsed", time.Since(start).Round(time.Millisecond).String())
	return stats, nil
}

type cachedAffs struct {
	list   []affiliation
	reason string
}

type loader struct {
	in    *legis.Inputs
	w     *legis.Writer
	stats connector.Stats
	ref   time.Time
	affs  map[string]cachedAffs
}

func (l *loader) skip(file string, item int, reason string) {
	l.stats["rows_skipped"]++
	l.stats["skipped_"+reason]++
	slog.Warn("senado: skipping senator", "file", file, "item", item, "reason", reason)
}

// legislature loads the senators of legislature n's list.
func (l *loader) legislature(ctx context.Context, n int) error {
	name := fmt.Sprintf("senadores_%d.json", n)
	b, err := l.in.Read(ctx, name)
	if err != nil {
		return fmt.Errorf("senado: %w", err)
	}
	list, err := senators(name, b)
	if err != nil {
		return err
	}
	subs := indexSubstitutes(list)
	for i, item := range list.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.stats["rows_read"]++
		var s senatorItem
		if err := json.Unmarshal(item, &s); err != nil {
			l.skip(name, i, "bad_item")
			continue
		}
		code, ok := s.code()
		if !ok {
			l.skip(name, i, "no_code")
			continue
		}
		var fullName string
		if s.Identificacao.Nome != nil {
			fullName = strings.Join(strings.Fields(*s.Identificacao.Nome), " ")
		}
		if fullName == "" {
			l.skip(name, i, "no_name")
			continue
		}
		if !s.inOffice() {
			l.stats["never_in_office"]++
			continue
		}
		base := list.Pointer(i)
		ps, ufs, reason := periods(s, n, l.ref, name, base, subs, l.stats)
		if reason != "" {
			l.skip(name, i, reason)
			continue
		}
		par := legis.Parliamentarian{ContextKey: "sen:" + code, Name: fullName,
			Ref: legis.Ref(name, listPrefix+base+"/IdentificacaoParlamentar")}
		if len(ps) > 0 {
			affs, err := l.affiliations(ctx, code)
			if err != nil {
				return err
			}
			if affs.reason != "" {
				l.skip(name, i, affs.reason)
				continue
			}
			for k, p := range ps {
				st, uncovered := split(p, ufs[k], n, affs.list)
				if uncovered {
					l.stats["exercises_partly_without_affiliation"]++
				}
				par.Stretches = append(par.Stretches, st...)
			}
		}
		if err := l.w.Write(ctx, par); err != nil {
			return err
		}
	}
	return nil
}

// affiliations reads (once) the affiliations of a senator.
func (l *loader) affiliations(ctx context.Context, code string) (cachedAffs, error) {
	if a, ok := l.affs[code]; ok {
		return a, nil
	}
	name := "senador_" + code + "_filiacoes.json"
	if !l.in.Has(name) && l.in.Has(legis.UnavailableName(name)) {
		a := cachedAffs{reason: "detail_unavailable"}
		l.affs[code] = a
		return a, nil
	}
	b, err := l.in.Read(ctx, name)
	if err != nil {
		return cachedAffs{}, fmt.Errorf("senado: %w", err)
	}
	list, reason, err := affiliations(name, b)
	if err != nil {
		return cachedAffs{}, err
	}
	a := cachedAffs{list: list, reason: reason}
	l.affs[code] = a
	return a, nil
}
