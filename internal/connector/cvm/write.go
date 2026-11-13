package cvm

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/connector"
	"geoint/internal/identity"
)

// Roles of the claims (attrs.role, part of the relationship natural key).
const (
	roleShareholder = "shareholder"
	roleController  = "controller"
)

// comparedAttrs are the attrs whose change makes a new claim: the open row
// is closed and a new one opened. reference_date and form_version are not
// compared: a newer form that declares the same position only marks the
// claim as seen again (the attrs keep the form that first declared it).
var comparedAttrs = map[claims.RelType][]string{
	claims.ShareholderOf: {"percent", "percent_on", "percent_pn"},
	claims.Controls:      nil,
}

// claim is one relationship of a listed company's latest form.
type claim struct {
	typ       claims.RelType
	role      string
	srcEntity int64  // the shareholder entity (unused when srcRecord is set)
	srcRecord *int64 // the shareholder's person record
	attrs     map[string]any
	ref       string
}

// maxChainDepth bounds how far up an indirect line's ancestors are followed
// (the real files nest up to 15 levels).
const maxChainDepth = 20

// percentTolerance: an indirect group's percentages must sum to 100 within
// this (inclusive) for them to be stored.
var percentTolerance = big.NewRat(1, 100)

// description is one form's list of the holders of a holding (a legal
// entity that is a holder in that form): the indirect lines whose parent is
// the holding's line.
type description struct {
	holding string // CNPJ base of the holding
	name    string // the holding's name in the describing form
	by      string // CNPJ base of the listed company whose form describes it
	form    formKey
	rows    []*holderRow
}

// write loads the latest form of every listed company, in CNPJ order: first
// the direct holders of each company, then the holders of every holding
// from the one form chosen to describe it (describeHoldings).
func write(ctx context.Context, pool *pgxpool.Pool, col collection.Collection, forms map[string][]holderRow, stats connector.Stats) error {
	st := claims.NewStore(pool)
	w := &writer{ctx: ctx, pool: pool, st: st, col: col, stats: stats}
	for _, base := range sortedKeys(forms) {
		forms[base] = dropDuplicateIDs(forms[base], stats)
	}
	for _, base := range sortedKeys(forms) {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows := forms[base]
		dst, err := st.UpsertEntity(ctx, claims.KindCompany, base, rows[0].CompanyName)
		if err != nil {
			return fmt.Errorf("cvm: company %s: %w", base, err)
		}
		stats["forms_loaded"]++
		if rows[0].Form.Year < col.ReferenceDate.UTC().Year() {
			stats["companies_from_previous_year"]++
		}
		var direct []*holderRow
		for i := range rows {
			if rows[i].ParentID == 0 {
				direct = append(direct, &rows[i])
			}
		}
		list, err := w.claims(base, direct, nil, true)
		if err != nil {
			return err
		}
		if err := assertAll(ctx, pool, col, dst, list, stats); err != nil {
			return fmt.Errorf("cvm: company %s: %w", base, err)
		}
	}
	descs := describeHoldings(forms, stats)
	for _, h := range sortedKeys(descs) {
		if err := ctx.Err(); err != nil {
			return err
		}
		d := descs[h]
		dst, err := st.UpsertEntity(ctx, claims.KindCompany, h, d.name)
		if err != nil {
			return fmt.Errorf("cvm: holding %s: %w", h, err)
		}
		stats["holdings_described"]++
		before := stats["shareholders_loaded"]
		list, err := w.claims(h, d.rows, map[string]any{"described_by": d.by}, groupComplete(d.rows))
		if err != nil {
			return err
		}
		stats["indirect_shareholders_loaded"] += stats["shareholders_loaded"] - before
		if err := assertAll(ctx, pool, col, dst, list, stats); err != nil {
			return fmt.Errorf("cvm: holding %s: %w", h, err)
		}
	}
	return nil
}

type writer struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	st     *claims.Store
	col    collection.Collection
	stats  connector.Stats
	logged int
}

// claims resolves the holders of one company (dstBase: the listed company,
// or a holding) and returns their claims. Lines are taken in ID_Acionista
// order, so a holder listed twice keeps its lowest-id line whatever the
// file order. withPercent false leaves the percentages out (an indirect
// group that does not sum to 100). extra attrs are added to every claim.
func (w *writer) claims(dstBase string, rows []*holderRow, extra map[string]any, withPercent bool) ([]claim, error) {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b *holderRow) int { return cmp.Compare(a.ID, b.ID) })
	var list []claim
	kept := map[string]*holderRow{}
	for _, r := range rows {
		switch {
		case strings.HasPrefix(r.NotLoaded, "aggregate_"):
			w.stats[r.NotLoaded]++
			continue
		case r.Defect != "":
			w.stats["rows_skipped"]++
			w.stats["latest_form_lines_skipped"]++
			w.stats[r.Defect]++
			if w.logged < maxLoggedMalformed {
				w.logged++
				// Never the row content: it holds personal data.
				slog.Warn("cvm: skipping malformed row", "file", entryName(r.Form.Year), "line", r.Line, "reason", r.Defect)
			}
			continue
		case r.NotLoaded != "":
			w.stats[r.NotLoaded]++
			continue
		case r.Kind == holderCompany && r.HolderBase == dstBase:
			w.stats["self_holding"]++
			continue
		}
		k := r.identityKey()
		if first, dup := kept[k]; dup {
			w.stats["duplicate_shareholder"]++
			if first.Percent != r.Percent || first.PercentON != r.PercentON || first.PercentPN != r.PercentPN || first.Controller != r.Controller {
				w.stats["duplicate_conflict"]++
			}
			continue
		}
		kept[k] = r
		ref := fmt.Sprintf("%s:%d", entryName(r.Form.Year), r.Line)
		c := claim{ref: ref}
		var err error
		switch r.Kind {
		case holderCompany:
			c.srcEntity, err = w.st.UpsertEntity(w.ctx, claims.KindCompany, r.HolderBase, r.Name)
		case holderForeign:
			c.srcEntity, err = w.st.UpsertEntity(w.ctx, claims.KindForeign, foreignKey(dstBase, r.NameNorm), r.Name)
		case holderPerson:
			var rec int64
			rec, _, err = w.st.UpsertPersonRecord(w.ctx, claims.PersonRecordInput{
				SourceID: sourceID, ContextKey: "cvm:" + dstBase, Name: r.Name, CPFFragment: r.Fragment,
				Prov: claims.Provenance{CollectionID: w.col.ID, RecordRef: ref},
			})
			c.srcRecord = &rec
		}
		if err != nil {
			return nil, fmt.Errorf("cvm: %s: shareholder: %w", ref, err)
		}
		w.stats["shareholders_loaded"]++
		sh := c
		sh.typ, sh.role = claims.ShareholderOf, roleShareholder
		sh.attrs = map[string]any{"role": roleShareholder, "reference_date": r.Form.RefDate, "form_version": r.Form.Version}
		if withPercent {
			sh.attrs["percent"] = r.Percent
			if r.PercentON != "" {
				sh.attrs["percent_on"] = r.PercentON
			}
			if r.PercentPN != "" {
				sh.attrs["percent_pn"] = r.PercentPN
			}
		}
		maps.Copy(sh.attrs, extra)
		list = append(list, sh)
		if r.Controller {
			ct := c
			ct.typ, ct.role = claims.Controls, roleController
			ct.attrs = map[string]any{"role": roleController, "reference_date": r.Form.RefDate, "form_version": r.Form.Version}
			maps.Copy(ct.attrs, extra)
			list = append(list, ct)
		}
	}
	return list, nil
}

// dropDuplicateIDs keeps, for every ID_Acionista of one form, its lowest
// line and skips the later ones (duplicate_shareholder_id, counted as
// skipped lines too): an id names one line of the tree, and a second line
// with the same id would make parent lookups depend on file order. Rows are
// returned in line order.
func dropDuplicateIDs(rows []holderRow, stats connector.Stats) []holderRow {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b holderRow) int { return cmp.Compare(a.Line, b.Line) })
	seen := map[int64]bool{}
	out := rows[:0]
	for _, r := range rows {
		if seen[r.ID] {
			stats["duplicate_shareholder_id"]++
			stats["rows_skipped"]++
			stats["latest_form_lines_skipped"]++
			continue
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	return out
}

// groupComplete reports whether the total percentages of a description
// (aggregate lines included) sum to 100 within percentTolerance, in exact
// decimal arithmetic. An all-zero group means "not informed".
func groupComplete(rows []*holderRow) bool {
	sum := new(big.Rat)
	for _, r := range rows {
		if !r.PercentOK {
			return false
		}
		v, ok := new(big.Rat).SetString(r.Percent)
		if !ok {
			return false
		}
		sum.Add(sum, v)
	}
	diff := new(big.Rat).Sub(sum, big.NewRat(100, 1))
	return diff.Abs(diff).Cmp(percentTolerance) <= 0
}

// describeHoldings picks, for every holding described by indirect lines,
// the one description that is loaded (docs/sources/cvm.md):
//   - the holding is the holder of the indirect lines' parent line, which
//     must be a legal entity with a CNPJ (indirect_parent_not_company);
//     the parent is looked up by ID_Acionista in the same form, and its own
//     ancestors are followed up to maxChainDepth with a cycle guard;
//   - a holding that is itself a listed company with a current form is
//     never described by others: its own direct lines are the truth
//     (indirect_superseded_by_own_form);
//   - within one form, a holding under several parent lines is described
//     by a complete group (groupComplete) rather than an incomplete one (an
//     all-zero "not informed" group, say), then by the lowest parent id
//     (indirect_duplicate_description for the lines of the others; each
//     other that differs also counts indirect_duplicate_conflict);
//   - otherwise the description from the newest form (formKey.newer, then
//     the smallest describing CNPJ base) wins; the lines of the others are
//     indirect_superseded, and each other description that differs is an
//     indirect_conflict.
//
// The choice does not depend on the order of files or lines.
func describeHoldings(forms map[string][]holderRow, stats connector.Stats) map[string]description {
	cands := map[string][]description{}
	for _, base := range sortedKeys(forms) {
		rows := forms[base]
		byID := map[int64]*holderRow{}
		groups := map[int64][]*holderRow{}
		for i := range rows {
			r := &rows[i]
			if _, dup := byID[r.ID]; !dup {
				byID[r.ID] = r
			}
			if r.ParentID != 0 {
				groups[r.ParentID] = append(groups[r.ParentID], r)
			}
		}
		type group struct {
			parent *holderRow
			lines  []*holderRow
		}
		described := map[string][]group{} // per holding, in parent id order
		for _, pid := range sortedKeys(groups) {
			lines := groups[pid]
			n := int64(len(lines))
			parent, ok := byID[pid]
			if !ok {
				stats["indirect_parent_missing"] += n
				continue
			}
			if reason := chainProblem(parent, byID); reason != "" {
				stats[reason] += n
				continue
			}
			if parent.Kind != holderCompany || parent.Defect != "" || parent.NotLoaded != "" {
				stats["indirect_parent_not_company"] += n
				continue
			}
			described[parent.HolderBase] = append(described[parent.HolderBase], group{parent, lines})
		}
		for _, h := range sortedKeys(described) {
			gs := described[h]
			// A complete group first; the lowest parent id among equals.
			best := 0
			for i, g := range gs {
				if groupComplete(g.lines) {
					best = i
					break
				}
			}
			for i, g := range gs {
				if i == best {
					continue
				}
				// The same holding under another parent line of one form.
				stats["indirect_duplicate_description"] += int64(len(g.lines))
				if !slices.Equal(fingerprint(g.lines), fingerprint(gs[best].lines)) {
					stats["indirect_duplicate_conflict"]++
				}
			}
			b := gs[best]
			cands[h] = append(cands[h], description{holding: h, name: b.parent.Name, by: base, form: b.parent.Form, rows: b.lines})
		}
	}
	out := map[string]description{}
	for _, h := range sortedKeys(cands) {
		ds := cands[h]
		if _, listed := forms[h]; listed {
			for _, d := range ds {
				stats["indirect_superseded_by_own_form"] += int64(len(d.rows))
			}
			continue
		}
		best := ds[0]
		for _, d := range ds[1:] {
			if d.form.newer(best.form) || (d.form == best.form && d.by < best.by) {
				best = d
			}
		}
		for _, d := range ds {
			if d.by == best.by {
				continue
			}
			stats["indirect_superseded"] += int64(len(d.rows))
			if !slices.Equal(fingerprint(d.rows), fingerprint(best.rows)) {
				stats["indirect_conflict"]++
			}
		}
		out[h] = best
	}
	return out
}

// chainProblem follows a parent line's ancestors: "" when they end at a
// direct line within maxChainDepth.
func chainProblem(r *holderRow, byID map[int64]*holderRow) string {
	seen := map[int64]bool{r.ID: true}
	for depth := 1; r.ParentID != 0; depth++ {
		if depth > maxChainDepth {
			return "indirect_too_deep"
		}
		p, ok := byID[r.ParentID]
		if !ok {
			return "indirect_parent_missing"
		}
		if seen[p.ID] {
			return "indirect_cycle"
		}
		seen[p.ID] = true
		r = p
	}
	return ""
}

// fingerprint is what two descriptions of a holding are compared by.
func fingerprint(rows []*holderRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		k := r.NotLoaded
		if k == "" {
			k = r.identityKey()
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%v", k, r.Percent, r.PercentON, r.PercentPN, r.Controller))
	}
	slices.Sort(out)
	return out
}

func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Statements of assertAll. The open-row lookup uses the expressions of the
// relationship natural key (migration 0001) and its partial index.
const (
	sqlRecordEntity = `SELECT entity_id FROM person_record WHERE id = $1`

	// $1 type, $2 source end key, $3 destination end key, $4 role, $5 cvm
	// source id, $6 the collection's reference date. mine: a fact first
	// written by a CVM collection, so the CVM may close it.
	sqlFindOpen = `SELECT r.id, r.attrs, (r.kind = 'fact' AND fc.source_id = $5) AS mine,
			least(lc.reference_date, $6::date)
		FROM relationship r
		JOIN collection fc ON fc.id = r.first_collection_id
		JOIN collection lc ON lc.id = coalesce(r.last_seen_collection_id, r.first_collection_id)
		WHERE r.rel_type = $1 AND coalesce(r.src_record_id, -r.src_entity_id) = $2
		  AND coalesce(r.dst_record_id, -r.dst_entity_id) = $3 AND r.attrs ->> 'role' = $4
		  AND r.valid_from IS NULL AND r.closed_collection_id IS NULL
		FOR UPDATE OF r`

	sqlSeen = `UPDATE relationship SET last_seen_collection_id = GREATEST(last_seen_collection_id, $2) WHERE id = $1`

	// Closing a changed claim: the same columns and bounds CloseUnseen uses
	// ($4 = closingBound).
	sqlCloseChanged = `UPDATE relationship SET closed_collection_id = $2, valid_to_after = $3, valid_to_before = $4
		WHERE id = $1 AND closed_collection_id IS NULL`

	sqlInsert = `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, src_record_id, dst_record_id,
			attrs, kind, confidence, valid_from, first_collection_id, record_ref, last_seen_collection_id)
		VALUES ($1, $2, $3, $4, NULL, $5::jsonb, 'fact', NULL, NULL, $6, $7, $6)
		ON CONFLICT (rel_type, (coalesce(src_record_id, -src_entity_id)), (coalesce(dst_record_id, -dst_entity_id)),
			(attrs ->> 'role'), valid_from)
			WHERE closed_collection_id IS NULL
		DO UPDATE SET last_seen_collection_id = GREATEST(relationship.last_seen_collection_id, EXCLUDED.last_seen_collection_id)
		RETURNING (xmax = 0)`
)

// assertAll writes the claims of one company in one short transaction under
// the identity lock (the entity of a person record is read and stored, so it
// must not move meanwhile). For each claim: no open row → insert; an open
// row with the same compared attrs (or one the CVM does not own) → mark it
// seen; an open CVM row whose attrs changed → close it in this collection
// (valid_to_after = the reference date of the collection that saw it last,
// valid_to_before = this collection's observed date + 1, the bound
// CloseUnseen uses) and insert the new claim.
func assertAll(ctx context.Context, pool *pgxpool.Pool, col collection.Collection, dst int64, list []claim, stats connector.Stats) error {
	if len(list) == 0 {
		return nil
	}
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := identity.LockTx(ctx, tx); err != nil {
			return err
		}
		for _, c := range list {
			src, srcKey := c.srcEntity, -c.srcEntity
			if c.srcRecord != nil {
				var ent *int64
				if err := tx.QueryRow(ctx, sqlRecordEntity, *c.srcRecord).Scan(&ent); err != nil {
					return fmt.Errorf("%s: person record: %w", c.ref, err)
				}
				if ent == nil {
					return fmt.Errorf("%s: %w", c.ref, claims.ErrUnknownRecord)
				}
				src, srcKey = *ent, *c.srcRecord
			}
			var (
				id       int64
				old      map[string]any
				mine     bool
				lastSeen time.Time
			)
			err := tx.QueryRow(ctx, sqlFindOpen, c.typ, srcKey, -dst, c.role, sourceID, col.ReferenceDate).Scan(&id, &old, &mine, &lastSeen)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return fmt.Errorf("%s: find open claim: %w", c.ref, err)
			case !mine || sameAttrs(c.typ, old, c.attrs):
				if _, err := tx.Exec(ctx, sqlSeen, id, col.ID); err != nil {
					return fmt.Errorf("%s: mark claim seen: %w", c.ref, err)
				}
				stats["relationships_seen"]++
				continue
			default:
				if _, err := tx.Exec(ctx, sqlCloseChanged, id, col.ID, lastSeen, closingBound(col)); err != nil {
					return fmt.Errorf("%s: close changed claim: %w", c.ref, err)
				}
				stats["relationships_replaced"]++
			}
			attrs, err := json.Marshal(c.attrs)
			if err != nil {
				return fmt.Errorf("%s: encode attrs: %w", c.ref, err)
			}
			var inserted bool
			if err := tx.QueryRow(ctx, sqlInsert, c.typ, src, dst, c.srcRecord, string(attrs), col.ID, c.ref).Scan(&inserted); err != nil {
				return fmt.Errorf("%s: insert claim: %w", c.ref, err)
			}
			if inserted {
				stats["relationships_inserted"]++
			} else {
				stats["relationships_seen"]++
			}
		}
		return nil
	})
}

// sameAttrs reports whether the compared attrs of an open claim equal the
// new ones (both absent counts as equal).
func sameAttrs(typ claims.RelType, old, new map[string]any) bool {
	for _, k := range comparedAttrs[typ] {
		o, okOld := old[k]
		n, okNew := new[k]
		if okOld != okNew || (okOld && fmt.Sprint(o) != fmt.Sprint(n)) {
			return false
		}
	}
	return true
}

// closingBound is the exclusive upper bound of a claim col found changed:
// the day after the date its state was observed (claims.CloseUnseen's rule;
// for the CVM, the day the collection started). The reference date is never
// used for it.
func closingBound(col collection.Collection) time.Time {
	d := col.ObservedDate
	if d.IsZero() || d.Before(col.ReferenceDate) {
		d = col.ReferenceDate // a collection struct built by hand; never stored so
	}
	return d.AddDate(0, 0, 1)
}
