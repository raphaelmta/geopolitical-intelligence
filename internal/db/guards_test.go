package db_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/db"
	"geoint/internal/testutil"
)

// Characterisation of the update guards of the three claim tables
// (relationship, person_record, company_version): for every column, every
// starting state that matters (never seen, seen, closed; unlocked, locked)
// and every role, which UPDATE is allowed and which is refused, with which
// SQLSTATE and message. The expectation is a small rule engine (expectGuard)
// that states the guard of migration 0001 in Go:
//
//  1. a column not on the table's allow-list may not change (NULL counts as
//     a value): "<table> claim columns are immutable";
//  2. a closed row stays closed with the same closing collection:
//     "<table> rows cannot be reopened";
//  3. last_seen_collection_id, once set, never becomes NULL nor smaller:
//     "<table> last_seen_collection_id cannot decrease";
//
// checked in that order, after the privilege check of the role (42501) and
// before NOT NULL (23502). TestUpdateGuardMatchesTheOriginal compares the
// installed guard with the original function itself, statement by
// statement, over every pair of columns.

// updateGuardAllow is the allow-list of each claim table's update guard:
// the only columns an UPDATE may change. Every other column, including one a
// later migration adds, is immutable.
var updateGuardAllow = map[string][]string{
	"relationship":    {"src_entity_id", "dst_entity_id", "last_seen_collection_id", "closed_collection_id", "valid_to_after", "valid_to_before"},
	"person_record":   {"entity_id", "identity_confidence", "locked_by_decision_id"},
	"company_version": {"last_seen_collection_id", "closed_collection_id"},
}

var guardedTables = []string{"relationship", "person_record", "company_version"}

// A value an UPDATE assigns to a column.
type variantKind int

const (
	vSame  variantKind = iota // col = col
	vOther                    // an expression that always differs from the current value
	vNull                     // NULL
	vColl                     // the id of collection coll (1 < 2 < 3)
)

type variant struct {
	kind variantKind
	expr string // vOther: SQL expression; may use {eOther}, {prOther}, {dOther}
	coll int
}

// guardColumns lists, per table, every column with the values an UPDATE
// tries. TestUpdateGuardCharacterisation fails when the table has a column
// missing here, so a new column gets its cases.
var guardColumns = map[string][]struct {
	col      string
	variants []variant
}{
	"relationship": {
		{"id", others("id + 1000000")},
		{"rel_type", others("CASE rel_type WHEN 'shareholder_of' THEN 'controls' ELSE 'shareholder_of' END")},
		{"src_entity_id", others("{eOther}", null)},
		{"dst_entity_id", others("{eOther}", null)},
		{"src_record_id", others("{prOther}", null)},
		{"dst_record_id", others("{prOther}", null)},
		{"attrs", others(`attrs || '{"zz": 1}'`, null)},
		{"kind", others("CASE kind WHEN 'fact' THEN 'inference' ELSE 'fact' END")},
		{"confidence", others("CASE WHEN confidence IS NULL THEN 0.25 ELSE confidence / 2 END", null)},
		{"valid_from", others("coalesce(valid_from, '1999-01-01') + 1", null)},
		{"valid_to_after", others("coalesce(valid_to_after, '1999-01-01') + 1", null)},
		{"valid_to_before", others("coalesce(valid_to_before, '1999-01-01') + 1", null)},
		{"first_collection_id", colls()},
		{"record_ref", others("record_ref || 'x'", null)},
		{"last_seen_collection_id", colls()},
		{"closed_collection_id", colls()},
		{"recorded_at", others("recorded_at - interval '1 day'", null)},
	},
	"person_record": {
		{"id", others("id + 1000000")},
		{"source_id", others("CASE source_id WHEN 1 THEN 2 ELSE 1 END", null)},
		{"context_key", others("context_key || 'x'", null)},
		{"name", others("name || 'x'", null)},
		{"name_norm", others("name_norm || 'X'", null)},
		{"cpf_fragment", others("CASE cpf_fragment WHEN '' THEN '999999' ELSE '' END", null)},
		{"entity_id", others("{eOther}", null)},
		{"identity_confidence", others("identity_confidence / 2", null)},
		{"locked_by_decision_id", others("{dOther}", null)},
		{"first_collection_id", colls()},
		{"record_ref", others("record_ref || 'x'", null)},
	},
	"company_version": {
		{"id", others("id + 1000000")},
		{"entity_id", others("{eOther}", null)},
		{"name", others("name || 'x'", null)},
		{"legal_nature", others("legal_nature || 'x'", null)},
		{"size", others("size || 'x'", null)},
		{"uf", others("uf || 'x'", null)},
		{"city", others("city || 'x'", null)},
		{"cnae", others("cnae || 'x'", null)},
		{"status", others("status || 'x'", null)},
		{"capital", others("coalesce(capital, 0) + 1", null)},
		{"first_collection_id", colls()},
		{"record_ref", others("record_ref || 'x'", null)},
		{"last_seen_collection_id", colls()},
		{"closed_collection_id", colls()},
		{"recorded_at", others("recorded_at - interval '1 day'", null)},
	},
}

var null = variant{kind: vNull}

// others: no-op, the given expressions (or NULL).
func others(vs ...any) []variant {
	out := []variant{{kind: vSame}}
	for _, v := range vs {
		switch v := v.(type) {
		case string:
			out = append(out, variant{kind: vOther, expr: v})
		case variant:
			out = append(out, v)
		}
	}
	return out
}

// colls: no-op, NULL and each of the three collections.
func colls() []variant {
	return []variant{{kind: vSame}, null, {kind: vColl, coll: 1}, {kind: vColl, coll: 2}, {kind: vColl, coll: 3}}
}

// guardFixture: rows in each state that matters, written by the owner.
type guardFixture struct {
	coll  [4]int64          // coll[1..3], increasing ids
	vars  map[string]string // {eOther}, {prOther}, {dOther}
	rows  map[string][]guardRow
	owner *pgxpool.Pool
}

type guardRow struct {
	name string
	id   int64
}

func newGuardFixture(t *testing.T, owner *pgxpool.Pool) *guardFixture {
	t.Helper()
	f := &guardFixture{owner: owner, vars: map[string]string{}, rows: map[string][]guardRow{}}
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := owner.QueryRow(roleCtx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	for i := 1; i <= 3; i++ {
		f.coll[i] = one(`INSERT INTO collection (source_id, reference_date, observed_date)
			VALUES ((SELECT id FROM source WHERE key = 'receita'), $1::date, $1::date) RETURNING id`, fmt.Sprintf("2026-0%d-01", i))
	}
	c1, c2 := f.coll[1], f.coll[2]
	company := func(key string) int64 {
		return one(`INSERT INTO entity (kind, natural_key, name) VALUES ('company', $1, 'EMPRESA ' || $1) RETURNING id`, key)
	}
	person := func(name string) int64 {
		return one(`INSERT INTO entity (kind, name, name_norm) VALUES ('person', $1, $1) RETURNING id`, name)
	}
	ca, cb, cc, cd, ce := company("00000001"), company("00000002"), company("00000003"), company("00000004"), company("00000005")
	pa, pb, pc := person("ANA"), person("BETO"), person("CARLA")
	eOther := company("00000099")
	decision := func(e int64) int64 {
		return one(`INSERT INTO identity_decision (kind, action, record_ids, entity_id, confidence, decided_by)
			VALUES ('analyst', 'merge', ARRAY[1::bigint], $1, 1, 'test') RETURNING id`, e)
	}
	d1, dOther := decision(pb), decision(pb)
	record := func(key, name, frag string, ent int64, locked *int64) int64 {
		return one(`INSERT INTO person_record (source_id, context_key, name, name_norm, cpf_fragment, entity_id,
				identity_confidence, locked_by_decision_id, first_collection_id, record_ref)
			VALUES ((SELECT id FROM source WHERE key = 'receita'), $1, $2, $2, $3, $4, 1, $5, $6, 'ref-' || $1) RETURNING id`,
			key, name, frag, ent, locked, c1)
	}
	prFree := record("00000001", "ANA", "123456", pa, nil)
	prLocked := record("00000002", "BETO", "", pb, &d1)
	prOther := record("00000003", "CARLA", "654321", pc, nil)
	f.rows["person_record"] = []guardRow{{"unlocked", prFree}, {"locked", prLocked}}

	rel := func(typ string, src, dst int64, srcRec, dstRec *int64, attrs, kind string, conf *float64, from, toAfter, toBefore *string, lastSeen, closed *int64) int64 {
		return one(`INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, src_record_id, dst_record_id, attrs, kind,
				confidence, valid_from, valid_to_after, valid_to_before, first_collection_id, record_ref,
				last_seen_collection_id, closed_collection_id)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9::date, $10::date, $11::date, $12, 'rel-' || $1, $13, $14) RETURNING id`,
			typ, src, dst, srcRec, dstRec, attrs, kind, conf, from, toAfter, toBefore, c1, lastSeen, closed)
	}
	half := 0.5
	d0, d9, d10 := "2001-01-01", "2009-01-01", "2010-01-01"
	f.rows["relationship"] = []guardRow{
		{"never seen", rel("partner_of", pa, ca, &prFree, nil, `{}`, "fact", nil, nil, nil, nil, nil, nil)},
		{"seen", rel("controls", cb, cc, nil, &prLocked, `{"role": "a"}`, "inference", &half, &d0, &d9, &d10, &c2, nil)},
		{"closed", rel("administrator_of", pb, cd, &prLocked, nil, `{"role": "b"}`, "fact", nil, &d0, nil, nil, &c2, &c2)},
	}
	cv := func(e int64, capital *float64, lastSeen, closed *int64) int64 {
		return one(`INSERT INTO company_version (entity_id, name, legal_nature, size, uf, city, cnae, status, capital,
				first_collection_id, record_ref, last_seen_collection_id, closed_collection_id)
			VALUES ($1::bigint, 'EMPRESA', '2062', '03', 'SP', '7107', '6201501', '02', $2, $3, 'cv-' || $1::text, $4, $5) RETURNING id`,
			e, capital, c1, lastSeen, closed)
	}
	ten := 10.0
	f.rows["company_version"] = []guardRow{
		{"never seen", cv(ca, nil, nil, nil)},
		{"seen", cv(cb, &ten, &c2, nil)},
		{"closed", cv(ce, &ten, &c2, &c2)},
	}
	f.vars["{eOther}"] = fmt.Sprint(eOther)
	f.vars["{prOther}"] = fmt.Sprint(prOther)
	f.vars["{dOther}"] = fmt.Sprint(dOther)
	return f
}

// state reads a row as the owner: column → value (JSON-decoded; nil = NULL).
func (f *guardFixture) state(t *testing.T, table string, id int64) map[string]any {
	t.Helper()
	var raw []byte
	if err := f.owner.QueryRow(roleCtx, fmt.Sprintf(`SELECT to_jsonb(t) FROM %s t WHERE id = $1`, table), id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

type assignment struct {
	col string
	v   variant
}

func (f *guardFixture) sql(a assignment) string {
	switch a.v.kind {
	case vSame:
		return a.col + " = " + a.col
	case vNull:
		return a.col + " = NULL"
	case vColl:
		return fmt.Sprintf("%s = %d", a.col, f.coll[a.v.coll])
	}
	e := a.v.expr
	for k, v := range f.vars {
		e = strings.ReplaceAll(e, k, v)
	}
	return a.col + " = " + e
}

func (f *guardFixture) update(table string, id int64, as []assignment) string {
	sets := make([]string, len(as))
	for i, a := range as {
		sets[i] = f.sql(a)
	}
	return fmt.Sprintf("UPDATE %s SET %s WHERE id = %d", table, strings.Join(sets, ", "), id)
}

// Outcomes.
const (
	outOK        = "ok"
	outImmutable = "immutable"
	outReopen    = "reopen"
	outDecrease  = "decrease"
	outDenied    = "denied"   // 42501, the role's privileges
	outNotNull   = "not null" // 23502
)

var notNullAllowed = map[string][]string{
	"relationship":  {"src_entity_id", "dst_entity_id"},
	"person_record": {"identity_confidence"},
}

// expectGuard is the rule of migration 0001's guard_update, plus the
// privileges of 0021 (apiCols: nil for a role with table-level UPDATE).
func (f *guardFixture) expectGuard(table string, old map[string]any, as []assignment, apiCols []string) string {
	if apiCols != nil {
		for _, a := range as {
			if !slices.Contains(apiCols, a.col) {
				return outDenied
			}
		}
	}
	collOf := func(v any) int64 {
		if v == nil {
			return 0
		}
		return int64(v.(float64))
	}
	differs := func(a assignment) bool {
		switch a.v.kind {
		case vSame:
			return false
		case vNull:
			return old[a.col] != nil
		case vColl:
			return collOf(old[a.col]) != f.coll[a.v.coll]
		}
		return true
	}
	newColl := func(a assignment) (int64, bool) { // value, is NULL
		switch a.v.kind {
		case vSame:
			return collOf(old[a.col]), old[a.col] == nil
		case vNull:
			return 0, true
		case vColl:
			return f.coll[a.v.coll], false
		}
		panic("collection column with an expression variant")
	}
	allow := updateGuardAllow[table]
	for _, a := range as {
		if !slices.Contains(allow, a.col) && differs(a) {
			return outImmutable
		}
	}
	for _, a := range as {
		if a.col == "closed_collection_id" && old[a.col] != nil && differs(a) {
			return outReopen
		}
	}
	for _, a := range as {
		if a.col == "last_seen_collection_id" && old[a.col] != nil {
			v, isNull := newColl(a)
			if isNull || v < collOf(old[a.col]) {
				return outDecrease
			}
		}
	}
	for _, a := range as {
		if a.v.kind == vNull && slices.Contains(notNullAllowed[table], a.col) {
			return outNotNull
		}
	}
	return outOK
}

// classify runs sql in a transaction that is rolled back and names the outcome.
func classify(t *testing.T, p *pgxpool.Pool, table, sql string) string {
	t.Helper()
	tx, err := p.Begin(roleCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(roleCtx) }()
	return outcomeOf(table, execErr(tx, sql))
}

func execErr(tx pgx.Tx, sql string) error {
	_, err := tx.Exec(roleCtx, sql)
	return err
}

func outcomeOf(table string, err error) string {
	if err == nil {
		return outOK
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "error: " + err.Error()
	}
	switch {
	case pgErr.Code == "42501":
		return outDenied
	case pgErr.Code == "23502":
		return outNotNull
	case pgErr.Code == "P0001" && pgErr.Message == table+" claim columns are immutable":
		return outImmutable
	case pgErr.Code == "P0001" && pgErr.Message == table+" rows cannot be reopened":
		return outReopen
	case pgErr.Code == "P0001" && pgErr.Message == table+" last_seen_collection_id cannot decrease":
		return outDecrease
	}
	return fmt.Sprintf("%s: %s", pgErr.Code, pgErr.Message)
}

// guardCombos: updates touching several columns at once, on every row.
var guardCombos = map[string][][]assignment{
	"relationship": {
		{{"last_seen_collection_id", variant{kind: vColl, coll: 3}}, {"record_ref", variant{kind: vOther, expr: "record_ref || 'x'"}}},
		{{"src_entity_id", variant{kind: vOther, expr: "{eOther}"}}, {"rel_type", variant{kind: vOther, expr: "'shareholder_of'"}}},
		{{"closed_collection_id", null}, {"record_ref", variant{kind: vOther, expr: "record_ref || 'x'"}}},
		{{"closed_collection_id", null}, {"last_seen_collection_id", variant{kind: vColl, coll: 1}}},
		{{"last_seen_collection_id", variant{kind: vColl, coll: 1}}, {"attrs", variant{kind: vOther, expr: `'{"x": 1}'`}}},
		{{"last_seen_collection_id", variant{kind: vColl, coll: 1}}, {"closed_collection_id", variant{kind: vColl, coll: 3}}},
		{{"src_entity_id", variant{kind: vOther, expr: "{eOther}"}}, {"dst_entity_id", variant{kind: vOther, expr: "{eOther}"}},
			{"last_seen_collection_id", variant{kind: vColl, coll: 3}}, {"closed_collection_id", variant{kind: vColl, coll: 3}},
			{"valid_to_after", variant{kind: vOther, expr: "'2020-01-01'"}}, {"valid_to_before", variant{kind: vOther, expr: "'2021-01-01'"}}},
		{{"src_entity_id", null}, {"record_ref", variant{kind: vOther, expr: "record_ref || 'x'"}}},
	},
	"person_record": {
		{{"entity_id", variant{kind: vOther, expr: "{eOther}"}}, {"identity_confidence", variant{kind: vOther, expr: "0.7"}},
			{"locked_by_decision_id", variant{kind: vOther, expr: "{dOther}"}}},
		{{"entity_id", variant{kind: vOther, expr: "{eOther}"}}, {"name", variant{kind: vOther, expr: "name || 'x'"}}},
		{{"identity_confidence", null}, {"cpf_fragment", variant{kind: vOther, expr: "'x'"}}},
		{{"locked_by_decision_id", null}, {"entity_id", null}},
	},
	"company_version": {
		{{"last_seen_collection_id", variant{kind: vColl, coll: 3}}, {"closed_collection_id", variant{kind: vColl, coll: 3}}},
		{{"last_seen_collection_id", variant{kind: vColl, coll: 3}}, {"name", variant{kind: vOther, expr: "name || 'x'"}}},
		{{"closed_collection_id", null}, {"capital", null}},
		{{"closed_collection_id", null}, {"last_seen_collection_id", null}},
		{{"last_seen_collection_id", null}, {"capital", variant{kind: vSame}}},
	},
}

// wholeRowNoop: SET every column to itself.
func wholeRowNoop(table string) []assignment {
	var as []assignment
	for _, c := range guardColumns[table] {
		as = append(as, assignment{c.col, variant{kind: vSame}})
	}
	return as
}

// characterise runs the matrix for one role and returns the number of
// updates per outcome.
func characterise(t *testing.T, f *guardFixture, p *pgxpool.Pool, role string, extra []string) map[string]int {
	t.Helper()
	tally := map[string]int{}
	for _, table := range guardedTables {
		var apiCols []string
		if role == db.APIRole {
			apiCols = append([]string{}, grantsMatrix[table].apiCols...) // non-nil: no table-level UPDATE
		}
		cases := [][]assignment{wholeRowNoop(table)}
		for _, c := range guardColumns[table] {
			for _, v := range c.variants {
				cases = append(cases, []assignment{{c.col, v}})
			}
		}
		for _, col := range extra {
			cases = append(cases, []assignment{{col, variant{kind: vSame}}},
				[]assignment{{col, variant{kind: vOther, expr: "'x'"}}},
				[]assignment{{col, variant{kind: vOther, expr: "'x'"}}, {allowFirst(table), variant{kind: vSame}}})
		}
		cases = append(cases, guardCombos[table]...)
		for _, row := range f.rows[table] {
			old := f.state(t, table, row.id)
			for _, as := range cases {
				sql := f.update(table, row.id, as)
				want := f.expectGuard(table, old, as, apiCols)
				if got := classify(t, p, table, sql); got != want {
					t.Errorf("%s, %s row: %s\n  got %s, want %s", role, row.name, sql, got, want)
				}
				tally[want]++
			}
		}
	}
	return tally
}

func allowFirst(table string) string { return updateGuardAllow[table][0] }

func checkGuardColumnsListed(t *testing.T, owner *pgxpool.Pool) {
	t.Helper()
	for _, table := range guardedTables {
		var listed []string
		for _, c := range guardColumns[table] {
			listed = append(listed, c.col)
		}
		cols := tableColumns(t, owner, table)
		if !slices.Equal(cols, listed) {
			t.Errorf("columns of %s are %v; guardColumns lists %v: give every column its cases", table, cols, listed)
		}
	}
}

func tableColumns(t *testing.T, owner *pgxpool.Pool, table string) []string {
	t.Helper()
	rows, err := owner.Query(roleCtx, `SELECT attname::text FROM pg_attribute
		WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped ORDER BY attnum`, table)
	if err != nil {
		t.Fatal(err)
	}
	cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return cols
}

// TestUpdateGuardCharacterisation: the refusal matrix of the claim tables'
// update guards, for the owner, the worker and the API.
func TestUpdateGuardCharacterisation(t *testing.T) {
	env := newRoleEnv(t)
	checkGuardColumnsListed(t, env.owner)
	f := newGuardFixture(t, env.owner)
	for _, r := range []struct {
		name string
		pool *pgxpool.Pool
	}{{"owner", env.owner}, {db.WorkerRole, env.worker}, {db.APIRole, env.api}} {
		t.Logf("%s: updates per outcome %v", r.name, characterise(t, f, r.pool, r.name, nil))
	}
}

// TestUpdateGuardCharacterisationWithAnUnguardedColumn: a column added
// without deciding its mutability is immutable, and nothing else changes —
// through the event trigger that re-installs the guard in the ALTER's
// transaction, and through the fallback (d) when the event trigger is
// skipped.
func TestUpdateGuardCharacterisationWithAnUnguardedColumn(t *testing.T) {
	for _, bypass := range []bool{false, true} {
		t.Run(map[bool]string{false: "event trigger", true: "fallback"}[bypass], func(t *testing.T) {
			env := newRoleEnv(t)
			f := newGuardFixture(t, env.owner)
			for _, table := range guardedTables {
				addColumn(t, env.owner, table, "extra text", bypass)
			}
			for _, r := range []struct {
				name string
				pool *pgxpool.Pool
			}{{"owner", env.owner}, {db.WorkerRole, env.worker}, {db.APIRole, env.api}} {
				characterise(t, f, r.pool, r.name, []string{"extra"})
			}
		})
	}
}

// addColumn adds a column as the owner; bypass skips the event trigger
// (geoint.update_guard_installing), leaving the guard as it was installed.
func addColumn(t *testing.T, owner *pgxpool.Pool, table, def string, bypass bool) {
	t.Helper()
	tx, err := owner.Begin(roleCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(roleCtx) }()
	if bypass {
		if _, err := tx.Exec(roleCtx, `SET LOCAL geoint.update_guard_installing = 'on'`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(roleCtx, "ALTER TABLE "+table+" ADD COLUMN "+def); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(roleCtx); err != nil {
		t.Fatal(err)
	}
}

// originalGuard is the trigger of migration 0001, verbatim: the oracle.
func originalGuard(table string) string {
	args := make([]string, len(updateGuardAllow[table]))
	for i, c := range updateGuardAllow[table] {
		args[i] = "'" + c + "'"
	}
	return fmt.Sprintf("CREATE OR REPLACE TRIGGER %[1]s_guard_update BEFORE UPDATE ON %[1]s FOR EACH ROW EXECUTE FUNCTION guard_update(%s)",
		table, strings.Join(args, ", "))
}

// TestUpdateGuardMatchesTheOriginal: every single-column update and every
// pair of columns (all their values), on every row, gives the same outcome
// (SQLSTATE and message) with the installed guard and with the original
// guard of migration 0001 put back in a transaction.
func TestUpdateGuardMatchesTheOriginal(t *testing.T) {
	pool := newRoleEnv(t).owner
	f := newGuardFixture(t, pool)
	for _, table := range guardedTables {
		var cases [][]assignment
		cols := guardColumns[table]
		for i, a := range cols {
			for _, va := range a.variants {
				cases = append(cases, []assignment{{a.col, va}})
				for _, b := range cols[i+1:] {
					for _, vb := range b.variants {
						cases = append(cases, []assignment{{a.col, va}, {b.col, vb}})
					}
				}
			}
		}
		run := func(original bool) []string {
			tx, err := pool.Begin(roleCtx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(roleCtx) }()
			if original {
				if _, err := tx.Exec(roleCtx, originalGuard(table)); err != nil {
					t.Fatal(err)
				}
			}
			var out []string
			for _, row := range f.rows[table] {
				for _, as := range cases {
					sp, err := tx.Begin(roleCtx)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, outcomeOf(table, execErr(sp, f.update(table, row.id, as))))
					if err := sp.Rollback(roleCtx); err != nil {
						t.Fatal(err)
					}
				}
			}
			return out
		}
		installed, original := run(false), run(true)
		i := 0
		for _, row := range f.rows[table] {
			for _, as := range cases {
				if installed[i] != original[i] {
					t.Errorf("%s row: %s\n  installed guard: %s, original guard: %s", row.name, f.update(table, row.id, as), installed[i], original[i])
				}
				i++
			}
		}
		t.Logf("%s: %d updates compared", table, len(installed))
	}
}

// guardTriggerEnabled is the one state the guards must be in (pg_trigger.tgenabled):
// 'O' fires in origin and local sessions. Task 6 (session_replication_role =
// replica for the loaders) changes it to 'A' here and in a migration.
const guardTriggerEnabled = "O"

// guardTrigger is the catalogue view of <table>_guard_update.
type guardTrigger struct {
	oid      uint32
	tgtype   int16
	enabled  string
	function string
	args     []string
	layout   []string // the column list given to update_guard_columns_changed
	deps     []string // columns its WHEN clause depends on
}

var layoutArg = regexp.MustCompile(`update_guard_columns_changed\('[^']*'::regclass, ('(?:[^']|'')*')::text\[\]\)`)

func readGuardTrigger(t *testing.T, owner *pgxpool.Pool, table string) (guardTrigger, bool) {
	t.Helper()
	var g guardTrigger
	var rawArgs, def string
	err := owner.QueryRow(roleCtx, `SELECT tg.oid, tg.tgtype, tg.tgenabled::text, p.proname::text,
			encode(tg.tgargs, 'escape'), pg_get_triggerdef(tg.oid)
		FROM pg_trigger tg JOIN pg_proc p ON p.oid = tg.tgfoid
		WHERE tg.tgrelid = $1::regclass AND tg.tgname = $1 || '_guard_update'`, table).
		Scan(&g.oid, &g.tgtype, &g.enabled, &g.function, &rawArgs, &def)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, false
	}
	if err != nil {
		t.Fatal(err)
	}
	g.args = strings.Split(strings.TrimSuffix(rawArgs, `\000`), `\000`)
	if m := layoutArg.FindStringSubmatch(def); m != nil {
		// The literal as deparsed, read back by the server as text[].
		if err := owner.QueryRow(roleCtx, "SELECT "+m[1]+"::text[]").Scan(&g.layout); err != nil {
			t.Fatalf("layout literal %s: %v", m[1], err)
		}
	}
	rows, err := owner.Query(roleCtx, `SELECT a.attname::text FROM pg_depend d
		JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		WHERE d.classid = 'pg_trigger'::regclass AND d.objid = $1
			AND d.refclassid = 'pg_class'::regclass AND d.refobjid = $2::regclass AND d.refobjsubid > 0
		ORDER BY a.attnum`, g.oid, table)
	if err != nil {
		t.Fatal(err)
	}
	if g.deps, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	return g, true
}

// decidedColumns: every column whose mutability the tests have decided —
// the columns guardColumns gives cases to.
func decidedColumns() map[string][]string {
	m := map[string][]string{}
	for _, table := range guardedTables {
		for _, c := range guardColumns[table] {
			m[table] = append(m[table], c.col)
		}
	}
	return m
}

// updateGuardProblems: each claim table has its guard, a row-level BEFORE
// UPDATE trigger in the state guardTriggerEnabled, calling guard_update
// with the allow-list; every column of the table is decided (in decided)
// and either on the allow-list or compared by the guard's WHEN clause; the
// clause's column list is the table's current one (the fast path is in
// force); and the event trigger that re-installs the guards is enabled. It
// returns what is wrong.
func updateGuardProblems(t *testing.T, owner *pgxpool.Pool, allowLists, decided map[string][]string) []string {
	t.Helper()
	var problems []string
	errorf := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	var evt string
	err := owner.QueryRow(roleCtx, `SELECT e.evtenabled::text || ' ' || e.evtevent || ' ' || array_to_string(e.evttags, ',') || ' ' || p.oid::regprocedure::text
		FROM pg_event_trigger e JOIN pg_proc p ON p.oid = e.evtfoid WHERE e.evtname = 'geoint_update_guard_reinstall'`).Scan(&evt)
	if errors.Is(err, pgx.ErrNoRows) {
		errorf("event trigger geoint_update_guard_reinstall is missing")
	} else if err != nil {
		t.Fatal(err)
	} else if evt != "O ddl_command_end ALTER TABLE geoint_guard.reinstall_update_guards()" {
		errorf("event trigger geoint_update_guard_reinstall: %s", evt)
	}
	for _, table := range guardedTables {
		g, ok := readGuardTrigger(t, owner, table)
		if !ok {
			errorf("%s has no trigger %s_guard_update", table, table)
			continue
		}
		const row, before, insert, del, update, truncate = 1, 2, 4, 8, 16, 32
		if g.tgtype&(row|before|update) != row|before|update || g.tgtype&(insert|del|truncate) != 0 {
			errorf("%s_guard_update: tgtype %d, want a row-level BEFORE UPDATE trigger", table, g.tgtype)
		}
		if g.enabled != guardTriggerEnabled {
			errorf("%s_guard_update: tgenabled %q, want %q", table, g.enabled, guardTriggerEnabled)
		}
		if g.function != "guard_update" || !slices.Equal(g.args, allowLists[table]) {
			errorf("%s_guard_update calls %s(%v), want guard_update(%v)", table, g.function, g.args, allowLists[table])
		}
		cols := tableColumns(t, owner, table)
		for _, c := range cols {
			if !slices.Contains(decided[table], c) {
				errorf("column %s.%s is not decided: it is immutable until a migration puts it on the update guard's "+
					"allow-list (SELECT install_update_guard('%s', ARRAY[…]), see 0023_cheap_update_guards.sql); "+
					"decide, then list it in guardColumns (and updateGuardAllow if it may change)", table, c, table)
			}
			if !slices.Contains(allowLists[table], c) && !slices.Contains(g.deps, c) {
				errorf("column %s.%s is neither on the update guard's allow-list nor compared by its WHEN clause", table, c)
			}
		}
		if !slices.Equal(g.layout, cols) {
			errorf("%s_guard_update was installed for the columns %v, the table has %v (every update takes the slow path)",
				table, g.layout, cols)
		}
	}
	return problems
}

// TestUpdateGuardCatalogue: the guards are installed as described, and
// default-deny is kept for columns added later: a new column is compared by
// the guard from the ALTER on (the event trigger re-installs it), and this
// check fails until the column is decided.
func TestUpdateGuardCatalogue(t *testing.T) {
	env := newRoleEnv(t)
	for _, p := range updateGuardProblems(t, env.owner, updateGuardAllow, decidedColumns()) {
		t.Error(p)
	}
	var workerExec, apiExec, workerInstall bool
	if err := env.owner.QueryRow(roleCtx, `SELECT
			has_function_privilege('geoint_worker', 'update_guard_columns_changed(regclass, text[])', 'EXECUTE'),
			has_function_privilege('geoint_api', 'update_guard_columns_changed(regclass, text[])', 'EXECUTE'),
			has_function_privilege('geoint_worker', 'install_update_guard(regclass, text[])', 'EXECUTE')`).
		Scan(&workerExec, &apiExec, &workerInstall); err != nil {
		t.Fatal(err)
	}
	var schemaOwned, workerSchema, apiSchema bool
	if err := env.owner.QueryRow(roleCtx, `SELECT n.nspowner = (SELECT oid FROM pg_roles WHERE rolname = current_user),
			has_schema_privilege('geoint_worker', 'geoint_guard', 'USAGE') OR has_schema_privilege('geoint_worker', 'geoint_guard', 'CREATE'),
			has_schema_privilege('geoint_api', 'geoint_guard', 'USAGE') OR has_schema_privilege('geoint_api', 'geoint_guard', 'CREATE')
		FROM pg_namespace n WHERE n.nspname = 'geoint_guard'`).Scan(&schemaOwned, &workerSchema, &apiSchema); err != nil {
		t.Fatal(err)
	}
	if !schemaOwned || workerSchema || apiSchema {
		t.Errorf("geoint_guard: owned by the owner %v (want true), worker/api privileges %v/%v (want none)", schemaOwned, workerSchema, apiSchema)
	}
	if !workerExec || !apiExec || workerInstall {
		t.Errorf("EXECUTE: worker/api on update_guard_columns_changed %v/%v (want true), worker on install_update_guard %v (want false)",
			workerExec, apiExec, workerInstall)
	}

	for _, table := range guardedTables {
		addColumn(t, env.owner, table, "extra text", false)
	}
	failures := updateGuardProblems(t, env.owner, updateGuardAllow, decidedColumns())
	for _, table := range guardedTables {
		want := "column " + table + ".extra is not decided"
		if !slices.ContainsFunc(failures, func(s string) bool { return strings.HasPrefix(s, want) }) {
			t.Errorf("an undecided %s.extra passes the catalogue check: %v", table, failures)
		}
	}
	for _, p := range failures {
		if !strings.Contains(p, ".extra is not decided") {
			t.Errorf("after ADD COLUMN, the event trigger left the guard stale: %s", p)
		}
	}

	// Deciding: extra immutable on relationship, allowed on company_version.
	f := newGuardFixture(t, env.owner)
	if _, err := env.owner.Exec(roleCtx, `SELECT install_update_guard('company_version',
		ARRAY['last_seen_collection_id', 'closed_collection_id', 'extra'])`); err != nil {
		t.Fatal(err)
	}
	decided := decidedColumns()
	decided["relationship"] = append(decided["relationship"], "extra")
	decided["company_version"] = append(decided["company_version"], "extra")
	allow := maps.Clone(updateGuardAllow)
	allow["company_version"] = append(slices.Clone(updateGuardAllow["company_version"]), "extra")
	for _, p := range updateGuardProblems(t, env.owner, allow, decided) {
		if !strings.HasPrefix(p, "column person_record.extra is not decided") {
			t.Errorf("after deciding relationship.extra and company_version.extra: %s", p)
		}
	}
	rel, cv := f.rows["relationship"][1].id, f.rows["company_version"][1].id
	if got := classify(t, env.owner, "relationship", fmt.Sprintf("UPDATE relationship SET extra = 'x' WHERE id = %d", rel)); got != outImmutable {
		t.Errorf("relationship.extra: %s, want %s", got, outImmutable)
	}
	if got := classify(t, env.owner, "company_version", fmt.Sprintf("UPDATE company_version SET extra = 'x' WHERE id = %d", cv)); got != outOK {
		t.Errorf("company_version.extra allow-listed: %s, want %s", got, outOK)
	}
	if got := classify(t, env.owner, "company_version", fmt.Sprintf("UPDATE company_version SET name = 'x' WHERE id = %d", cv)); got != outImmutable {
		t.Errorf("company_version.name after re-install: %s, want %s", got, outImmutable)
	}
	// A later ALTER keeps the decided allow-list.
	addColumn(t, env.owner, "company_version", "extra2 text", false)
	if g, _ := readGuardTrigger(t, env.owner, "company_version"); !slices.Equal(g.args, allow["company_version"]) {
		t.Errorf("after another ALTER, company_version_guard_update allows %v, want %v", g.args, allow["company_version"])
	}
	// Only the owner may install a guard.
	for _, p := range []*pgxpool.Pool{env.worker, env.api} {
		denied(t, p, `SELECT install_update_guard('relationship', ARRAY['src_entity_id'])`)
	}
}

// TestUpdateGuardNewColumnUnderAnOlderSnapshot: a REPEATABLE READ
// transaction whose snapshot was taken before the column was added cannot
// write it either (review of task 4, I1).
func TestUpdateGuardNewColumnUnderAnOlderSnapshot(t *testing.T) {
	env := newRoleEnv(t)
	f := newGuardFixture(t, env.owner)
	for _, table := range guardedTables {
		id := f.rows[table][0].id
		for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
			tx, err := env.worker.BeginTx(roleCtx, pgx.TxOptions{IsoLevel: iso})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(roleCtx, `SELECT 1`); err != nil { // the snapshot; relationship untouched
				t.Fatal(err)
			}
			col := "note_" + strings.ReplaceAll(string(iso), " ", "_")
			if _, err := env.owner.Exec(roleCtx, "ALTER TABLE "+table+" ADD COLUMN "+strings.ToLower(col)+" text"); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(roleCtx, fmt.Sprintf("UPDATE %s SET %s = 'x' WHERE id = %d", table, strings.ToLower(col), id))
			if got := outcomeOf(table, err); got != outImmutable {
				t.Errorf("%s, %s snapshot older than ADD COLUMN %s: %s, want %s", table, iso, col, got, outImmutable)
			}
			_ = tx.Rollback(roleCtx)
		}
	}
}

// TestInstallUpdateGuardKeepsEnabledState: re-installing a guard (directly
// or through the event trigger) keeps ENABLE ALWAYS / REPLICA / DISABLE.
func TestInstallUpdateGuardKeepsEnabledState(t *testing.T) {
	env := newRoleEnv(t)
	state := func() string {
		var s string
		if err := env.owner.QueryRow(roleCtx, `SELECT tgenabled::text FROM pg_trigger
			WHERE tgrelid = 'relationship'::regclass AND tgname = 'relationship_guard_update'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	install := `SELECT install_update_guard('relationship', ARRAY['src_entity_id', 'dst_entity_id',
		'last_seen_collection_id', 'closed_collection_id', 'valid_to_after', 'valid_to_before'])`
	for i, tc := range []struct{ alter, want string }{
		{"ENABLE ALWAYS", "A"}, {"ENABLE REPLICA", "R"}, {"DISABLE", "D"}, {"ENABLE", "O"},
	} {
		if _, err := env.owner.Exec(roleCtx, "ALTER TABLE relationship "+tc.alter+" TRIGGER relationship_guard_update"); err != nil {
			t.Fatal(err)
		}
		if got := state(); got != tc.want {
			t.Errorf("after %s (and the event trigger's re-install): %s, want %s", tc.alter, got, tc.want)
		}
		if _, err := env.owner.Exec(roleCtx, install); err != nil {
			t.Fatal(err)
		}
		if got := state(); got != tc.want {
			t.Errorf("%s then install_update_guard: %s, want %s", tc.alter, got, tc.want)
		}
		addColumn(t, env.owner, "relationship", fmt.Sprintf("c%d text", i), false)
		if got := state(); got != tc.want {
			t.Errorf("%s then ADD COLUMN: %s, want %s", tc.alter, got, tc.want)
		}
	}
}

// TestDropColumnCascadeKeepsTheGuard: DROP COLUMN … CASCADE of a column the
// guard compares would drop the guard with it; the event trigger refuses.
func TestDropColumnCascadeKeepsTheGuard(t *testing.T) {
	env := newRoleEnv(t)
	for _, table := range guardedTables {
		_, err := env.owner.Exec(roleCtx, "ALTER TABLE "+table+" DROP COLUMN record_ref CASCADE")
		if err == nil || !strings.Contains(err.Error(), "without its update guard") {
			t.Errorf("%s: DROP COLUMN record_ref CASCADE: %v", table, err)
		}
		if _, ok := readGuardTrigger(t, env.owner, table); !ok {
			t.Errorf("%s lost its guard", table)
		}
	}
	// A plain DROP COLUMN of a compared column is refused by the dependency.
	if _, err := env.owner.Exec(roleCtx, "ALTER TABLE relationship DROP COLUMN record_ref"); err == nil {
		t.Error("DROP COLUMN record_ref succeeded")
	}
	// The documented way to drop a column, in one transaction (a migration).
	addColumn(t, env.owner, "company_version", "extra text", false)
	tx, err := env.owner.Begin(roleCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(roleCtx) }()
	for _, sql := range []string{
		`SET LOCAL geoint.update_guard_installing = 'on'`,
		`ALTER TABLE company_version DROP COLUMN extra CASCADE`,
		`SELECT install_update_guard('company_version', ARRAY['last_seen_collection_id', 'closed_collection_id'])`,
	} {
		if _, err := tx.Exec(roleCtx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := tx.Commit(roleCtx); err != nil {
		t.Fatal(err)
	}
	for _, p := range updateGuardProblems(t, env.owner, updateGuardAllow, decidedColumns()) {
		t.Errorf("after dropping company_version.extra: %s", p)
	}
}

// TestTriggerFunctionsPinSearchPath: every trigger function, event trigger
// function and SECURITY DEFINER function of the schema (and of
// geoint_guard) runs with search_path = pg_catalog, pg_temp, so no name it
// uses can resolve to an object the worker created in the schema.
func TestTriggerFunctionsPinSearchPath(t *testing.T) {
	env := newRoleEnv(t)
	rows, err := env.owner.Query(roleCtx, `SELECT p.oid::regprocedure::text, coalesce(p.proconfig, '{}')
		FROM pg_proc p
		WHERE p.pronamespace IN (current_schema()::regnamespace, 'geoint_guard'::regnamespace)
			AND (p.prorettype IN ('trigger'::regtype, 'event_trigger'::regtype) OR p.prosecdef
				OR p.proname IN ('update_guard_columns_changed', 'install_update_guard'))
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	type fn struct {
		name   string
		config []string
	}
	fns, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (fn, error) {
		var f fn
		return f, r.Scan(&f.name, &f.config)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fns) < 9 {
		t.Fatalf("only %d functions found: %v", len(fns), fns)
	}
	want := []string{"search_path=pg_catalog, pg_temp"}
	for _, f := range fns {
		if !slices.Equal(f.config, want) {
			t.Errorf("%s has proconfig %q, want exactly %q (SET search_path = pg_catalog, pg_temp)", f.name, f.config, want)
		}
	}
}

// TestEventTriggerRunsOnlyTheOwnersInstall: in the state `goose down` of
// 0023 leaves (install_update_guard dropped, the 0001 triggers back, the
// event trigger still serving other schemas), the worker plants an
// install_update_guard(regclass, text[]) of its own; an ALTER TABLE by the
// owner (a superuser) must not run it, and must say so (review N1).
func TestEventTriggerRunsOnlyTheOwnersInstall(t *testing.T) {
	env := newRoleEnv(t)
	for _, sql := range []string{
		`DROP FUNCTION install_update_guard(regclass, text[])`,
		originalGuard("relationship"), originalGuard("person_record"), originalGuard("company_version"),
	} {
		if _, err := env.owner.Exec(roleCtx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// Without install_update_guard the event trigger leaves the schema alone.
	addColumn(t, env.owner, "person_record", "before_plant text", false)

	for _, sql := range []string{
		`CREATE SEQUENCE planted_calls`,
		// Schema-qualified: the caller's search_path may not reach it.
		`CREATE FUNCTION install_update_guard(tbl regclass, a text[]) RETURNS void LANGUAGE plpgsql AS
			$$BEGIN PERFORM nextval('` + env.schema + `.planted_calls'); END$$`,
	} {
		allowed(t, env.worker, sql)
	}
	for _, table := range guardedTables {
		_, err := env.owner.Exec(roleCtx, "ALTER TABLE "+table+" ADD COLUMN after_plant text")
		if err == nil || !strings.Contains(err.Error(), "is not run") || !strings.Contains(err.Error(), "geoint_worker") {
			t.Errorf("%s: ALTER TABLE with a planted install_update_guard: %v, want a refusal naming it", table, err)
		}
	}
	var called bool // a sequence is not rolled back with the ALTER
	if err := env.owner.QueryRow(roleCtx, `SELECT is_called FROM planted_calls`).Scan(&called); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("the worker's install_update_guard was executed by the owner's ALTER TABLE")
	}
}

// TestMigrationIgnoresAPlantedInstall: an install_update_guard(text, text[])
// the worker creates before 0023 is applied is not what the migration calls
// for its string literals.
func TestMigrationIgnoresAPlantedInstall(t *testing.T) {
	u := testutil.EmptySchemaURL(t)
	if err := db.MigrateTo(roleCtx, u, 22); err != nil {
		t.Fatalf("migrate to 22: %v", err)
	}
	owner, err := db.Open(roleCtx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	pw := map[string]string{db.WorkerRole: randomPassword(t), db.APIRole: randomPassword(t)}
	if err := db.SetRolePasswords(roleCtx, owner, pw); err != nil {
		t.Fatal(err)
	}
	worker := openAs(t, u, db.WorkerRole, pw[db.WorkerRole])
	var schema string
	if err := worker.QueryRow(roleCtx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE SEQUENCE planted_calls`,
		`CREATE FUNCTION install_update_guard(tbl text, a text[]) RETURNS void LANGUAGE plpgsql AS
			$$BEGIN PERFORM nextval('` + schema + `.planted_calls'); END$$`,
		`CREATE FUNCTION pg_advisory_xact_lock(integer) RETURNS void LANGUAGE plpgsql AS
			$$BEGIN PERFORM nextval('` + schema + `.planted_calls'); END$$`,
	} {
		allowed(t, worker, sql)
	}
	if err := db.Migrate(roleCtx, u); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var called bool
	if err := owner.QueryRow(roleCtx, `SELECT is_called FROM planted_calls`).Scan(&called); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("the migration executed a function the worker planted")
	}
	for _, p := range updateGuardProblems(t, owner, updateGuardAllow, decidedColumns()) {
		t.Error(p)
	}
}

// TestWorkerCannotShadowGuardFunctions: the worker, which may CREATE in the
// schema, defines exact-type functions that would make the guards compare
// nothing; the guards still refuse (review of task 4, P1). Without the
// pinned search_path the same functions switch the guard off, which the
// test shows first.
func TestWorkerCannotShadowGuardFunctions(t *testing.T) {
	env := newRoleEnv(t)
	f := newGuardFixture(t, env.owner)
	for _, sql := range []string{
		`CREATE FUNCTION to_jsonb(relationship) RETURNS jsonb LANGUAGE sql AS $$SELECT '{}'::jsonb$$`,
		`CREATE FUNCTION to_jsonb(entity) RETURNS jsonb LANGUAGE sql AS $$SELECT '{}'::jsonb$$`,
		`CREATE FUNCTION to_jsonb(company_version) RETURNS jsonb LANGUAGE sql AS $$SELECT '{}'::jsonb$$`,
		`CREATE FUNCTION arr_eq_true(text[], text[]) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true'`,
		`CREATE OPERATOR = (LEFTARG = text[], RIGHTARG = text[], FUNCTION = arr_eq_true)`,
	} {
		allowed(t, env.worker, sql)
	}
	rel := f.rows["relationship"][1].id
	forge := fmt.Sprintf("UPDATE relationship SET record_ref = 'forged' WHERE id = %d", rel)

	// The attack works on an unpinned guard_update (owner, rolled back).
	tx, err := env.owner.Begin(roleCtx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`ALTER FUNCTION guard_update() RESET search_path`, forge} {
		if _, err := tx.Exec(roleCtx, sql); err != nil {
			t.Errorf("unpinned: %s: %v (the attack should succeed, or the test proves nothing)", sql, err)
		}
	}
	_ = tx.Rollback(roleCtx)

	// Fresh sessions (plpgsql resolves names per session), every role.
	for _, p := range []*pgxpool.Pool{env.owner, env.worker} {
		p.Reset()
		if got := classify(t, p, "relationship", forge); got != outImmutable {
			t.Errorf("forged record_ref: %s, want %s", got, outImmutable)
		}
		_, err := p.Exec(roleCtx, `UPDATE entity SET name = 'forged' WHERE id = (SELECT min(id) FROM entity)`)
		if err == nil || !strings.Contains(err.Error(), "entity columns are immutable") {
			t.Errorf("forged entity name: %v", err)
		}
	}
	// The fallback (d) with an undecided column, under the worker's operator.
	addColumn(t, env.owner, "company_version", "extra text", true)
	env.worker.Reset()
	cv := f.rows["company_version"][1].id
	if got := classify(t, env.worker, "company_version", fmt.Sprintf("UPDATE company_version SET extra = 'x' WHERE id = %d", cv)); got != outImmutable {
		t.Errorf("undecided column under a shadowed text[] =: %s, want %s", got, outImmutable)
	}
}

// TestUpdateGuardEqualValuesWrittenDifferently: values equal under each
// type's equality but written differently (key order, numeric scale, the
// same instant, -0) and the loaders' statement shapes give the same outcome
// with the installed guard and with the original one.
func TestUpdateGuardEqualValuesWrittenDifferently(t *testing.T) {
	env := newRoleEnv(t)
	f := newGuardFixture(t, env.owner)
	var rel, cv int64
	if err := env.owner.QueryRow(roleCtx, `INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, attrs, kind,
			confidence, first_collection_id, record_ref, last_seen_collection_id)
		SELECT 'shareholder_of', e.id, e.id, '{"role": "a", "zz": 1}', 'inference', 0, $1, 'eq', $1
		FROM entity e WHERE e.kind = 'company' ORDER BY e.id LIMIT 1 RETURNING id`, f.coll[1]).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	cv = f.rows["company_version"][1].id // capital 10.00
	stmts := map[string][]string{
		"relationship": {
			`UPDATE relationship SET attrs = '{"zz": 1, "role": "a"}' WHERE id = {rel}`,
			`UPDATE relationship SET attrs = '{"zz":1.0,"role":"a"}' WHERE id = {rel}`,
			`UPDATE relationship SET confidence = '-0' WHERE id = {rel}`,
			`UPDATE relationship SET confidence = 0.0 WHERE id = {rel}`,
			`UPDATE relationship SET recorded_at = (recorded_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' WHERE id = {rel}`,
			`UPDATE relationship SET recorded_at = (recorded_at AT TIME ZONE 'America/Sao_Paulo') AT TIME ZONE 'America/Sao_Paulo' WHERE id = {rel}`,
			`UPDATE relationship SET valid_from = '2001-01-01'::timestamp::date WHERE id = {rel}`,
			`UPDATE relationship r SET last_seen_collection_id = v.c FROM (VALUES ({rel}, {c3})) v(id, c) WHERE r.id = v.id`,
			`UPDATE relationship r SET last_seen_collection_id = v.c, record_ref = r.record_ref || 'x' FROM (VALUES ({rel}, {c3})) v(id, c) WHERE r.id = v.id`,
			`UPDATE relationship r SET last_seen_collection_id = v.c FROM (VALUES ({rel}, {c1})) v(id, c) WHERE r.id = v.id`,
		},
		"company_version": {
			`UPDATE company_version SET capital = 10.0 WHERE id = {cv}`,
			`UPDATE company_version SET capital = 10.004 WHERE id = {cv}`,
			`UPDATE company_version SET capital = '1e1' WHERE id = {cv}`,
			`UPDATE company_version SET capital = 10.01 WHERE id = {cv}`,
			`UPDATE company_version SET name = 'EMPRESA'::varchar WHERE id = {cv}`,
			`INSERT INTO company_version (id, entity_id, name, first_collection_id, record_ref) SELECT id, entity_id, name,
				first_collection_id, record_ref FROM company_version WHERE id = {cv}
				ON CONFLICT (id) DO UPDATE SET last_seen_collection_id = {c3}`,
			`INSERT INTO company_version (id, entity_id, name, first_collection_id, record_ref) SELECT id, entity_id, 'OTHER',
				first_collection_id, record_ref FROM company_version WHERE id = {cv}
				ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`,
			`INSERT INTO company_version (id, entity_id, name, first_collection_id, record_ref) SELECT id, entity_id, name,
				first_collection_id, record_ref FROM company_version WHERE id = {cv}
				ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, last_seen_collection_id = {c1}`,
		},
	}
	repl := strings.NewReplacer("{rel}", fmt.Sprint(rel), "{cv}", fmt.Sprint(cv),
		"{c1}", fmt.Sprint(f.coll[1]), "{c3}", fmt.Sprint(f.coll[3]))
	for table, list := range stmts {
		run := func(original bool) []string {
			tx, err := env.owner.Begin(roleCtx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(roleCtx) }()
			if original {
				if _, err := tx.Exec(roleCtx, originalGuard(table)); err != nil {
					t.Fatal(err)
				}
			}
			var out []string
			for _, sql := range list {
				sp, err := tx.Begin(roleCtx)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, outcomeOf(table, execErr(sp, repl.Replace(sql))))
				_ = sp.Rollback(roleCtx)
			}
			return out
		}
		installed, original := run(false), run(true)
		for i, sql := range list {
			if installed[i] != original[i] {
				t.Errorf("%s\n  installed guard: %s, original guard: %s", repl.Replace(sql), installed[i], original[i])
			}
		}
		t.Logf("%s: %v", table, installed)
	}
}

// guardCalls runs sql in a transaction that is rolled back and returns how
// many times guard_update and update_guard_columns_changed were called
// (function statistics of the transaction; track_functions needs the owner,
// a superuser).
func guardCalls(t *testing.T, owner *pgxpool.Pool, sql string) (guard, layout int64) {
	t.Helper()
	tx, err := owner.Begin(roleCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(roleCtx) }()
	if _, err := tx.Exec(roleCtx, `SET LOCAL track_functions = 'pl'`); err != nil {
		t.Fatal(err)
	}
	// The counters also hold the calls of earlier transactions of the
	// session not yet reported: count the difference.
	counts := func() (g, l int64) {
		if err := tx.QueryRow(roleCtx, `SELECT coalesce(pg_stat_get_xact_function_calls('guard_update'::regproc), 0),
				coalesce(pg_stat_get_xact_function_calls('update_guard_columns_changed'::regproc), 0)`).Scan(&g, &l); err != nil {
			t.Fatal(err)
		}
		return g, l
	}
	g0, l0 := counts()
	if _, err := tx.Exec(roleCtx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	g1, l1 := counts()
	return g1 - g0, l1 - l0
}

// TestUpdateGuardFastPath: an update the guard would allow does not call
// it; the column check runs once per statement, not per row. After an
// unguarded column is added, every row goes through guard_update again.
func TestUpdateGuardFastPath(t *testing.T) {
	env := newRoleEnv(t)
	f := newGuardFixture(t, env.owner)
	c3 := f.coll[3]
	visited := map[string]bool{}
	for i, tc := range []struct {
		table, set string
		rows       int64
	}{
		{"relationship", fmt.Sprintf("last_seen_collection_id = %d", c3), 3},
		{"relationship", "src_entity_id = src_entity_id, dst_entity_id = dst_entity_id", 3},
		{"person_record", "entity_id = entity_id, identity_confidence = 1", 3},
		{"company_version", fmt.Sprintf("last_seen_collection_id = %d", c3), 3},
	} {
		sql := fmt.Sprintf("UPDATE %s SET %s", tc.table, tc.set)
		if !visited[tc.table] { // as migrated; a later visit follows the fallback below
			visited[tc.table] = true
			if guard, layout := guardCalls(t, env.owner, sql); guard != 0 || layout != 1 {
				t.Errorf("%s: guard_update called %d times, column check %d times; want 0 and 1", sql, guard, layout)
			}
		}
		// A column added through the event trigger (it also heals the
		// fallback of an earlier row): the fast path.
		addColumn(t, env.owner, tc.table, fmt.Sprintf("extra_%d text", i), false)
		if guard, layout := guardCalls(t, env.owner, sql); guard != 0 || layout != 1 {
			t.Errorf("%s after ADD COLUMN: guard_update called %d times, column check %d times; want 0 and 1", sql, guard, layout)
		}
		// A column added with the event trigger skipped: the fallback (d).
		addColumn(t, env.owner, tc.table, fmt.Sprintf("fallback_%d text", i), true)
		if guard, _ := guardCalls(t, env.owner, sql); guard != tc.rows {
			t.Errorf("%s with an unguarded column: guard_update called %d times, want %d (once per row)", sql, guard, tc.rows)
		}
	}
}
