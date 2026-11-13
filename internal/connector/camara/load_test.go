package camara

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/graph"
)

// The fixture (testdata/camara/README.md), legislatures 56 and 57. Ends are
// exclusive (valid_to_before: the first day the stretch no longer holds).
var wantMandates = map[string][]string{
	// mid-term party change; ended and current mandate
	"204379": {
		"56:PROS:AP:exercicio 2019-02-01..2022-04-11 closed",
		"56:MDB:AP:exercicio 2022-04-11..2023-02-01 closed",
		"57:MDB:AP:exercicio 2023-02-01.. open",
	},
	// resignation (definitive exit)
	"74159": {
		"56:PT:MG:exercicio 2019-02-01..2023-02-01 closed",
		"57:PT:MG:exercicio 2023-02-01..2026-05-19 closed",
	},
	// substitute in office
	"100689": {
		"57:REPUBLICANOS:GO:suplente_exercicio 2023-12-12..2024-04-11 closed",
	},
	// leave, party change during the leave, return; party change in the 57th
	"204511": {
		"56:DEM:SP:exercicio 2019-02-01..2021-11-11 closed",
		"56:DEM:SP:licenciado 2021-11-11..2022-02-23 closed",
		"56:UNIÃO:SP:licenciado 2022-02-23..2022-03-13 closed",
		"56:UNIÃO:SP:exercicio 2022-03-13..2023-02-01 closed",
		"57:UNIÃO:SP:exercicio 2023-02-01..2026-04-08 closed",
		"57:PODE:SP:exercicio 2026-04-08.. open",
	},
	// minister: on leave at the end of the 56th (no end event: closed at the
	// legislature's end) and again now (open licenciado)
	"204503": {
		"56:PT:SP:exercicio 2019-02-01..2023-01-01 closed",
		"56:PT:SP:licenciado 2023-01-01..2023-02-01 closed",
		"57:PT:SP:exercicio 2023-02-01..2023-02-02 closed",
		"57:PT:SP:licenciado 2023-02-02..2025-01-31 closed",
		"57:PT:SP:exercicio 2025-01-31..2025-02-03 closed",
		"57:PT:SP:licenciado 2025-02-03.. open",
	},
	// titular in the 56th; in the 57th the substitute in office while
	// 204503 is on leave
	"74283": {
		"56:PT:SP:exercicio 2019-02-01..2023-02-01 closed",
		"57:PT:SP:suplente_exercicio 2023-02-03..2025-01-31 closed",
		"57:PT:SP:suplente_exercicio 2025-02-03..2026-03-31 closed",
	},
}

func TestDeputyMandateLinkedToChamber(t *testing.T) {
	e := newEnv(t)
	e.collect(t, newRef())
	got := e.strings(t, `
		SELECT concat_ws('|', b.kind, b.natural_key, b.name, r.attrs ->> 'legislature', r.attrs ->> 'uf', r.attrs ->> 'party',
			r.attrs ->> 'status', r.attrs ->> 'role', r.kind, r.valid_from, r.closed_collection_id IS NULL, r.dst_entity_id = b.id,
			r.src_entity_id = p.entity_id)
		FROM relationship r
		JOIN person_record p ON p.id = r.src_record_id
		JOIN entity b ON b.id = r.dst_entity_id
		WHERE p.context_key = 'dep:204379' AND r.attrs ->> 'legislature' = '57'`)
	want := []string{"public_body|camara|Câmara dos Deputados|57|AP|MDB|exercicio|57:MDB:AP:exercicio|fact|2023-02-01|t|t|t"}
	if !slices.Equal(got, want) {
		t.Errorf("57th legislature mandate of 204379\n got %v\nwant %v", got, want)
	}
	// The record carries the civil name (nomeCivil) and only the CPF fragment.
	rec := e.strings(t, `SELECT concat_ws('|', name, name_norm, cpf_fragment, record_ref) FROM person_record WHERE context_key = 'dep:204379'`)
	if want := []string{"ACÁCIO DA SILVA FAVACHO NETO|ACACIO DA SILVA FAVACHO NETO|111111|deputado_204379.json:/dados"}; !slices.Equal(rec, want) {
		t.Errorf("person record\n got %v\nwant %v", rec, want)
	}
	for id, want := range wantMandates {
		if got := e.mandates(t, id); !slices.Equal(got, want) {
			t.Errorf("deputy %s\n got %v\nwant %v", id, got, want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship`); n != 21 {
		t.Errorf("%d relationships, want 21", n)
	}
	// Party entities carry the real name from the parties list.
	parties := e.strings(t, `SELECT natural_key || '=' || name FROM entity WHERE kind = 'party' ORDER BY 1`)
	if want := []string{"DEM=Democratas", "MDB=Movimento Democrático Brasileiro", "PODE=Podemos",
		"PROS=Partido Republicano da Ordem Social", "PT=Partido dos Trabalhadores", "REPUBLICANOS=Republicanos",
		"UNIÃO=União Brasil"}; !slices.Equal(parties, want) {
		t.Errorf("parties %v, want %v", parties, want)
	}
}

func TestEndedMandateHasExactEndDate(t *testing.T) {
	e := newEnv(t)
	col, _ := e.collect(t, newRef())
	got := e.strings(t, `
		SELECT concat_ws('|', r.valid_from, r.valid_to_after, r.valid_to_before, r.closed_collection_id = $1, r.last_seen_collection_id = $1)
		FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE p.context_key IN ('dep:74159', 'dep:100689') AND r.attrs ->> 'legislature' = '57' ORDER BY r.valid_from`, col.ID)
	want := []string{"2023-02-01|2026-05-19|2026-05-19|t|t", "2023-12-12|2024-04-11|2024-04-11|t|t"}
	// 74159 resigned on 2026-05-19 at 15:44: the day itself is the first
	// day without the seat. A term ending at 23:59 of its last day ends the
	// next day (no gap and no overlap with the next legislature's row).
	if !slices.Equal(got, want) {
		t.Errorf("ended mandates\n got %v\nwant %v", got, want)
	}
	// Open means current: 204379 and 204511 in office, 204503 on leave.
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NULL`); n != 3 {
		t.Errorf("%d open relationships, want 3", n)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NULL
		AND (valid_to_after IS NOT NULL OR valid_to_before IS NOT NULL)`); n != 0 {
		t.Errorf("%d open relationships with an end", n)
	}
}

func TestPartyChangeMidTermCreatesTwoRows(t *testing.T) {
	e := newEnv(t)
	// First collection: the API does not show the change of 2026-04-08 yet.
	var change map[string]any
	e.api.edit(t, "deputado_204511_historico.json", func(doc map[string]any) {
		var kept []any
		for _, ev := range doc["dados"].([]any) {
			m := ev.(map[string]any)
			if m["dataHora"] == "2026-04-08T19:30" {
				change = m
				continue
			}
			kept = append(kept, ev)
		}
		doc["dados"] = kept
	})
	if change == nil {
		t.Fatal("fixture has no party change on 2026-04-08")
	}
	first, _ := e.collect(t, newRef())
	if got, want := e.mandates(t, "204511")[4:], []string{"57:UNIÃO:SP:exercicio 2023-02-01.. open"}; !slices.Equal(got, want) {
		t.Fatalf("before the change\n got %v\nwant %v", got, want)
	}
	openID := e.count(t, `SELECT r.id FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE p.context_key = 'dep:204511' AND r.attrs ->> 'role' = '57:UNIÃO:SP:exercicio'`)

	e.api.edit(t, "deputado_204511_historico.json", func(doc map[string]any) {
		doc["dados"] = append(doc["dados"].([]any), change)
	})
	second, _ := e.collect(t, newRef())
	if got, want := e.mandates(t, "204511"), wantMandates["204511"]; !slices.Equal(got, want) {
		t.Fatalf("after the change\n got %v\nwant %v", got, want)
	}
	// The old row is the same claim, closed by the second collection at the
	// change date; the new one starts that day.
	got := e.strings(t, `SELECT concat_ws('|', id = $1, first_collection_id = $2, closed_collection_id = $3, valid_to_before)
		FROM relationship WHERE attrs ->> 'role' = '57:UNIÃO:SP:exercicio'`, openID, first.ID, second.ID)
	if want := []string{"t|t|t|2026-04-08"}; !slices.Equal(got, want) {
		t.Errorf("old row %v, want %v", got, want)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE attrs ->> 'role' = '57:PODE:SP:exercicio' AND first_collection_id = $1
		AND closed_collection_id IS NULL AND valid_from = '2026-04-08'`, second.ID); n != 1 {
		t.Errorf("%d new open PODE rows, want 1", n)
	}
}

func TestMalformedRowsAreSkippedAndCounted(t *testing.T) {
	e := newEnv(t)
	// A list item without id, and an event of 100689 with an impossible date.
	e.api.edit(t, "deputados_57_p2.json", func(doc map[string]any) {
		doc["dados"] = append(doc["dados"].([]any), map[string]any{"nome": "SEM ID", "siglaPartido": "PT", "siglaUf": "SP", "idLegislatura": 57})
	})
	e.api.edit(t, "deputado_100689_historico.json", func(doc map[string]any) {
		doc["dados"] = append(doc["dados"].([]any), map[string]any{"id": 100689, "idLegislatura": 57, "siglaPartido": "REPUBLICANOS",
			"siglaUf": "GO", "dataHora": "2024-13-45T00:00", "situacao": "Exercício", "condicaoEleitoral": "Suplente",
			"descricaoStatus": "Entrada - Reassunção"})
	})
	_, stats := e.collect(t, newRef())
	for k, want := range map[string]int64{"rows_read": 15, "rows_skipped": 2, "skipped_no_id": 1, "skipped_bad_date": 1} {
		if stats[k] != want {
			t.Errorf("stats[%s] = %d, want %d (stats %v)", k, stats[k], want, stats)
		}
	}
	if got := e.mandates(t, "100689"); len(got) != 0 {
		t.Errorf("skipped deputy has mandates %v", got)
	}
	if got, want := e.mandates(t, "204379"), wantMandates["204379"]; !slices.Equal(got, want) {
		t.Errorf("the rest must load\n got %v\nwant %v", got, want)
	}
}

func TestLoadFailsOnUnexpectedShape(t *testing.T) {
	e := newEnv(t)
	ref := newRef()
	files, err := e.c.Fetch(ctx, ref, e.raw)
	if err != nil {
		t.Fatal(err)
	}
	// A stored history that is not a list (Fetch checks it too; Load must
	// not trust that it ran).
	bad, err := e.raw.Put(ctx, "raw/camara/test-bad-shape/deputado_74159_historico.json", strings.NewReader(`{"dados": {"nao": "lista"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range files {
		if strings.HasSuffix(f.Key, "/deputado_74159_historico.json") {
			files[i] = bad
		}
	}
	col, err := collection.Begin(ctx, e.pool, sourceID, ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Load(ctx, col, files, e.raw, e.pool); err == nil || !strings.Contains(err.Error(), "deputado_74159_historico.json") {
		t.Fatalf("got %v, want a shape error naming the file", err)
	}
}

func TestLoadTwiceIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.collect(t, newRef())
	snapshot := func() []string {
		return e.strings(t, `SELECT concat_ws('|', r.id, r.rel_type, r.src_record_id, r.dst_entity_id, r.attrs::text, r.valid_from,
			r.valid_to_after, r.valid_to_before, r.closed_collection_id, r.first_collection_id, r.record_ref) FROM relationship r ORDER BY r.id`)
	}
	before := snapshot()
	records := e.strings(t, `SELECT concat_ws('|', id, context_key, name, cpf_fragment, entity_id) FROM person_record ORDER BY id`)
	entities := e.count(t, `SELECT count(*) FROM entity`)

	// The second response spells a name differently: the record is reused.
	e.api.edit(t, "deputado_204379.json", func(doc map[string]any) { doc["dados"].(map[string]any)["nomeCivil"] = "ACACIO FAVACHO" })
	second, _ := e.collect(t, newRef())
	if after := snapshot(); !slices.Equal(after, before) {
		t.Errorf("a second load changed the claims\nbefore %v\nafter  %v", before, after)
	}
	if after := e.strings(t, `SELECT concat_ws('|', id, context_key, name, cpf_fragment, entity_id) FROM person_record ORDER BY id`); !slices.Equal(after, records) {
		t.Errorf("person records changed\nbefore %v\nafter  %v", records, after)
	}
	if n := e.count(t, `SELECT count(*) FROM entity`); n != entities {
		t.Errorf("%d entities after the second load, want %d", n, entities)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE last_seen_collection_id <> $1`, second.ID); n != 0 {
		t.Errorf("%d claims not marked as seen by the second load", n)
	}
}

func TestEveryClaimHasProvenance(t *testing.T) {
	e := newEnv(t)
	e.collect(t, newRef())
	// The file part of record_ref (up to the last ':') is a collection_file
	// of the claim's first collection, as the provenance route resolves it.
	for _, table := range []string{"relationship", "person_record"} {
		if n := e.count(t, `SELECT count(*) FROM `+table+` c WHERE NOT EXISTS (SELECT 1 FROM collection_file f
			WHERE f.collection_id = c.first_collection_id
			AND f.name = left(c.record_ref, length(c.record_ref) - position(':' in reverse(c.record_ref))))`); n != 0 {
			t.Errorf("%d %s rows whose record_ref names no collected file", n, table)
		}
	}
	refs := e.strings(t, `SELECT record_ref FROM relationship UNION ALL SELECT record_ref FROM person_record`)
	re := regexp.MustCompile(`^deputado_[0-9]+(_historico)?\.json:/dados(/[0-9]+)?$`)
	for _, r := range refs {
		if !re.MatchString(r) {
			t.Errorf("record_ref %q is not <file>:<JSON pointer>", r)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM collection_file WHERE sha256 !~ '^[0-9a-f]{64}$' OR raw_key NOT LIKE 'raw/camara/%'`); n != 0 {
		t.Errorf("%d collection files without checksum or outside raw/camara", n)
	}
}

func TestOnlyTheCPFFragmentIsStored(t *testing.T) {
	e := newEnv(t)
	// Personal fields the connector must not load, set to recognisable
	// values in the served response, and a malformed item to make it log.
	seeded := []string{"pessoa.inventada@example.invalid", "1901-02-03", "Cidade Inventada", "+55 61 0000-0000"}
	e.api.edit(t, "deputado_204379.json", func(doc map[string]any) {
		d := doc["dados"].(map[string]any)
		d["dataNascimento"], d["municipioNascimento"] = seeded[1], seeded[2]
		st := d["ultimoStatus"].(map[string]any)
		st["email"] = seeded[0]
		st["gabinete"].(map[string]any)["telefone"] = seeded[3]
	})
	e.api.edit(t, "deputados_57_p2.json", func(doc map[string]any) {
		doc["dados"] = append(doc["dados"].([]any), map[string]any{"nome": "SEM ID", "email": seeded[0]})
	})
	logs := captureLogs(t)
	e.collect(t, newRef())
	frags := e.strings(t, `SELECT cpf_fragment FROM person_record ORDER BY context_key`)
	if want := []string{"333333", "111111", "555555", "444444", "222222", "666666"}; !slices.Equal(frags, want) {
		t.Errorf("fragments %v, want %v", frags, want)
	}
	// The fixture CPFs (900 + fragment + 2 digits) and the seeded values
	// appear nowhere in the database nor in the logs.
	full := regexp.MustCompile(`900[0-9]{8}`)
	var texts []string
	for _, q := range []string{
		`SELECT concat_ws('|', name, name_norm, cpf_fragment, record_ref, context_key) FROM person_record`,
		`SELECT concat_ws('|', attrs::text, record_ref) FROM relationship`,
		`SELECT concat_ws('|', name, natural_key) FROM entity`,
		`SELECT coalesce(stats::text, '') || coalesce(error, '') FROM collection`,
	} {
		texts = append(texts, e.strings(t, q)...)
	}
	texts = append(texts, logs.String())
	if !strings.Contains(logs.String(), "skipping list item") {
		t.Fatal("no log captured")
	}
	for _, s := range texts {
		if full.MatchString(s) {
			t.Errorf("a full CPF is stored or logged: %.80s", s)
		}
		for _, v := range seeded {
			if strings.Contains(s, v) {
				t.Errorf("a field that is not loaded (%q) is stored or logged", v)
			}
		}
	}
}

// heldOn lists, for the deputy, the roles of the holds_mandate edges the
// graph shows valid on day d.
func (e env) heldOn(t *testing.T, id, d string) []string {
	t.Helper()
	at, err := time.Parse(time.DateOnly, d)
	if err != nil {
		t.Fatal(err)
	}
	body := int64(e.count(t, `SELECT id FROM entity WHERE kind = 'public_body' AND natural_key = 'camara'`))
	person := int64(e.count(t, `SELECT entity_id FROM person_record WHERE context_key = $1`, "dep:"+id))
	edges, _, err := graph.New(e.pool).Neighbors(ctx, body, graph.Options{At: &at, RelTypes: []claims.RelType{claims.HoldsMandate}}, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ed := range edges {
		if ed.Src.ID == person {
			out = append(out, ed.Attrs["role"].(string))
		}
	}
	slices.Sort(out)
	return out
}

func TestSeatHolderOnBoundaryDays(t *testing.T) {
	e := newEnv(t)
	e.collect(t, newRef())
	for _, c := range []struct {
		id, day string
		want    []string
	}{
		// Last day of the 56th (term ends 23:59) and first of the 57th:
		// one row each day, no gap, no overlap.
		{"74159", "2023-01-31", []string{"56:PT:MG:exercicio"}},
		{"74159", "2023-02-01", []string{"57:PT:MG:exercicio"}},
		// Resigned on 2026-05-19: the day before is the last day held.
		{"74159", "2026-05-18", []string{"57:PT:MG:exercicio"}},
		{"74159", "2026-05-19", nil},
		// Titular -> leave -> return -> leave (still on leave: open row).
		{"204503", "2023-02-01", []string{"57:PT:SP:exercicio"}},
		{"204503", "2023-02-02", []string{"57:PT:SP:licenciado"}},
		{"204503", "2025-01-30", []string{"57:PT:SP:licenciado"}},
		{"204503", "2025-01-31", []string{"57:PT:SP:exercicio"}},
		{"204503", "2025-02-03", []string{"57:PT:SP:licenciado"}},
		{"204503", "2026-11-01", []string{"57:PT:SP:licenciado"}},
		// The substitute exercises while the titular is on leave.
		{"74283", "2024-06-01", []string{"57:PT:SP:suplente_exercicio"}},
		{"74283", "2025-01-31", nil},
		// Party change on 2026-04-08: the old party until the day before.
		{"204511", "2026-04-07", []string{"57:UNIÃO:SP:exercicio"}},
		{"204511", "2026-04-08", []string{"57:PODE:SP:exercicio"}},
	} {
		if got := e.heldOn(t, c.id, c.day); !slices.Equal(got, c.want) {
			t.Errorf("%s on %s: %v, want %v", c.id, c.day, got, c.want)
		}
	}
}

func TestUnknownLeaveCauseEndsTheStretch(t *testing.T) {
	e := newEnv(t)
	e.api.edit(t, "deputado_204503_historico.json", func(doc map[string]any) {
		for _, ev := range doc["dados"].([]any) {
			m := ev.(map[string]any)
			if m["dataHora"] == "2025-02-03T12:18" {
				m["descricaoStatus"] = "Saída - Afastamento sem prazo determinado - Motivo Inventado"
			}
		}
	})
	_, stats := e.collect(t, newRef())
	if stats["unknown_leave_cause"] != 1 {
		t.Errorf("unknown_leave_cause = %d, want 1", stats["unknown_leave_cause"])
	}
	// The exercise ends on that day; no licenciado is opened (the later
	// leave event finds no stretch to continue).
	got := e.mandates(t, "204503")[4:]
	if want := []string{"57:PT:SP:exercicio 2025-01-31..2025-02-03 closed"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// editEvents lets f change (or extend) the events of deputy id's history.
func (e env) editEvents(t *testing.T, id string, f func(evs []any) []any) {
	t.Helper()
	e.api.edit(t, "deputado_"+id+"_historico.json", func(doc map[string]any) { doc["dados"] = f(doc["dados"].([]any)) })
}

func eventAt(evs []any, at string) map[string]any {
	for _, ev := range evs {
		if m := ev.(map[string]any); m["dataHora"] == at {
			return m
		}
	}
	panic("no event at " + at)
}

func TestTermEndDoesNotDependOnTheTimeOfDay(t *testing.T) {
	e := newEnv(t)
	// Older histories record the end of the legislature at 00:00.
	e.editEvents(t, "74159", func(evs []any) []any {
		for _, ev := range evs {
			if m := ev.(map[string]any); m["dataHora"] == "2023-01-31T23:59" {
				m["dataHora"] = "2023-01-31T00:00"
			}
		}
		return evs
	})
	e.collect(t, newRef())
	if got, want := e.mandates(t, "74159"), wantMandates["74159"]; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (no gap day before the next legislature)", got, want)
	}
	if got := e.heldOn(t, "74159", "2023-01-31"); !slices.Equal(got, []string{"56:PT:MG:exercicio"}) {
		t.Errorf("last day of the 56th: %v", got)
	}
}

func TestDefinitiveExitDuringLeaveEndsIt(t *testing.T) {
	e := newEnv(t)
	e.editEvents(t, "204503", func(evs []any) []any {
		ev := map[string]any{}
		for k, v := range eventAt(evs, "2025-03-10T17:52") {
			ev[k] = v
		}
		ev["dataHora"], ev["situacao"] = "2026-06-01T10:00", "VACANCIA"
		ev["descricaoStatus"] = "Saída - Afastamento definitivo - Renúncia"
		return append(evs, ev)
	})
	e.collect(t, newRef())
	got := e.mandates(t, "204503")[5:]
	if want := []string{"57:PT:SP:licenciado 2025-02-03..2026-06-01 closed"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSubstituteEvents(t *testing.T) {
	e := newEnv(t)
	// 100689 leaves with an event that is not a leave description: counted
	// apart, not as an unknown leave cause.
	e.editEvents(t, "100689", func(evs []any) []any {
		eventAt(evs, "2024-04-11T00:00")["descricaoStatus"] = "DIVERSO - Alteração na Situação - Deixa a Condição de Licenciado (automático)"
		return evs
	})
	_, stats := e.collect(t, newRef())
	if stats["unknown_leave_cause"] != 0 || stats["substitute_exit_other"] != 1 {
		t.Errorf("unknown_leave_cause %d, substitute_exit_other %d; want 0 and 1", stats["unknown_leave_cause"], stats["substitute_exit_other"])
	}
	if got, want := e.mandates(t, "100689"), wantMandates["100689"]; !slices.Equal(got, want) {
		t.Errorf("100689: %v, want %v", got, want)
	}

	// "Posse como Titular" makes a substitute exercicio whatever the condição.
	e = newEnv(t)
	e.editEvents(t, "74283", func(evs []any) []any {
		eventAt(evs, "2025-02-03T16:30")["descricaoStatus"] = "Entrada - Posse de Suplente - Posse como Titular"
		return evs
	})
	e.collect(t, newRef())
	if got := e.mandates(t, "74283")[2:]; !slices.Equal(got, []string{"57:PT:SP:exercicio 2025-02-03..2026-03-31 closed"}) {
		t.Errorf("74283 as titular: %v", got)
	}
}

func TestPartyWithoutNameGetsNoEntity(t *testing.T) {
	e := newEnv(t)
	for _, f := range []string{"partidos_56_p1.json", "partidos_57_p1.json"} {
		e.api.edit(t, f, func(doc map[string]any) {
			var kept []any
			for _, p := range doc["dados"].([]any) {
				if p.(map[string]any)["sigla"] != "PODE" {
					kept = append(kept, p)
				}
			}
			doc["dados"] = kept
		})
	}
	_, stats := e.collect(t, newRef())
	if stats["party_without_name"] != 1 {
		t.Errorf("party_without_name = %d, want 1", stats["party_without_name"])
	}
	if n := e.count(t, `SELECT count(*) FROM entity WHERE kind = 'party' AND natural_key = 'PODE'`); n != 0 {
		t.Errorf("a party entity was created without a name")
	}
	if got, want := e.mandates(t, "204511"), wantMandates["204511"]; !slices.Equal(got, want) {
		t.Errorf("the mandate keeps the party attribute\n got %v\nwant %v", got, want)
	}
}

func TestUnavailableDeputyIsSkipped(t *testing.T) {
	e := newEnv(t)
	delete(e.api.files, "deputado_100689.json") // 404
	_, stats := e.collect(t, newRef())
	for k, want := range map[string]int64{"rows_read": 14, "rows_skipped": 1, "skipped_detail_unavailable": 1} {
		if stats[k] != want {
			t.Errorf("stats[%s] = %d, want %d", k, stats[k], want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key = 'dep:100689'`); n != 0 {
		t.Errorf("unavailable deputy written")
	}
	if got, want := e.mandates(t, "204379"), wantMandates["204379"]; !slices.Equal(got, want) {
		t.Errorf("the rest must load\n got %v\nwant %v", got, want)
	}
}

func TestNonFullScopeIsRejected(t *testing.T) {
	e := newEnv(t)
	col, err := collection.Begin(ctx, e.pool, sourceID, newRef(), collection.Scope{CNPJs: []string{"12345678"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Load(ctx, col, nil, e.raw, e.pool); !errors.Is(err, ErrScope) {
		t.Fatalf("got %v, want ErrScope", err)
	}
	if n := e.count(t, `SELECT count(*) FROM entity`); n != 0 {
		t.Errorf("%d entities written before the scope check", n)
	}
}
