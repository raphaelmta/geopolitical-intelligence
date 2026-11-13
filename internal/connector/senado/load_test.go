package senado

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

// The fixture (testdata/senado/README.md), legislatures 56 and 57. Ends are
// exclusive (valid_to_before: the first day the stretch no longer holds;
// the Senado publishes the last day, so it is stored + 1).
var wantMandates = map[string][]string{
	// titular, exercise ended by death
	"751": {"56:PSD:RJ:exercicio 2019-02-01..2020-10-22 closed"},
	// 8-year mandate cut at the 56th; party change; leave; ends without party
	"5322": {
		"56:PODE:RJ:exercicio 2019-02-01..2021-04-08 closed",
		"56:PL:RJ:exercicio 2021-04-08..2023-02-01 closed",
		"57:PL:RJ:exercicio 2023-02-01..2025-12-10 closed",
		"57:PL:RJ:licenciado 2025-12-10..2026-04-10 closed",
		"57:PL:RJ:exercicio 2026-04-10..2026-09-10 closed",
		"57::RJ:exercicio 2026-09-10.. open",
	},
	// titular, leave (LCS), return, mid-term party change
	"4642": {
		"57:UNIÃO:PB:exercicio 2023-02-01..2024-06-20 closed",
		"57:UNIÃO:PB:licenciado 2024-06-20..2024-10-18 closed",
		"57:UNIÃO:PB:exercicio 2024-10-18..2026-03-30 closed",
		"57:PL:PB:exercicio 2026-03-30.. open",
	},
	// 4642's substitute during the leave
	"6366": {"57:UNIÃO:PB:suplente_exercicio 2024-06-20..2024-10-18 closed"},
	// minister: repeated leaves (AFO), on leave now
	"5016": {
		"57:PT:PI:exercicio 2023-02-01..2023-02-04 closed",
		"57:PT:PI:licenciado 2023-02-04..2023-12-13 closed",
		"57:PT:PI:exercicio 2023-12-13..2023-12-14 closed",
		"57:PT:PI:licenciado 2023-12-14..2025-02-01 closed",
		"57:PT:PI:exercicio 2025-02-01..2025-02-03 closed",
		"57:PT:PI:licenciado 2025-02-03..2026-04-28 closed",
		"57:PT:PI:exercicio 2026-04-28..2026-05-07 closed",
		"57:PT:PI:licenciado 2026-05-07.. open",
	},
	// titular on leave as minister (AFO), back on 2024-02-01 only to resign
	// (REN): the leave is bounded, nothing stays open
	"4605": {
		"57:PSB:MA:exercicio 2023-02-01..2023-02-03 closed",
		"57:PSB:MA:licenciado 2023-02-03..2024-02-01 closed",
		"57:PSB:MA:exercicio 2024-02-01..2024-02-21 closed",
	},
	// 4605's 1st substitute, effective after the resignation (the API keeps
	// her as "1º Suplente")
	"6358": {
		"57:PSB:MA:suplente_exercicio 2023-02-02..2024-02-01 closed",
		"57:PSB:MA:suplente_exercicio 2024-02-21..2024-04-24 closed",
		"57:PDT:MA:suplente_exercicio 2024-04-24..2026-03-04 closed",
		"57:PSB:MA:suplente_exercicio 2026-03-04..2026-07-31 closed",
		"57:PSB:MA:suplente_exercicio 2026-10-06.. open",
	},
	"6359": {"57:PSB:MA:suplente_exercicio 2026-08-05..2026-10-07 closed"},
	// 5016's substitute
	"6369": {
		"57:PSD:PI:suplente_exercicio 2023-02-06..2023-12-13 closed",
		"57:PSD:PI:suplente_exercicio 2023-12-14..2025-02-02 closed",
		"57:PSD:PI:suplente_exercicio 2025-02-03..2026-04-29 closed",
		"57:PSD:PI:suplente_exercicio 2026-05-06.. open",
	},
}

func TestSenatorMandateLinkedToSenate(t *testing.T) {
	e := newEnv(t)
	e.collect(t, newRef())
	got := e.strings(t, `
		SELECT concat_ws('|', b.kind, b.natural_key, b.name, r.attrs ->> 'legislature', r.attrs ->> 'uf', r.attrs ->> 'party',
			r.attrs ->> 'status', r.attrs ->> 'role', r.kind, r.valid_from, r.closed_collection_id IS NULL, r.src_entity_id = p.entity_id)
		FROM relationship r
		JOIN person_record p ON p.id = r.src_record_id
		JOIN entity b ON b.id = r.dst_entity_id
		WHERE p.context_key = 'sen:4642' AND r.closed_collection_id IS NULL`)
	want := []string{"public_body|senado|Senado Federal|57|PB|PL|exercicio|57:PL:PB:exercicio|fact|2026-03-30|t|t"}
	if !slices.Equal(got, want) {
		t.Errorf("current mandate of 4642\n got %v\nwant %v", got, want)
	}
	// Civil name (NomeCompletoParlamentar); the Senado publishes no CPF.
	rec := e.strings(t, `SELECT concat_ws('|', name, cpf_fragment, record_ref) FROM person_record WHERE context_key = 'sen:4642'`)
	if want := []string{"Efraim de Araújo Morais Filho||senadores_57.json:/ListaParlamentarLegislatura/Parlamentares/Parlamentar/4/IdentificacaoParlamentar"}; !slices.Equal(rec, want) {
		t.Errorf("person record\n got %v\nwant %v", rec, want)
	}
	for id, want := range wantMandates {
		if got := e.mandates(t, id); !slices.Equal(got, want) {
			t.Errorf("senator %s\n got %v\nwant %v", id, got, want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM relationship`); n != 33 {
		t.Errorf("%d relationships, want 33", n)
	}
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key = 'sen:5918'`); n != 0 {
		t.Errorf("a substitute never in office has a person record")
	}
	// The party name comes from the affiliations.
	if got := e.strings(t, `SELECT name FROM entity WHERE kind = 'party' AND natural_key = 'PSD'`); !slices.Equal(got, []string{"Partido Social Democrático"}) {
		t.Errorf("PSD entity name %v", got)
	}
}

func TestEndedMandateHasExactEndDate(t *testing.T) {
	e := newEnv(t)
	col, _ := e.collect(t, newRef())
	got := e.strings(t, `
		SELECT concat_ws('|', r.valid_from, r.valid_to_after, r.valid_to_before, r.closed_collection_id = $1)
		FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE p.context_key IN ('sen:751', 'sen:6366') ORDER BY r.valid_from`, col.ID)
	// Published last days 2020-10-21 and 2024-10-17: stored exclusive.
	want := []string{"2019-02-01|2020-10-22|2020-10-22|t", "2024-06-20|2024-10-18|2024-10-18|t"}
	if !slices.Equal(got, want) {
		t.Errorf("ended mandates\n got %v\nwant %v", got, want)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NULL`); n != 5 {
		t.Errorf("%d open relationships, want 5", n)
	}
	if n := e.count(t, `SELECT count(*) FROM relationship WHERE closed_collection_id IS NULL
		AND (valid_to_after IS NOT NULL OR valid_to_before IS NOT NULL)`); n != 0 {
		t.Errorf("%d open relationships with an end", n)
	}
}

func TestPartyChangeMidTermCreatesTwoRows(t *testing.T) {
	e := newEnv(t)
	// First collection: the affiliation to PL (2026-03-30) is not published yet.
	var pl any
	e.api.edit(t, "senador_4642_filiacoes.json", func(doc map[string]any) {
		fs := doc["FiliacaoParlamentar"].(map[string]any)["Parlamentar"].(map[string]any)["Filiacoes"].(map[string]any)
		list := fs["Filiacao"].([]any)
		pl = list[0]
		union := list[1].(map[string]any)
		delete(union, "DataDesfiliacao")
		fs["Filiacao"] = list[1:]
	})
	first, _ := e.collect(t, newRef())
	if got, want := e.mandates(t, "4642")[2:], []string{"57:UNIÃO:PB:exercicio 2024-10-18.. open"}; !slices.Equal(got, want) {
		t.Fatalf("before the change\n got %v\nwant %v", got, want)
	}
	openID := e.count(t, `SELECT r.id FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE p.context_key = 'sen:4642' AND r.closed_collection_id IS NULL`)

	e.api.edit(t, "senador_4642_filiacoes.json", func(doc map[string]any) {
		fs := doc["FiliacaoParlamentar"].(map[string]any)["Parlamentar"].(map[string]any)["Filiacoes"].(map[string]any)
		list := fs["Filiacao"].([]any)
		list[0].(map[string]any)["DataDesfiliacao"] = "2026-03-29"
		fs["Filiacao"] = append([]any{pl}, list...)
	})
	second, _ := e.collect(t, newRef())
	if got, want := e.mandates(t, "4642"), wantMandates["4642"]; !slices.Equal(got, want) {
		t.Fatalf("after the change\n got %v\nwant %v", got, want)
	}
	got := e.strings(t, `SELECT concat_ws('|', first_collection_id = $2, closed_collection_id = $3, valid_to_before)
		FROM relationship WHERE id = $1`, openID, first.ID, second.ID)
	if want := []string{"t|t|2026-03-30"}; !slices.Equal(got, want) {
		t.Errorf("old row %v, want %v (same claim, closed at the change date)", got, want)
	}
}

func TestMalformedRowsAreSkippedAndCounted(t *testing.T) {
	e := newEnv(t)
	e.api.edit(t, "senadores_57.json", func(doc map[string]any) {
		ps := doc["ListaParlamentarLegislatura"].(map[string]any)["Parlamentares"].(map[string]any)
		list := ps["Parlamentar"].([]any)
		// A senator without code, and 6366 with an impossible exercise date.
		list = append(list, map[string]any{"IdentificacaoParlamentar": map[string]any{"NomeCompletoParlamentar": "SEM CODIGO"}})
		for _, p := range list {
			m := p.(map[string]any)
			id := m["IdentificacaoParlamentar"].(map[string]any)
			if id["CodigoParlamentar"] == "6366" {
				mand := m["Mandatos"].(map[string]any)["Mandato"].([]any)[0].(map[string]any)
				ex := mand["Exercicios"].(map[string]any)["Exercicio"].([]any)[0].(map[string]any)
				ex["DataFim"] = "2024-13-45"
			}
		}
		ps["Parlamentar"] = list
	})
	_, stats := e.collect(t, newRef())
	for k, want := range map[string]int64{"rows_read": 14, "rows_skipped": 2, "skipped_no_code": 1, "skipped_bad_date": 1, "never_in_office": 2} {
		if stats[k] != want {
			t.Errorf("stats[%s] = %d, want %d (stats %v)", k, stats[k], want, stats)
		}
	}
	if got := e.mandates(t, "6366"); len(got) != 0 {
		t.Errorf("skipped senator has mandates %v", got)
	}
	if got, want := e.mandates(t, "4642"), wantMandates["4642"]; !slices.Equal(got, want) {
		t.Errorf("the rest must load\n got %v\nwant %v", got, want)
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

	// A later response spells a name differently: the record is reused.
	e.api.edit(t, "senadores_57.json", func(doc map[string]any) {
		for _, p := range doc["ListaParlamentarLegislatura"].(map[string]any)["Parlamentares"].(map[string]any)["Parlamentar"].([]any) {
			id := p.(map[string]any)["IdentificacaoParlamentar"].(map[string]any)
			if id["CodigoParlamentar"] == "4642" {
				id["NomeCompletoParlamentar"] = "EFRAIM FILHO"
			}
		}
	})
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
	for _, table := range []string{"relationship", "person_record"} {
		if n := e.count(t, `SELECT count(*) FROM `+table+` c WHERE NOT EXISTS (SELECT 1 FROM collection_file f
			WHERE f.collection_id = c.first_collection_id
			AND f.name = left(c.record_ref, length(c.record_ref) - position(':' in reverse(c.record_ref))))`); n != 0 {
			t.Errorf("%d %s rows whose record_ref names no collected file", n, table)
		}
	}
	re := regexp.MustCompile(`^senadores_5[67]\.json:/ListaParlamentarLegislatura/Parlamentares/Parlamentar/[0-9]+/` +
		`(IdentificacaoParlamentar|Mandatos/Mandato/[0-9]+/Exercicios/Exercicio/[0-9]+)$`)
	for _, r := range e.strings(t, `SELECT record_ref FROM relationship UNION ALL SELECT record_ref FROM person_record`) {
		if !re.MatchString(r) {
			t.Errorf("record_ref %q is not <file>:<JSON pointer>", r)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM collection_file WHERE sha256 !~ '^[0-9a-f]{64}$' OR raw_key NOT LIKE 'raw/senado/%'`); n != 0 {
		t.Errorf("%d collection files without checksum or outside raw/senado", n)
	}
}

func TestLoadFailsWithoutTheAffiliations(t *testing.T) {
	e := newEnv(t)
	ref := newRef()
	files, err := e.c.Fetch(ctx, ref, e.raw)
	if err != nil {
		t.Fatal(err)
	}
	var kept = files[:0]
	for _, f := range files {
		if !strings.HasSuffix(f.Key, "/senador_4642_filiacoes.json") {
			kept = append(kept, f)
		}
	}
	col, err := collection.Begin(ctx, e.pool, sourceID, ref, collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Load(ctx, col, kept, e.raw, e.pool); err == nil || !strings.Contains(err.Error(), "senador_4642_filiacoes.json") {
		t.Fatalf("got %v, want an error naming the missing file", err)
	}
}

// heldOn lists, for the senator, the roles of the holds_mandate edges the
// graph shows valid on day d.
func (e env) heldOn(t *testing.T, code, d string) []string {
	t.Helper()
	at, err := time.Parse(time.DateOnly, d)
	if err != nil {
		t.Fatal(err)
	}
	body := int64(e.count(t, `SELECT id FROM entity WHERE kind = 'public_body' AND natural_key = 'senado'`))
	person := int64(e.count(t, `SELECT entity_id FROM person_record WHERE context_key = $1`, "sen:"+code))
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
		code, day string
		want      []string
	}{
		// Death on 2020-10-21 (published last day): held that day, not the next.
		{"751", "2020-10-21", []string{"56:PSD:RJ:exercicio"}},
		{"751", "2020-10-22", nil},
		// Leave from the day after the last day in exercise (2024-06-19)
		// until the return (2024-10-18): no gap, no overlap.
		{"4642", "2024-06-19", []string{"57:UNIÃO:PB:exercicio"}},
		{"4642", "2024-06-20", []string{"57:UNIÃO:PB:licenciado"}},
		{"4642", "2024-10-17", []string{"57:UNIÃO:PB:licenciado"}},
		{"4642", "2024-10-18", []string{"57:UNIÃO:PB:exercicio"}},
		// The substitute in office during the titular's leave.
		{"6366", "2024-06-20", []string{"57:UNIÃO:PB:suplente_exercicio"}},
		{"6366", "2024-10-17", []string{"57:UNIÃO:PB:suplente_exercicio"}},
		{"6366", "2024-10-18", nil},
		// Party: UNIÃO until 2026-03-29 (last day), PL from 2026-03-30.
		{"4642", "2026-03-29", []string{"57:UNIÃO:PB:exercicio"}},
		{"4642", "2026-03-30", []string{"57:PL:PB:exercicio"}},
		// Mandate across two legislatures: last day of the 56th, first of the 57th.
		{"5322", "2023-01-31", []string{"56:PL:RJ:exercicio"}},
		{"5322", "2023-02-01", []string{"57:PL:RJ:exercicio"}},
		// A minister on leave now: open licenciado.
		{"5016", "2026-11-01", []string{"57:PT:PI:licenciado"}},
	} {
		if got := e.heldOn(t, c.code, c.day); !slices.Equal(got, c.want) {
			t.Errorf("%s on %s: %v, want %v", c.code, c.day, got, c.want)
		}
	}
}

func TestUnknownLeaveCauseEndsTheStretch(t *testing.T) {
	e := newEnv(t)
	e.api.edit(t, "senadores_57.json", func(doc map[string]any) {
		for _, p := range doc["ListaParlamentarLegislatura"].(map[string]any)["Parlamentares"].(map[string]any)["Parlamentar"].([]any) {
			m := p.(map[string]any)
			if m["IdentificacaoParlamentar"].(map[string]any)["CodigoParlamentar"] != "4642" {
				continue
			}
			for _, ex := range m["Mandatos"].(map[string]any)["Mandato"].([]any)[0].(map[string]any)["Exercicios"].(map[string]any)["Exercicio"].([]any) {
				if x := ex.(map[string]any); x["SiglaCausaAfastamento"] == "LCS" {
					x["SiglaCausaAfastamento"] = "XYZ"
				}
			}
		}
	})
	_, stats := e.collect(t, newRef())
	if stats["unknown_leave_cause"] != 1 {
		t.Errorf("unknown_leave_cause = %d, want 1", stats["unknown_leave_cause"])
	}
	want := []string{
		"57:UNIÃO:PB:exercicio 2023-02-01..2024-06-20 closed",
		"57:UNIÃO:PB:exercicio 2024-10-18..2026-03-30 closed",
		"57:PL:PB:exercicio 2026-03-30.. open",
	}
	if got := e.mandates(t, "4642"); !slices.Equal(got, want) {
		t.Errorf("no licenciado for an unknown cause\n got %v\nwant %v", got, want)
	}
}

func TestUnavailableAffiliationsSkipTheSenator(t *testing.T) {
	e := newEnv(t)
	delete(e.api.files, "senador_6366_filiacoes.json") // 404
	_, stats := e.collect(t, newRef())
	for k, want := range map[string]int64{"rows_read": 13, "rows_skipped": 1, "skipped_detail_unavailable": 1} {
		if stats[k] != want {
			t.Errorf("stats[%s] = %d, want %d", k, stats[k], want)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM person_record WHERE context_key = 'sen:6366'`); n != 0 {
		t.Errorf("unavailable senator written")
	}
	if got, want := e.mandates(t, "4642"), wantMandates["4642"]; !slices.Equal(got, want) {
		t.Errorf("the rest must load\n got %v\nwant %v", got, want)
	}
}

func TestPersonalFieldsAreNotLoadedNorLogged(t *testing.T) {
	e := newEnv(t)
	seeded := []string{"pessoa.inventada@example.invalid", "https://example.invalid/foto.jpg", "Feminino-inventado"}
	e.api.edit(t, "senadores_57.json", func(doc map[string]any) {
		list := doc["ListaParlamentarLegislatura"].(map[string]any)["Parlamentares"].(map[string]any)
		for _, p := range list["Parlamentar"].([]any) {
			id := p.(map[string]any)["IdentificacaoParlamentar"].(map[string]any)
			id["EmailParlamentar"], id["UrlFotoParlamentar"], id["SexoParlamentar"] = seeded[0], seeded[1], seeded[2]
		}
		list["Parlamentar"] = append(list["Parlamentar"].([]any), map[string]any{"IdentificacaoParlamentar": map[string]any{"EmailParlamentar": seeded[0]}})
	})
	logs := captureLogs(t)
	e.collect(t, newRef())
	if !strings.Contains(logs.String(), "skipping senator") {
		t.Fatal("no log captured")
	}
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
	for _, s := range texts {
		for _, v := range seeded {
			if strings.Contains(s, v) {
				t.Errorf("a field that is not loaded (%q) is stored or logged", v)
			}
		}
	}
}

// editExercises lets f change the exercises of senator code's first
// mandate in list file.
func (e env) editExercises(t *testing.T, file, code string, f func([]any) []any) {
	t.Helper()
	e.api.edit(t, file, func(doc map[string]any) {
		for _, p := range doc["ListaParlamentarLegislatura"].(map[string]any)["Parlamentares"].(map[string]any)["Parlamentar"].([]any) {
			m := p.(map[string]any)
			if m["IdentificacaoParlamentar"].(map[string]any)["CodigoParlamentar"] != code {
				continue
			}
			ex := m["Mandatos"].(map[string]any)["Mandato"].([]any)[0].(map[string]any)["Exercicios"].(map[string]any)
			ex["Exercicio"] = f(ex["Exercicio"].([]any))
		}
	})
}

func TestDefinitiveExitAfterLeaveLeavesNothingOpen(t *testing.T) {
	e := newEnv(t)
	e.collect(t, newRef())
	if n := e.count(t, `SELECT count(*) FROM relationship r JOIN person_record p ON p.id = r.src_record_id
		WHERE p.context_key = 'sen:4605' AND r.closed_collection_id IS NULL`); n != 0 {
		t.Errorf("%d open rows for a titular who resigned", n)
	}
	if got := e.heldOn(t, "4605", "2024-02-20"); !slices.Equal(got, []string{"57:PSB:MA:exercicio"}) {
		t.Errorf("last day before the resignation: %v", got)
	}
	if got := e.heldOn(t, "4605", "2024-02-21"); got != nil {
		t.Errorf("after the resignation: %v", got)
	}
}

func TestLeaveWithoutReturnNeedsASubstitute(t *testing.T) {
	// Corroborated (the fixture): 6369 in exercise since 2026-05-06 -> open.
	e := newEnv(t)
	e.collect(t, newRef())
	if got := e.mandates(t, "5016")[7:]; !slices.Equal(got, []string{"57:PT:PI:licenciado 2026-05-07.. open"}) {
		t.Fatalf("corroborated leave: %v", got)
	}

	// The substitute's exercise ended: the leave ends with it.
	e = newEnv(t)
	e.editExercises(t, "senadores_57.json", "6369", func(ex []any) []any {
		for _, x := range ex {
			if m := x.(map[string]any); m["DataInicio"] == "2026-05-06" {
				m["DataFim"], m["SiglaCausaAfastamento"] = "2026-09-30", "RET"
			}
		}
		return ex
	})
	_, stats := e.collect(t, newRef())
	if got := e.mandates(t, "5016")[7:]; !slices.Equal(got, []string{"57:PT:PI:licenciado 2026-05-07..2026-10-01 closed"}) {
		t.Errorf("leave bounded by the substitute: %v", got)
	}
	if stats["leave_bounded_by_substitute"] != 1 {
		t.Errorf("leave_bounded_by_substitute = %d", stats["leave_bounded_by_substitute"])
	}

	// No substitute exercise since the titular's last one: not written.
	e = newEnv(t)
	e.editExercises(t, "senadores_57.json", "6369", func(ex []any) []any {
		var kept []any
		for _, x := range ex {
			if x.(map[string]any)["DataInicio"] != "2026-05-06" {
				kept = append(kept, x)
			}
		}
		return kept
	})
	_, stats = e.collect(t, newRef())
	got := e.mandates(t, "5016")
	if len(got) != 7 || got[6] != "57:PT:PI:exercicio 2026-04-28..2026-05-07 closed" {
		t.Errorf("uncorroborated leave written: %v", got)
	}
	if stats["leave_without_return"] != 1 {
		t.Errorf("leave_without_return = %d", stats["leave_without_return"])
	}
}

// The real shape: substitutes follow each other with a gap of a few days.
// A substitute whose exercise ended 5 days before the collection, without a
// terminal cause, keeps the titular's leave open (the next substitute may
// take office any day); 40 days after, it is bounded by that end.
func TestLeaveStaysOpenWithinTheSubstituteGap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ago      int // days before the collection date the exercise ended (exclusive end)
		cause    string
		open     bool
		statOpen int64
	}{
		{"5-day gap, no terminal cause", 5, "LCS", true, 1},
		{"40 days, no terminal cause", 40, "LCS", false, 0},
		{"5-day gap, titular returned", 5, "RET", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ref := newRef()
			last := ref.AddDate(0, 0, -tc.ago-1) // DataFim is the last day in exercise
			e.editExercises(t, "senadores_57.json", "6369", func(ex []any) []any {
				for _, x := range ex {
					if m := x.(map[string]any); m["DataInicio"] == "2026-05-06" {
						m["DataFim"], m["SiglaCausaAfastamento"] = last.Format(time.DateOnly), tc.cause
					}
				}
				return ex
			})
			_, stats := e.collect(t, ref)
			got := e.mandates(t, "5016")[7:]
			want := "57:PT:PI:licenciado 2026-05-07.. open"
			if !tc.open {
				want = "57:PT:PI:licenciado 2026-05-07.." + last.AddDate(0, 0, 1).Format(time.DateOnly) + " closed"
			}
			if !slices.Equal(got, []string{want}) {
				t.Errorf("leave: %v, want %s", got, want)
			}
			if stats["leave_open_within_substitute_gap"] != tc.statOpen {
				t.Errorf("leave_open_within_substitute_gap = %d, want %d", stats["leave_open_within_substitute_gap"], tc.statOpen)
			}
		})
	}
}

func TestLeaveWithoutReturnAcrossLegislatures(t *testing.T) {
	// 751's mandate spans the 56th and 57th. Here he goes on leave in 2020
	// and never comes back (no substitute exercise in the lists): nothing
	// is written for the leave, in either legislature.
	e := newEnv(t)
	for _, f := range []string{"senadores_56.json", "senadores_57.json"} {
		e.editExercises(t, f, "751", func(ex []any) []any {
			m := ex[0].(map[string]any)
			m["DataFim"], m["SiglaCausaAfastamento"] = "2020-06-01", "LCS"
			return ex
		})
	}
	_, stats := e.collect(t, newRef())
	if got, want := e.mandates(t, "751"), []string{"56:PSD:RJ:exercicio 2019-02-01..2020-06-02 closed"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if stats["leave_without_return"] != 1 {
		t.Errorf("leave_without_return = %d, want 1 (counted in the legislature where the leave starts)", stats["leave_without_return"])
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
