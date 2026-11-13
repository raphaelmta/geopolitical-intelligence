package cvm

import (
	"bytes"
	"strings"
	"testing"
)

func TestCanonicalPercent(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"51.370000", "51.37", true},
		{"100.000000", "100", true},
		{"0.000000", "0", true},
		{"0.000001", "0.000001", true},
		{"007.500000", "7.5", true},
		{"51,37", "51.37", true}, // the Brazilian comma, should the CVM switch
		{"42", "42", true},
		{" 3.5 ", "3.5", true},
		{"100.000001", "", false},
		{"150.000000", "", false},
		{"-1.000000", "", false},
		{"12,5x", "", false},
		{"1.234,56", "", false},
		{"1e2", "", false},
		{"", "", false},
		{".5", "", false},
		{"5.", "", false},
		{"NaN", "", false},
	}
	for _, c := range cases {
		got, ok := canonicalPercent(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("canonicalPercent(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParserKeepsOnlyTheCPFFragment(t *testing.T) {
	rows, _, err := parseEntry(bytes.NewReader(fixtureFileRaw(t, entryName(2026))), entryName(2026), 2026)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		s := strings.Join([]string{r.Name, r.Fragment, r.HolderBase, r.Percent}, "|")
		for _, cpf := range []string{"98765432101", "987.654.321-01", "321.654.987-00", "123.123.123-00"} {
			if strings.Contains(s, cpf) {
				t.Errorf("line %d keeps a full CPF", r.Line)
			}
		}
		if r.Name == "ANA FICTÍCIA CONTROLADORA" {
			found = true
			if r.Fragment != "654321" || r.Kind != holderPerson || !r.Controller {
				t.Errorf("Ana parsed as %+v", r)
			}
		}
	}
	if !found {
		t.Error("Ana's row not parsed")
	}
}

func TestAggregateLabels(t *testing.T) {
	cases := map[string]string{
		"Outros":                           "aggregate_outros",
		"OUTROS":                           "aggregate_outros",
		"Outros (registered shareholders)": "aggregate_outros",
		"Ações Tesouraria":                 "aggregate_tesouraria",
		"Ações em Tesouraria":              "aggregate_tesouraria",
		"Total":                            "aggregate_total",
		"OUTROS INVESTIMENTOS S.A.":        "aggregate_outros", // only without a document; see parseRow
		"Outra Coisa S.A.":                 "",
		"Tesouraria Participações Ltda":    "",
	}
	for in, want := range cases {
		if got := aggregate(in); got != want {
			t.Errorf("aggregate(%q) = %q, want %q", in, got, want)
		}
	}
}

func fixtureFileRaw(t *testing.T, name string) []byte { return fixtureFile(t, name) }
