package jobs

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/collection"
	"geoint/internal/graph"
	"geoint/internal/testutil"
)

func TestValidateParams(t *testing.T) {
	cases := []struct {
		name, typ, params, wantErr string // wantErr "" means valid
	}{
		{"paths minimal", "paths", `{"a":1,"b":2}`, ""},
		{"paths full", "paths", `{"a":1,"b":2,"max_depth":6,"rel_types":["partner_of","donated_to"],"at":"2024-02-29"}`, ""},
		{"paths depth 0 is default", "paths", `{"a":1,"b":2,"max_depth":0}`, ""},
		{"paths missing a", "paths", `{"b":2}`, "a must be"},
		{"paths zero b", "paths", `{"a":1,"b":0}`, "b must be"},
		{"paths negative a", "paths", `{"a":-1,"b":2}`, "a must be"},
		{"paths depth 7", "paths", `{"a":1,"b":2,"max_depth":7}`, "max_depth"},
		{"paths depth negative", "paths", `{"a":1,"b":2,"max_depth":-1}`, "max_depth"},
		{"paths unknown field", "paths", `{"a":1,"b":2,"hub_degree":3}`, "unknown field"},
		{"paths unknown rel", "paths", `{"a":1,"b":2,"rel_types":["friend_of"]}`, "rel_types"},
		{"paths bad date", "paths", `{"a":1,"b":2,"at":"29/02/2024"}`, "at must be"},
		{"paths impossible date", "paths", `{"a":1,"b":2,"at":"2023-02-29"}`, "at must be"},
		{"paths float id", "paths", `{"a":1.5,"b":2}`, "invalid"},
		{"paths string id", "paths", `{"a":"1","b":2}`, "invalid"},
		{"paths trailing data", "paths", `{"a":1,"b":2} {}`, "invalid"},
		{"paths not object", "paths", `[1,2]`, "invalid"},
		{"paths empty", "paths", ``, "invalid"},
		{"control documented", "control_group", `{"company_id":9,"mode":"documented"}`, ""},
		{"control corporate", "control_group", `{"company_id":9,"mode":"corporate_links"}`, ""},
		{"control missing company", "control_group", `{"mode":"documented"}`, "company_id must be"},
		{"control bad mode", "control_group", `{"company_id":9,"mode":"all"}`, "mode must be"},
		{"control missing mode", "control_group", `{"company_id":9}`, "mode must be"},
		{"control unknown field", "control_group", `{"company_id":9,"mode":"documented","x":1}`, "unknown field"},
		{"paths duplicate key", "paths", `{"a":1,"b":2,"a":3}`, "duplicate key"},
		{"paths keys differing by case", "paths", `{"a":1,"A":1,"b":2}`, "duplicate key"},
		{"paths key in another case", "paths", `{"A":1,"b":2}`, "must be written"},
		{"paths rel_types in another case", "paths", `{"a":1,"b":2,"Rel_Types":["partner_of"]}`, "must be written"},
		{"control duplicate mode", "control_group", `{"company_id":9,"mode":"documented","mode":"corporate_links"}`, "duplicate key"},
		{"control key in another case", "control_group", `{"company_id":9,"MODE":"documented"}`, "must be written"},
		{"unknown type", "nope", `{}`, "unknown job type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateParams(c.typ, json.RawMessage(c.params))
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
			}
			if c.typ == "nope" && !errors.Is(err, ErrUnknownType) {
				t.Fatalf("err = %v, want ErrUnknownType", err)
			}
			if c.wantErr != "" && c.typ != "nope" && !errors.Is(err, ErrBadParams) {
				t.Fatalf("err = %v, want ErrBadParams", err)
			}
		})
	}
}

// seedTiny writes three companies in a chain A - B - C (partner_of) and
// returns their entity ids. All data is invented.
func seedTiny(t *testing.T, p *pgxpool.Pool) (a, b, c int64) {
	t.Helper()
	testutil.SeedSources(t, p)
	col, err := collection.Begin(bg, p, 1, time.Now().UTC().Truncate(24*time.Hour), collection.Scope{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Finish(bg, p, col.ID, nil); err != nil {
		t.Fatal(err)
	}
	s := claims.NewStore(p)
	ent := func(cnpj, name string) int64 {
		id, err := s.UpsertEntity(bg, claims.KindCompany, cnpj, name)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, c = ent("00000001", "ALFA TESTE LTDA"), ent("00000002", "BETA TESTE LTDA"), ent("00000003", "GAMA TESTE LTDA")
	link := func(src, dst int64) {
		if _, err := s.AssertRelationship(bg, claims.RelationshipInput{Type: claims.PartnerOf, SrcEntityID: src, DstEntityID: dst,
			Kind: claims.Fact, Prov: claims.Provenance{CollectionID: col.ID, RecordRef: "test:" + string(rune('a'+src))}}); err != nil {
			t.Fatal(err)
		}
	}
	link(a, b)
	link(b, c)
	return a, b, c
}

func TestPathsJobAgainstRealGraph(t *testing.T) {
	f := newFixture(t)
	a, _, c := seedTiny(t, f.pool)
	RegisterGraphHandlers(f.svc, graph.New(f.pool))
	f.startWorker()

	id, err := f.svc.Enqueue(bg, "paths", map[string]any{"a": a, "b": c, "max_depth": 3, "rel_types": []string{"partner_of"}}, "u1")
	if err != nil {
		t.Fatal(err)
	}
	j := f.await(id, "u1")
	if j.Status != "done" || j.Progress != 1 {
		t.Fatalf("job = %+v", j)
	}
	var res struct{ Paths [][]json.RawMessage }
	if err := json.Unmarshal(j.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 1 || len(res.Paths[0]) != 2 {
		t.Fatalf("result = %s", j.Result)
	}

	id, err = f.svc.Enqueue(bg, "control_group", map[string]any{"company_id": a, "mode": "corporate_links"}, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if j := f.await(id, "u1"); j.Status != "done" || len(j.Result) == 0 {
		t.Fatalf("control job = %+v", j)
	}
}

func TestGraphJobsFailWithReadableErrors(t *testing.T) {
	f := newFixture(t)
	a, _, _ := seedTiny(t, f.pool)
	RegisterGraphHandlers(f.svc, graph.New(f.pool))
	f.startWorker()

	for name, c := range map[string]struct {
		typ    string
		params any
		want   string
	}{
		"missing entity":  {"paths", map[string]any{"a": a, "b": 999999}, "entity not found"},
		"missing company": {"control_group", map[string]any{"company_id": 999999, "mode": "documented"}, "entity not found"},
	} {
		id, err := f.svc.Enqueue(bg, c.typ, c.params, "u1")
		if err != nil {
			t.Fatal(err)
		}
		if j := f.await(id, "u1"); j.Status != "failed" || !strings.Contains(j.Error, c.want) {
			t.Errorf("%s: job = %+v", name, j)
		}
	}
}

func TestEnqueueRejectsInvalidGraphParams(t *testing.T) {
	f := newFixture(t)
	RegisterGraphTypes(f.svc) // enqueue-only, as in the API process
	for name, p := range map[string]struct {
		typ    string
		params any
		want   string
	}{
		"missing b":     {"paths", map[string]any{"a": 1}, "b must be"},
		"unknown field": {"control_group", map[string]any{"company_id": 1, "mode": "documented", "zz": 1}, "unknown field"},
	} {
		if _, err := f.svc.Enqueue(bg, p.typ, p.params, "u1"); !errors.Is(err, ErrBadParams) || !strings.Contains(err.Error(), p.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	var n int
	if err := f.pool.QueryRow(bg, `SELECT count(*) FROM job`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d, %v", n, err)
	}
	// Valid params are accepted without any graph in the process.
	if _, err := f.svc.Enqueue(bg, "paths", map[string]any{"a": 1, "b": 2}, "u1"); err != nil {
		t.Fatal(err)
	}
}

func TestGraphHandlersValidateToo(t *testing.T) {
	f := newFixture(t)
	RegisterGraphHandlers(f.svc, graph.New(f.pool))
	reg, _ := f.svc.handler("paths")
	if _, err := reg.h(bg, json.RawMessage(`{"a":1}`), func(float64) {}); !errors.Is(err, ErrBadParams) {
		t.Fatalf("err = %v", err)
	}
}

func TestParamsFieldListsMatchTheStructs(t *testing.T) {
	tags := func(v any) []string {
		var out []string
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			out = append(out, name)
		}
		return out
	}
	if got := tags(PathsParams{}); !slices.Equal(got, pathsFields) {
		t.Errorf("PathsParams keys %v, pathsFields %v", got, pathsFields)
	}
	if got := tags(ControlGroupParams{}); !slices.Equal(got, controlGroupFields) {
		t.Errorf("ControlGroupParams keys %v, controlGroupFields %v", got, controlGroupFields)
	}
}
