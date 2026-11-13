package senado

import (
	"testing"
	"time"
)

func d(s string) *time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// Ends are exclusive here (as affiliations() returns them).
func TestSplitByAffiliation(t *testing.T) {
	affs := []affiliation{
		{party: "PODE", from: *d("2017-07-04"), to: d("2021-04-08")},
		{party: "PL", from: *d("2021-04-08"), to: d("2026-09-10")},
		{party: "", from: *d("2026-09-10")},
	}
	for name, tc := range map[string]struct {
		p         period
		want      []string
		uncovered bool
	}{
		"two parties":              {period{from: *d("2019-02-01"), to: d("2023-02-01")}, []string{"PODE 2019-02-01..2021-04-08", "PL 2021-04-08..2023-02-01"}, false},
		"open, ends without party": {period{from: *d("2026-04-10")}, []string{"PL 2026-04-10..2026-09-10", " 2026-09-10.."}, false},
		"before any affiliation":   {period{from: *d("2015-02-01"), to: d("2018-01-01")}, []string{"PODE 2017-07-04..2018-01-01"}, true},
	} {
		got, uncovered := split(tc.p, "RJ", 57, affs)
		var s []string
		for _, st := range got {
			end := ""
			if st.To != nil {
				end = st.To.Format(time.DateOnly)
			}
			s = append(s, st.Party+" "+st.From.Format(time.DateOnly)+".."+end)
		}
		if len(s) != len(tc.want) || uncovered != tc.uncovered {
			t.Errorf("%s: got %v (uncovered %v), want %v (uncovered %v)", name, s, uncovered, tc.want, tc.uncovered)
			continue
		}
		for i := range s {
			if s[i] != tc.want[i] {
				t.Errorf("%s: got %v, want %v", name, s, tc.want)
			}
		}
	}
	// Affiliations: the published last day is stored + 1; a same-day change
	// (old ends D, new starts D) ends the old on D.
	got, reason, err := affiliations("f.json", []byte(`{"FiliacaoParlamentar": {"Parlamentar": {"Filiacoes": {"Filiacao": [
		{"Partido": {"SiglaPartido": "UNIÃO", "NomePartido": "União Brasil"}, "DataFiliacao": "2022-02-23", "DataDesfiliacao": "2026-03-29"},
		{"Partido": {"SiglaPartido": "DEM", "NomePartido": "Democratas"}, "DataFiliacao": "2007-08-03", "DataDesfiliacao": "2022-02-23"},
		{"Partido": {"SiglaPartido": "PL", "NomePartido": "Partido Liberal"}, "DataFiliacao": "2026-03-30"}]}}}}`))
	if err != nil || reason != "" || len(got) != 3 {
		t.Fatalf("affiliations: %v %q %v", got, reason, err)
	}
	for i, want := range []string{"2022-02-23", "2026-03-30", ""} {
		end := ""
		if got[i].to != nil {
			end = got[i].to.Format(time.DateOnly)
		}
		if end != want {
			t.Errorf("affiliation %s ends %q, want %q", got[i].party, end, want)
		}
	}
	// An affiliation that ends while the exercise goes on leaves it uncovered.
	if _, uncovered := split(period{from: *d("2024-01-01")}, "RJ", 57, affs[:2]); !uncovered {
		t.Error("open exercise past the last affiliation is not reported")
	}
}
