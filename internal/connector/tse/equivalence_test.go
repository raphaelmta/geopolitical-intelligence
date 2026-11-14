package tse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claimstest"
	"geoint/internal/collection"
	"geoint/internal/connector/receita"
	"geoint/internal/rawstore"
)

// Equivalence test of the load optimisation (task 1 of
// docs/performance/2026-load-optimization-design.md) for TSE, and for TSE
// after Receita (identity groups across sources): see the Receita
// counterpart (receita/equivalence_test.go) and internal/claimstest.
//
// TSE is not a snapshot source (nothing is closed, so there is no shrink
// guard) and takes no slices: those scenarios exist for Receita only.
//
// Reference dates are in 2042 (Receita) and 2043 (TSE), used by no other
// test: the raw store is shared by the test packages and refuses different
// bytes under a key it holds. TSE keys are per day: 2043-01-10 always holds
// the fixture, 2043-01-11 eqEdition2.
var (
	eqRefA       = time.Date(2043, 1, 10, 0, 0, 0, 0, time.UTC)
	eqRefB       = time.Date(2043, 1, 11, 0, 0, 0, 0, time.UTC)
	eqRefReceita = time.Date(2042, 1, 1, 0, 0, 0, 0, time.UTC)
)

// eqThreshold: with 1, the donors whose names the Receita fixture also holds
// with another CPF fragment (two fragments for the name) are proposed.
const eqThreshold = 1

var errInjected = errors.New("injected failure")

type edit = func(entry string, b []byte) []byte

// toLatin1 encodes s (runes below 256 only) as the fixture's Latin-1 bytes.
func toLatin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xff {
			panic(fmt.Sprintf("toLatin1: %q", r))
		}
		out = append(out, byte(r))
	}
	return out
}

// lineEdit applies f to the lines (CRLF kept) of the entry.
func lineEdit(entry string, f func(lines [][]byte) [][]byte) edit {
	return func(name string, b []byte) []byte {
		if name != entry {
			return b
		}
		return bytes.Join(f(bytes.SplitAfter(b, []byte("\n"))), nil)
	}
}

// copyLine appends a copy of the entry's line containing sub with the
// replacements (old, new pairs) applied.
func copyLine(entry, sub string, repl ...string) edit {
	return lineEdit(entry, func(lines [][]byte) [][]byte {
		for _, l := range lines {
			if bytes.Contains(l, toLatin1(sub)) {
				for i := 0; i+1 < len(repl); i += 2 {
					l = bytes.ReplaceAll(l, toLatin1(repl[i]), toLatin1(repl[i+1]))
				}
				return append(lines, l)
			}
		}
		panic("copyLine: no line with " + sub)
	})
}

func dropLine(entry, sub string) edit {
	return lineEdit(entry, func(lines [][]byte) [][]byte {
		var out [][]byte
		for _, l := range lines {
			if !bytes.Contains(l, toLatin1(sub)) {
				out = append(out, l)
			}
		}
		return out
	})
}

func replaceIn(entry, old, new string) edit {
	return func(name string, b []byte) []byte {
		if name != entry {
			return b
		}
		return bytes.Replace(b, toLatin1(old), toLatin1(new), 1)
	}
}

func chain(edits ...edit) edit {
	return func(name string, b []byte) []byte {
		for _, e := range edits {
			b = e(name, b)
		}
		return b
	}
}

// eqEdition2 is a later TSE collection: a new donor (synthetic, invalid CPF
// check digits), a new donation by an existing donor, a receipt gone, a
// changed amount and a changed candidacy result.
var eqEdition2 = chain(
	copyLine("receitas_candidatos_2026_AC.csv", `"70001"`,
		`"55566677788";"LÚCIA DOADORA EXEMPLO";"LÚCIA DOADORA EXEMPLO"`, `"40071234500";"TEREZA NOVA DOADORA";"TEREZA NOVA DOADORA"`,
		`"70001"`, `"70002"`, `"1234,56"`, `"300,00"`),
	copyLine("receitas_candidatos_2026_AC.csv", `"70001"`,
		`"55566677788";"LÚCIA DOADORA EXEMPLO";"LÚCIA DOADORA EXEMPLO"`, `"11122233344";"ROBERTO DOADOR FICTICIO";"ROBERTO DOADOR FICTÍCIO"`,
		`"70001"`, `"70003"`, `"1234,56"`, `"450,00"`),
	dropLine("receitas_candidatos_2024_AC.csv", `"60001"`),
	replaceIn("receitas_candidatos_2022_AC.csv", `"50001";"15/09/2022";"#NULO";"1500,00"`, `"50001";"15/09/2022";"#NULO";"1600,00"`),
	replaceIn("consulta_cand_2026_AC.csv", `"1";"ELEITO"`, `"4";"NÃO ELEITO"`),
)

type eqScenario struct {
	name string
	mask claimstest.Mask
	note string
	run  func(t *testing.T, s *eqSession)
}

type eqSession struct {
	e env
	p *claimstest.Pipeline
}

func (s *eqSession) collect(ctx context.Context, t *testing.T, ref time.Time, zips map[string][]byte) error {
	t.Helper()
	_, err := s.p.Collect(ctx, s.e.c, s.e.store(t, ref, zips), ref, collection.Scope{Full: true})
	return err
}

func (s *eqSession) ok(t *testing.T, ref time.Time, zips map[string][]byte) {
	t.Helper()
	if err := s.collect(t.Context(), t, ref, zips); err != nil {
		t.Fatalf("collection of %s: %v", ref.Format(time.DateOnly), err)
	}
}

// interrupted runs a collection that stops at the nth time the loader
// reaches the stagingHook point: the context is cancelled there, so the
// transaction the point is in rolls back and the load fails, as a crash at
// that point would (the earlier transactions stay committed).
func (s *eqSession) interrupted(t *testing.T, ref time.Time, zips map[string][]byte, point string, nth int) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	seen := 0
	stagingHook = func(_ context.Context, _ pgx.Tx, p string) {
		if p == point {
			if seen++; seen == nth {
				cancel(errInjected)
			}
		}
	}
	err := s.collect(ctx, t, ref, zips)
	stagingHook = nil
	if err == nil || seen < nth {
		t.Fatalf("interrupted collection: err = %v after %d %q points, want a failure at point %d", err, seen, point, nth)
	}
}

// loadReceita runs a full Receita collection of its fixture.
func (s *eqSession) loadReceita(t *testing.T) {
	t.Helper()
	c := receita.New("http://127.0.0.1:1/unused/")
	var files = map[string][]byte{}
	for zipName, csv := range map[string]string{
		"Empresas0.zip": "Empresas.csv", "Estabelecimentos0.zip": "Estabelecimentos.csv", "Socios0.zip": "Socios.csv",
	} {
		files[zipName] = zipOf(t, map[string][]byte{csv: receitaFixture(t, csv)})
	}
	var stored []rawstore.File
	for name, b := range files {
		f, err := s.e.raw.Put(ctx, "raw/receita/"+eqRefReceita.Format(time.DateOnly)+"/"+name, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		stored = append(stored, f)
	}
	if _, err := s.p.Collect(t.Context(), c, stored, eqRefReceita, collection.Scope{Full: true}); err != nil {
		t.Fatalf("receita collection: %v", err)
	}
}

func receitaFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "receita", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// records returns the ids of the TSE person records whose normalized name
// starts with prefix and whose fragment is fragment, by context key.
func (s *eqSession) records(t *testing.T, prefix, fragment string) []int64 {
	t.Helper()
	rows, err := s.e.pool.Query(ctx, `SELECT id FROM person_record WHERE source_id = $1 AND name_norm LIKE $2 || '%'
		AND cpf_fragment = $3 ORDER BY context_key`, sourceID, prefix, fragment)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatalf("no record %s… with fragment %q", prefix, fragment)
	}
	return ids
}

func (s *eqSession) pendingProposal(t *testing.T, prefix string) int64 {
	t.Helper()
	return s.e.id(t, `SELECT id FROM identity_proposal WHERE group_key LIKE $1 || '%' AND status = 'pending'
		ORDER BY group_key LIMIT 1`, prefix)
}

var eqScenarios = []eqScenario{
	{name: "first_load", run: func(t *testing.T, s *eqSession) {
		s.ok(t, eqRefA, fixtureZips(t, nil))
	}},
	{name: "second_collection", note: "first collection, then one with a new donor and donation, a receipt gone and changed values",
		run: func(t *testing.T, s *eqSession) {
			s.ok(t, eqRefA, fixtureZips(t, nil))
			s.ok(t, eqRefB, fixtureZips(t, eqEdition2))
		}},
	{name: "interrupted_in_candidacy_records", note: "second collection interrupted before its candidacy records commit, then repeated",
		run: func(t *testing.T, s *eqSession) {
			s.ok(t, eqRefA, fixtureZips(t, nil))
			s.interrupted(t, eqRefB, fixtureZips(t, eqEdition2), "person staging", 1)
			s.ok(t, eqRefB, fixtureZips(t, eqEdition2))
		}},
	{name: "interrupted_in_donor_records", note: "second collection interrupted before its donor records commit, then repeated",
		run: func(t *testing.T, s *eqSession) {
			s.ok(t, eqRefA, fixtureZips(t, nil))
			s.interrupted(t, eqRefB, fixtureZips(t, eqEdition2), "person staging", 2)
			s.ok(t, eqRefB, fixtureZips(t, eqEdition2))
		}},
	{name: "interrupted_in_relationship_chunk_1", note: "second collection interrupted before its first relationship chunk commits, then repeated",
		run: func(t *testing.T, s *eqSession) {
			s.ok(t, eqRefA, fixtureZips(t, nil))
			s.interrupted(t, eqRefB, fixtureZips(t, eqEdition2), "relationship staging", 1)
			s.ok(t, eqRefB, fixtureZips(t, eqEdition2))
		}},
	{name: "interrupted_in_relationship_chunk_3", mask: claimstest.MaskFirstCollectionOf("tse@2043-01-10#"),
		note: "first collection interrupted in its third relationship chunk (two committed), then repeated\nmasked: first collection of the relationships written by the interrupted collection (depends on how claims are cut into chunks)",
		run: func(t *testing.T, s *eqSession) {
			s.interrupted(t, eqRefA, fixtureZips(t, nil), "relationship staging", 3)
			s.ok(t, eqRefA, fixtureZips(t, nil))
		}},
	{name: "interrupted_after_load", note: "second collection interrupted after every claim is written, then repeated",
		run: func(t *testing.T, s *eqSession) {
			s.ok(t, eqRefA, fixtureZips(t, nil))
			s.interrupted(t, eqRefB, fixtureZips(t, eqEdition2), "after upsert", 1)
			s.ok(t, eqRefB, fixtureZips(t, eqEdition2))
		}},
	{name: "after_receita_with_analyst_decisions",
		note: "Receita full load, TSE collection (groups across sources), analyst Merge, Split, Decide (confirm, reject), TSE second collection",
		run: func(t *testing.T, s *eqSession) {
			s.loadReceita(t)
			s.ok(t, eqRefA, fixtureZips(t, nil))
			r, c := s.p.Resolver, t.Context()
			if err := r.Decide(c, s.pendingProposal(t, "ROBERTO"), true, "analyst-a"); err != nil {
				t.Fatal(err)
			}
			if err := r.Decide(c, s.pendingProposal(t, "LUCIA"), false, "analyst-a"); err != nil {
				t.Fatal(err)
			}
			mariana := s.records(t, "MARIANA", "123456")
			if len(mariana) < 3 {
				t.Fatalf("%d records of MARIANA with her fragment, want 3 (2022, 2026 candidacies, 2024 donor)", len(mariana))
			}
			if _, err := r.Merge(c, []int64{s.records(t, "MARIANA", "")[0], mariana[0]}, "analyst-b", "same candidate (test)"); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Split(c, []int64{mariana[len(mariana)-1]}, "analyst-b", "different person (test)"); err != nil {
				t.Fatal(err)
			}
			s.ok(t, eqRefB, fixtureZips(t, eqEdition2))
		}},
}

func TestEquivalence(t *testing.T) {
	for _, sc := range eqScenarios {
		t.Run(sc.name, func(t *testing.T) {
			claimstest.Equivalence(t, filepath.Join("testdata", "equivalence", sc.name+".golden"), sc.mask, sc.note,
				func(t *testing.T, shift bool) *pgxpool.Pool {
					s := &eqSession{e: newEnv(t)}
					if shift {
						if err := claimstest.ShiftIDs(t.Context(), s.e.pool); err != nil {
							t.Fatal(err)
						}
					}
					s.p = claimstest.NewPipeline(s.e.pool, s.e.raw, eqThreshold)
					sc.run(t, s)
					if s.p.SearchErr != nil {
						t.Fatal(s.p.SearchErr)
					}
					// The scenario ran in the new order (task 3): every load
					// resolved its new records before its relationships.
					s.p.CheckEarlyPass(t)
					return s.e.pool
				})
		})
	}
}
