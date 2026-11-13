package connector_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"geoint/internal/claims"
	"geoint/internal/connector/cvm"
	"geoint/internal/connector/legis"
	"geoint/internal/connector/receita"
	"geoint/internal/connector/tse"
)

// relLiteral finds the relationship types a connector's code writes: SQL
// literals ('partner_of') and claims constants (claims.PartnerOf).
var relLiteral = regexp.MustCompile(`'(partner_of|administrator_of|legal_rep_of|donated_to|candidate_for|holds_mandate|controls|shareholder_of)'|claims\.(PartnerOf|AdministratorOf|LegalRepOf|DonatedTo|CandidateFor|HoldsMandate|Controls|ShareholderOf)\b`)

var relConst = map[string]claims.RelType{
	"PartnerOf": claims.PartnerOf, "AdministratorOf": claims.AdministratorOf, "LegalRepOf": claims.LegalRepOf,
	"DonatedTo": claims.DonatedTo, "CandidateFor": claims.CandidateFor, "HoldsMandate": claims.HoldsMandate,
	"Controls": claims.Controls, "ShareholderOf": claims.ShareholderOf,
}

// scanRelTypes returns the relationship types named in the non-test Go
// files of dirs (outside the RelTypes declaration itself).
func scanRelTypes(t *testing.T, dirs ...string) map[claims.RelType]bool {
	t.Helper()
	out := map[claims.RelType]bool{}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "var RelTypes") {
					continue
				}
				for _, m := range relLiteral.FindAllStringSubmatch(line, -1) {
					if m[1] != "" {
						out[claims.RelType(m[1])] = true
					} else {
						out[relConst[m[2]]] = true
					}
				}
			}
		}
	}
	return out
}

// No two connectors may write the same rel_type between two entity-only
// ends: those rows share one open natural key across sources, and
// claims.Store.CloseUnseen lets only the source that wrote a row first close
// it, so the other source's departures would never be recorded. Each
// connector declares what it writes (RelTypes); the declaration must match
// its code.
func TestNoTwoConnectorsShareAnEntityToEntityRelType(t *testing.T) {
	connectors := []struct {
		key      string
		declared map[claims.RelType]bool
		dirs     []string
	}{
		{"receita", receita.RelTypes, []string{"receita"}},
		{"tse", tse.RelTypes, []string{"tse"}},
		{"cvm", cvm.RelTypes, []string{"cvm"}},
		// Câmara and Senado write through the shared legis writer.
		{"camara+senado", legis.RelTypes, []string{"camara", "senado", "legis"}},
	}
	owner := map[claims.RelType]string{}
	for _, c := range connectors {
		scanned := scanRelTypes(t, c.dirs...)
		var got, want []string
		for k := range scanned {
			got = append(got, string(k))
		}
		for k := range c.declared {
			want = append(want, string(k))
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: its code writes %v but RelTypes declares %v", c.key, got, want)
		}
		for typ, entityOnly := range c.declared {
			if !entityOnly {
				continue
			}
			if other, dup := owner[typ]; dup {
				t.Errorf("%s and %s both write %s between entity-only ends", other, c.key, typ)
			}
			owner[typ] = c.key
		}
	}
}
