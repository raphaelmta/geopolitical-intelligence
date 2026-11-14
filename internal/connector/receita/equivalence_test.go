package receita

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"geoint/internal/claims"
	"geoint/internal/claimstest"
	"geoint/internal/collection"
)

// Equivalence test of the load optimisation (task 1 of
// docs/performance/2026-load-optimization-design.md): every scenario runs
// the worker's collection path (claimstest.Pipeline: Load, CloseUnseen,
// Resolve, search refresh) over the fixture, and its final canonical dump
// must match testdata/equivalence/<scenario>.golden, recorded from the code
// before the optimisation. Regenerating the golden files is for intended
// differences only (see internal/claimstest/golden.go).
//
// Reference dates are in 2041, used by no other test: the raw store is shared
// by the test packages, a key is per edition month and it refuses different
// bytes under a key it holds. So each month always carries the same edition:
// January eqBase, February and March eqEdition2, April eqEdition3, May
// eqShrunk, June eqEdition2, July to September eqLife1 to eqLife3.
var (
	eqRef1 = time.Date(2041, 1, 1, 0, 0, 0, 0, time.UTC)
	eqRef2 = time.Date(2041, 2, 1, 0, 0, 0, 0, time.UTC)
	eqRef3 = time.Date(2041, 3, 1, 0, 0, 0, 0, time.UTC)
	eqRef4 = time.Date(2041, 4, 1, 0, 0, 0, 0, time.UTC)
	eqRef5 = time.Date(2041, 5, 1, 0, 0, 0, 0, time.UTC)
	eqRef6 = time.Date(2041, 6, 1, 0, 0, 0, 0, time.UTC)
	eqRef7 = time.Date(2041, 7, 1, 0, 0, 0, 0, time.UTC)
	eqRef8 = time.Date(2041, 8, 1, 0, 0, 0, 0, time.UTC)
	eqRef9 = time.Date(2041, 9, 1, 0, 0, 0, 0, time.UTC)
)

// eqThreshold is the resolver's CommonNameThreshold in these scenarios: low
// enough for the synthetic name with three fragments (eqIdentityRows) to be
// proposed instead of merged.
const eqThreshold = 2

var errInjected = errors.New("injected failure")

type eqScenario struct {
	name string
	mask claimstest.Mask
	note string
	run  func(t *testing.T, s *eqSession)
}

// eqSession is one run of a scenario on its own schema.
type eqSession struct {
	e env
	p *claimstest.Pipeline
}

// collect runs one collection of the edition ref over zips; it returns the
// collection's error (the collection is then recorded failed).
func (s *eqSession) collect(t *testing.T, ref time.Time, zips map[string][]byte, scope collection.Scope) error {
	t.Helper()
	_, err := s.p.Collect(t.Context(), s.e.c, s.e.store(t, ref, zips), ref, scope)
	return err
}

func (s *eqSession) ok(t *testing.T, ref time.Time, zips map[string][]byte, scope collection.Scope) {
	t.Helper()
	if err := s.collect(t, ref, zips, scope); err != nil {
		t.Fatalf("collection of %s: %v", ref.Format(time.DateOnly), err)
	}
}

// interrupted runs a full collection that stops at the checkpoint for which
// stop returns true, as a crash there would. checkpointHook is a package
// global: the scenarios (and every test of this package that sets it) must
// not run in parallel.
func (s *eqSession) interrupted(t *testing.T, ref time.Time, zips map[string][]byte, stop func(point string) bool) {
	t.Helper()
	checkpointHook = func(p string) error {
		if stop(p) {
			return fmt.Errorf("%w at %s", errInjected, p)
		}
		return nil
	}
	defer func() { checkpointHook = nil }()
	err := s.collect(t, ref, zips, full)
	if !errors.Is(err, errInjected) {
		t.Fatalf("interrupted collection: err = %v, want the injected failure", err)
	}
}

func (s *eqSession) record(t *testing.T, context, namePrefix, fragment string) int64 {
	t.Helper()
	return s.e.id(t, `SELECT id FROM person_record WHERE source_id = $1 AND context_key = $2 AND name_norm LIKE $3 || '%'
		AND cpf_fragment = $4`, sourceID, context, namePrefix, fragment)
}

func (s *eqSession) proposal(t *testing.T, groupKeySuffix string) int64 {
	t.Helper()
	return s.e.id(t, `SELECT id FROM identity_proposal WHERE group_key LIKE '%' || $1 AND status = 'pending'`, groupKeySuffix)
}

var full = collection.Scope{Full: true}

// Fixture editions. Every added row is synthetic (invented names, CPF
// fragments in the 7xxxxx range, CNPJ bases in the fixture's 9899 range).

type edit = func(name string, b []byte) []byte

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

func socio(base, name, fragment, qual, since string) string {
	return fmt.Sprintf(`"%s";"2";"%s";"***%s**";"%s";"%s";"";"***000000**";"";"00";"5"`, base, name, fragment, qual, since)
}

// appendLines adds LF-terminated lines to the CSV file.
func appendLines(file string, lines ...string) edit {
	return func(name string, b []byte) []byte {
		if name != file {
			return b
		}
		out := append([]byte{}, b...)
		for _, l := range lines {
			out = append(append(out, toLatin1(l)...), '\n')
		}
		return out
	}
}

// dropLines removes from the CSV file every line containing any of subs
// (Latin-1). Later lines move up, as in a real new edition.
func dropLines(file string, subs ...string) edit {
	return func(name string, b []byte) []byte {
		if name != file {
			return b
		}
		var out []byte
		for _, l := range bytes.SplitAfter(b, []byte("\n")) {
			keep := true
			for _, s := range subs {
				if bytes.Contains(l, toLatin1(s)) {
					keep = false
				}
			}
			if keep {
				out = append(out, l...)
			}
		}
		return out
	}
}

// copyLine appends to file a copy of its line containing sub, with the
// replacements (old, new pairs) applied.
func copyLine(file, sub string, repl ...string) edit {
	return func(name string, b []byte) []byte {
		if name != file {
			return b
		}
		for _, l := range bytes.SplitAfter(b, []byte("\n")) {
			if bytes.Contains(l, toLatin1(sub)) {
				for i := 0; i+1 < len(repl); i += 2 {
					l = bytes.ReplaceAll(l, toLatin1(repl[i]), toLatin1(repl[i+1]))
				}
				return append(append([]byte{}, b...), l...)
			}
		}
		panic("copyLine: no line with " + sub)
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

// eqIdentityRows: a name with three CPF fragments (above eqThreshold: its
// groups are proposed) and a second record without fragment of a fixture
// partner (only an analyst can merge it).
var eqIdentityRows = appendLines("Socios.csv",
	socio("98990018", "CARLA REPETIDA FICTÍCIA", "700001", "49", "20200303"),
	socio("98990021", "CARLA REPETIDA FICTÍCIA", "700001", "22", "20200303"),
	socio("98990023", "CARLA REPETIDA FICTÍCIA", "700002", "49", "20200303"),
	socio("98990024", "CARLA REPETIDA FICTÍCIA", "700002", "22", "20200303"),
	socio("98990025", "CARLA REPETIDA FICTÍCIA", "700003", "49", "20200303"),
	socio("98990027", "DÉBORA HOLANDA", "000000", "22", "20190919"),
)

// eqBase is the first edition: the fixture and eqIdentityRows.
func eqBase(t *testing.T) map[string][]byte { return fixtureZips(t, eqIdentityRows) }

// eqEdition2 changes eqBase: a renamed company and a changed qualification
// (variant), a new company with a new partner, a new partner in an existing
// company, one partner gone and one company gone (all its rows).
var eqEdition2 = chain(eqIdentityRows, variant,
	appendLines("Empresas.csv", `"98990030";"ÔMICRON LOGÍSTICA LTDA";"2062";"49";"30000,00";"01";""`),
	copyLine("Estabelecimentos.csv", `"98990004";"0001"`, `"98990004"`, `"98990030"`),
	appendLines("Socios.csv",
		socio("98990030", "MÁRCIA NOVATA FICTÍCIA", "710001", "49", "20301215"),
		socio("98990025", "OTÁVIO RECENTE FICTÍCIO", "710002", "22", "20301215")),
	dropLines("Socios.csv", "CÍNTIA GUEDES", `"98990028";`),
	dropLines("Empresas.csv", `"98990028";`),
	dropLines("Estabelecimentos.csv", `"98990028";`),
)

// eqEdition3 is eqEdition2 with new records whose keys an analyst decided
// on (analyst scenario): a confirmed key, a rejected key and an ambiguous one.
var eqEdition3 = chain(eqEdition2, appendLines("Socios.csv",
	socio("98990030", "CARLA REPETIDA FICTÍCIA", "700001", "22", "20301215"),
	socio("98990020", "CARLA REPETIDA FICTÍCIA", "700002", "22", "20301215"),
	socio("98990027", "JOSÉ CONCEIÇÃO BRANDÃO", "123456", "22", "20301215"),
))

// eqShrunk loses the partners of every company from 98990010 on: far fewer
// than 90 % of the open relationships are seen again.
var eqShrunk = chain(eqIdentityRows, func(name string, b []byte) []byte {
	if name != "Socios.csv" {
		return b
	}
	var out []byte
	for _, l := range bytes.SplitAfter(b, []byte("\n")) {
		if len(l) > 9 && string(l[1:9]) < "98990010" {
			out = append(out, l...)
		}
	}
	return out
})

// The proposal life cycle (scenario proposal_lifecycle). Person records are
// never removed (an edition that drops a partner only closes its
// relationships, and a proposal counts closed relationships too), so a
// proposal changes only when its name is touched again by a new record:
//   - eqLife1: eqBase and a third record of CARLA|700002;
//   - eqLife2: a new CARLA record (fragment 700003) touches the name;
//   - eqLife3: two new CARLA|700001 records, in two new entities.
var (
	eqLife1 = chain(eqIdentityRows, appendLines("Socios.csv",
		socio("98990016", "CARLA REPETIDA FICTÍCIA", "700002", "22", "20200303")))
	eqLife2 = chain(eqLife1, appendLines("Socios.csv",
		socio("98990020", "CARLA REPETIDA FICTÍCIA", "700003", "49", "20301215")))
	eqLife3 = chain(eqLife2, appendLines("Socios.csv",
		socio("98990011", "CARLA REPETIDA FICTÍCIA", "700001", "22", "20310115"),
		socio("98990013", "CARLA REPETIDA FICTÍCIA", "700001", "49", "20310115")))
)

// proposalState returns status, number of records and connections of the
// proposal whose group key ends with suffix.
func (s *eqSession) proposalState(t *testing.T, suffix string) (status string, records, connections int) {
	t.Helper()
	if err := s.e.pool.QueryRow(ctx, `SELECT status, cardinality(record_ids), connections FROM identity_proposal
		WHERE group_key LIKE '%' || $1`, suffix).Scan(&status, &records, &connections); err != nil {
		t.Fatalf("proposal %s: %v", suffix, err)
	}
	return status, records, connections
}

func chunkAt(n int) func(string) bool {
	return func(p string) bool { return p == fmt.Sprintf("relationship chunk %d", n) }
}

var eqScenarios = []eqScenario{
	{name: "first_load", run: func(t *testing.T, s *eqSession) {
		s.ok(t, eqRef1, eqBase(t), full)
	}},
	{name: "second_edition", note: "first load, then an edition with additions, removals and changed companies", run: func(t *testing.T, s *eqSession) {
		s.ok(t, eqRef1, eqBase(t), full)
		s.ok(t, eqRef2, fixtureZips(t, eqEdition2), full)
	}},
	{name: "slice_between_full_loads", note: "full load, a depth-1 slice of the next edition, then that edition in full", run: func(t *testing.T, s *eqSession) {
		s.ok(t, eqRef1, eqBase(t), full)
		s.ok(t, eqRef2, fixtureZips(t, eqEdition2), collection.Scope{CNPJs: []string{"98990002"}, Depth: 1})
		s.ok(t, eqRef3, fixtureZips(t, eqEdition2), full)
	}},
	{name: "interrupted_after_companies", note: "second edition interrupted after the companies phase, then repeated", run: func(t *testing.T, s *eqSession) {
		s.ok(t, eqRef1, eqBase(t), full)
		s.interrupted(t, eqRef2, fixtureZips(t, eqEdition2), func(p string) bool { return p == "companies" })
		s.ok(t, eqRef2, fixtureZips(t, eqEdition2), full)
	}},
	{name: "interrupted_after_persons", note: "second edition interrupted after the persons phase, then repeated", run: func(t *testing.T, s *eqSession) {
		s.ok(t, eqRef1, eqBase(t), full)
		s.interrupted(t, eqRef2, fixtureZips(t, eqEdition2), func(p string) bool { return p == "persons" })
		s.ok(t, eqRef2, fixtureZips(t, eqEdition2), full)
	}},
	{name: "interrupted_after_relationship_chunk_1", mask: claimstest.MaskFirstCollectionOf("receita@2041-02-01#"),
		note: "second edition interrupted after the first relationship chunk, then repeated\nmasked: first collection of the relationships written by the interrupted edition (depends on how claims are cut into chunks)",
		run: func(t *testing.T, s *eqSession) {
			s.ok(t, eqRef1, eqBase(t), full)
			s.interrupted(t, eqRef2, fixtureZips(t, eqEdition2), chunkAt(1))
			s.ok(t, eqRef2, fixtureZips(t, eqEdition2), full)
		}},
	{name: "interrupted_after_relationship_chunk_3", mask: claimstest.MaskFirstCollectionOf("receita@2041-01-01#"),
		note: "first load interrupted after the third relationship chunk, then repeated\nmasked: first collection of the relationships written by the interrupted edition (depends on how claims are cut into chunks)",
		run: func(t *testing.T, s *eqSession) {
			s.interrupted(t, eqRef1, eqBase(t), chunkAt(3))
			s.ok(t, eqRef1, eqBase(t), full)
		}},
	{name: "interrupted_after_last_relationship_chunk", note: "second edition interrupted after its last relationship chunk (every claim written), then repeated",
		run: func(t *testing.T, s *eqSession) {
			s.ok(t, eqRef1, eqBase(t), full)
			chunks := 0
			s.interrupted(t, eqRef2, fixtureZips(t, eqEdition2), func(p string) bool {
				if !strings.HasPrefix(p, "relationship chunk ") {
					return false
				}
				if chunks == 0 {
					chunks = s.e.count(t, `SELECT count(DISTINCT bucket) FROM receita_stg_partner`)
				}
				return p == fmt.Sprintf("relationship chunk %d", chunks)
			})
			s.ok(t, eqRef2, fixtureZips(t, eqEdition2), full)
		}},
	{name: "shrink_guard_refused", note: "first load, an edition refused by the shrink guard of CloseUnseen, then a good edition", run: func(t *testing.T, s *eqSession) {
		s.ok(t, eqRef1, eqBase(t), full)
		if err := s.collect(t, eqRef5, fixtureZips(t, eqShrunk), full); !errors.Is(err, claims.ErrShrunkCollection) {
			t.Fatalf("shrunk edition: err = %v, want ErrShrunkCollection", err)
		}
		s.ok(t, eqRef6, fixtureZips(t, eqEdition2), full)
	}},
	{name: "analyst_decisions", note: "first load; analyst Merge, Split, Decide (confirm, reject); an edition with new records of those keys",
		run: func(t *testing.T, s *eqSession) {
			s.ok(t, eqRef1, eqBase(t), full)
			r := s.p.Resolver
			// Merge: two records of one partner that Resolve cannot join (no
			// fragment on one side); both get locked by the decision.
			if _, err := r.Merge(t.Context(), []int64{
				s.record(t, "98990028", "DEBORA", "161718"), s.record(t, "98990027", "DEBORA", ""),
			}, "analyst-a", "same partner (test)"); err != nil {
				t.Fatal(err)
			}
			// Split: one of the three records Resolve merged by name and
			// fragment; the key ends up on both sides, so it becomes ambiguous.
			if _, err := r.Split(t.Context(), []int64{s.record(t, "98990005", "JOSE", "123456")}, "analyst-a",
				"different person (test)"); err != nil {
				t.Fatal(err)
			}
			if n := s.e.count(t, `SELECT count(*) FROM identity_ambiguous_key`); n != 1 {
				t.Fatalf("%d ambiguous keys after the split, want 1", n)
			}
			// The name with three fragments: both of its groups were proposed.
			if err := r.Decide(t.Context(), s.proposal(t, "|700001"), true, "analyst-b"); err != nil {
				t.Fatal(err)
			}
			if err := r.Decide(t.Context(), s.proposal(t, "|700002"), false, "analyst-b"); err != nil {
				t.Fatal(err)
			}
			s.ok(t, eqRef4, fixtureZips(t, eqEdition3), full)
		}},
}

func init() {
	eqScenarios = append(eqScenarios,
		eqScenario{name: "proposal_lifecycle",
			note: "proposals that go obsolete and reopen, and one whose records and connections shrink, across three editions",
			run: func(t *testing.T, s *eqSession) {
				r := s.p.Resolver
				s.ok(t, eqRef7, fixtureZips(t, eqLife1), full)
				if st, n, _ := s.proposalState(t, "|700001"); st != "pending" || n != 2 {
					t.Fatalf("CARLA|700001 after the first edition: %s with %d records, want pending with 2", st, n)
				}
				_, n2, c2 := s.proposalState(t, "|700002")
				if n2 != 3 {
					t.Fatalf("CARLA|700002 has %d records, want 3", n2)
				}
				// An analyst joins both records of CARLA|700001 (locked: nothing
				// is left to propose) and locks one record of CARLA|700002 into
				// another person (that proposal loses a record and its links).
				if _, err := r.Merge(t.Context(), []int64{
					s.record(t, "98990018", "CARLA", "700001"), s.record(t, "98990021", "CARLA", "700001"),
				}, "analyst-a", "same person (test)"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Merge(t.Context(), []int64{
					s.record(t, "98990016", "CARLA", "700002"), s.record(t, "98990016", "OSVALDO", "223344"),
				}, "analyst-a", "same person (test)"); err != nil {
					t.Fatal(err)
				}
				s.ok(t, eqRef8, fixtureZips(t, eqLife2), full)
				if st, _, _ := s.proposalState(t, "|700001"); st != "obsolete" {
					t.Fatalf("CARLA|700001 after the analyst merge and a new record of the name: %s, want obsolete", st)
				}
				if st, n, c := s.proposalState(t, "|700002"); st != "pending" || n != 2 || c >= c2 {
					t.Fatalf("CARLA|700002: %s with %d records and %d connections, want pending with 2 and fewer than %d", st, n, c, c2)
				}
				s.ok(t, eqRef9, fixtureZips(t, eqLife3), full)
				if st, n, _ := s.proposalState(t, "|700001"); st != "pending" || n != 2 {
					t.Fatalf("CARLA|700001 after two new records: %s with %d records, want pending (reopened) with 2", st, n)
				}
			}},
		eqScenario{name: "decisions_between_interrupted_load_and_repeat",
			note: "second edition interrupted after the persons phase; analyst Decide (confirm, reject) and a Merge of a record the failed run wrote; then the edition repeated",
			run: func(t *testing.T, s *eqSession) {
				r := s.p.Resolver
				s.ok(t, eqRef1, eqBase(t), full)
				s.interrupted(t, eqRef2, fixtureZips(t, eqEdition2), func(p string) bool { return p == "persons" })
				if err := r.Decide(t.Context(), s.proposal(t, "|700001"), true, "analyst-b"); err != nil {
					t.Fatal(err)
				}
				if err := r.Decide(t.Context(), s.proposal(t, "|700002"), false, "analyst-b"); err != nil {
					t.Fatal(err)
				}
				// MÁRCIA's record was written by the failed run and never resolved.
				if _, err := r.Merge(t.Context(), []int64{
					s.record(t, "98990030", "MARCIA", "710001"), s.record(t, "98990004", "ANTONIO", "345678"),
				}, "analyst-b", "same person (test)"); err != nil {
					t.Fatal(err)
				}
				s.ok(t, eqRef2, fixtureZips(t, eqEdition2), full)
			}},
	)
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
					return s.e.pool
				})
		})
	}
}
