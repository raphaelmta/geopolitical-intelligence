package camara

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
const loadLockKey int64 = 0x63616d6172613031 // "camara01"

var legislatureFile = regexp.MustCompile(`^legislaturas_([0-9]+)\.json$`)

// Load reads the files Fetch stored (never the network) and writes, for
// every deputy in office in a fetched legislature: the person record
// (context "dep:<id>", civil name, CPF fragment) and one holds_mandate
// claim to the chamber per stretch in office (docs/sources/camara.md).
// rows_read counts the items of the deputies lists; an item without id, or
// whose deputy and legislature have a malformed event, is skipped and
// counted by reason (skipped_<reason>). A response that is not shaped as
// documented fails the load.
func (c *camara) Load(ctx context.Context, col collection.Collection, files []rawstore.File, raw rawstore.Store, pool *pgxpool.Pool) (_ connector.Stats, err error) {
	if col.SourceID != sourceID {
		return nil, fmt.Errorf("camara: collection %d belongs to source %d", col.ID, col.SourceID)
	}
	if !col.Scope.Full || len(col.Scope.CNPJs) > 0 {
		return nil, ErrScope
	}
	start := time.Now()
	in, err := legis.NewInputs(files, raw, pool, col.ID, c.maxBody, c.fetched.Map())
	if err != nil {
		return nil, fmt.Errorf("camara: %w", err)
	}
	var legs []int
	for _, n := range in.Names() {
		if m := legislatureFile.FindStringSubmatch(n); m != nil {
			l, _ := strconv.Atoi(m[1])
			legs = append(legs, l)
		}
	}
	if len(legs) == 0 {
		return nil, fmt.Errorf("camara: no legislature among the %d files", len(files))
	}
	slices.Sort(legs)

	release, err := legis.LoadLock(ctx, pool, loadLockKey, Key)
	if err != nil {
		return nil, err
	}
	defer release()

	stats := connector.Stats{"rows_read": 0, "rows_skipped": 0, "events_read": 0, "mandates_current": 0, "mandates_ended": 0,
		"person_records_new": 0, "person_records_reused": 0, "deputies_without_mandate": 0,
		"unknown_leave_cause": 0, "substitute_exit_other": 0, "party_without_name": 0, "closed_at_legislature_end": 0}
	ends := map[int]time.Time{}
	// listed[id][legislature] = list items of that deputy in that legislature
	listed := map[int64]map[int]int64{}
	// partyNames maps a party key to its name; legislatures are read in
	// order, so the newest legislature's name wins.
	partyNames := map[string]string{}
	for _, n := range legs {
		name := fmt.Sprintf("legislaturas_%d.json", n)
		b, err := in.Read(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("camara: %w", err)
		}
		if _, ends[n], err = legislature(name, b, n); err != nil {
			return nil, err
		}
		if err := readParties(ctx, in, n, partyNames); err != nil {
			return nil, err
		}
		if err := readLists(ctx, in, n, listed, stats); err != nil {
			return nil, err
		}
	}

	w, err := legis.NewWriter(ctx, claims.NewStore(pool), sourceID, col.ID, bodyKey, bodyName, stats)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(listed))
	for id := range listed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.loadDeputy(ctx, in, w, id, listed[id], ends, partyNames, col.ReferenceDate, stats); err != nil {
			return nil, err
		}
	}
	slog.Info("camara: load done", "collection", col.ID, "deputies", len(ids), "rows_read", stats["rows_read"],
		"rows_skipped", stats["rows_skipped"], "elapsed", time.Since(start).Round(time.Millisecond).String())
	return stats, nil
}

// readParties reads every page of legislature n's parties list into names.
func readParties(ctx context.Context, in *legis.Inputs, n int, names map[string]string) error {
	for page := 1; ; page++ {
		name := fmt.Sprintf("partidos_%d_p%d.json", n, page)
		b, err := in.Read(ctx, name)
		if err != nil {
			return fmt.Errorf("camara: %w", err)
		}
		list, next, err := items(name, b)
		if err != nil {
			return err
		}
		for _, it := range list {
			var p struct {
				Sigla, Nome string
			}
			if json.Unmarshal(it, &p) != nil {
				continue
			}
			k, nome := legis.PartyKey(p.Sigla), strings.Join(strings.Fields(p.Nome), " ")
			if k != "" && nome != "" {
				names[k] = nome
			}
		}
		if !next {
			return nil
		}
	}
}

// readLists reads every page of legislature n's deputies list.
func readLists(ctx context.Context, in *legis.Inputs, n int, listed map[int64]map[int]int64, stats connector.Stats) error {
	for page := 1; ; page++ {
		name := fmt.Sprintf("deputados_%d_p%d.json", n, page)
		b, err := in.Read(ctx, name)
		if err != nil {
			return fmt.Errorf("camara: %w", err)
		}
		list, next, err := items(name, b)
		if err != nil {
			return err
		}
		for i, it := range list {
			stats["rows_read"]++
			id, ok := deputyID(it)
			if !ok {
				skip(stats, "no_id", 1)
				slog.Warn("camara: skipping list item", "file", name, "item", i, "reason", "no_id")
				continue
			}
			if listed[id] == nil {
				listed[id] = map[int]int64{}
			}
			listed[id][n]++
		}
		if !next {
			return nil
		}
	}
}

func skip(stats connector.Stats, reason string, items int64) {
	stats["rows_skipped"] += items
	stats["skipped_"+reason] += items
}

// loadDeputy writes one deputy's record and mandates in the legislatures he
// was listed in.
func (c *camara) loadDeputy(ctx context.Context, in *legis.Inputs, w *legis.Writer, id int64, legs map[int]int64,
	ends map[int]time.Time, partyNames map[string]string, ref time.Time, stats connector.Stats) error {
	s := strconv.FormatInt(id, 10)
	detailName, histName := "deputado_"+s+".json", "deputado_"+s+"_historico.json"
	order := make([]int, 0, len(legs))
	for n := range legs {
		order = append(order, n)
	}
	slices.Sort(order)
	for _, name := range []string{detailName, histName} {
		// The API answered 4xx for this deputy (marker from Fetch): skip
		// him. Safe: the Câmara is not a snapshot source, so a deputy
		// missing from one collection never closes anything.
		if !in.Has(name) && in.Has(legis.UnavailableName(name)) {
			for _, n := range order {
				skip(stats, "detail_unavailable", legs[n])
			}
			slog.Warn("camara: skipping deputy", "id", id, "reason", "detail_unavailable")
			return nil
		}
	}
	b, err := in.Read(ctx, detailName)
	if err != nil {
		return fmt.Errorf("camara: %w", err)
	}
	p, err := deputy(detailName, b, id)
	if err != nil {
		return err
	}
	b, err = in.Read(ctx, histName)
	if err != nil {
		return fmt.Errorf("camara: %w", err)
	}
	evs, read, reason, err := history(histName, b)
	if err != nil {
		return err
	}
	stats["events_read"] += int64(read)
	if reason == "" && p.Name == "" {
		reason = "no_name"
	}
	if reason != "" {
		for _, n := range order {
			skip(stats, reason, legs[n])
		}
		slog.Warn("camara: skipping deputy", "id", id, "reason", reason)
		return nil
	}
	par := legis.Parliamentarian{ContextKey: "dep:" + s, Name: p.Name, CPFFragment: p.Fragment, Ref: legis.Ref(detailName, "/dados")}
	for _, n := range order {
		st, reason := stretches(evs, n, ends[n], ref, histName, stats)
		if reason != "" {
			skip(stats, reason, legs[n])
			slog.Warn("camara: skipping deputy in legislature", "id", id, "legislature", n, "reason", reason)
			continue
		}
		for i := range st {
			st[i].PartyName = partyNames[st[i].Party]
		}
		par.Stretches = append(par.Stretches, st...)
	}
	if len(par.Stretches) == 0 {
		stats["deputies_without_mandate"]++
		return nil
	}
	return w.Write(ctx, par)
}
