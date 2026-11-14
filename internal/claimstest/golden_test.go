package claimstest

import (
	"strings"
	"testing"
)

func TestGoldenRoundTrip(t *testing.T) {
	in := map[string]string{
		"a":     "x\ty\n# not a comment inside a section\n",
		"b":     "",
		"empty": "",
		"c":     "== almost a header\n",
	}
	got, err := parseGolden(renderGolden(in, "note line 1\nnote line 2"))
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(in, got); d != "" {
		t.Errorf("round trip differs:%s", d)
	}
	if Hash(in) != Hash(got) {
		t.Error("hash changed by the round trip")
	}
}

func TestDiffListsMissingAndExtraLines(t *testing.T) {
	want := map[string]string{"s": "a\nb\nb\nc\n", "same": "x\n"}
	got := map[string]string{"s": "a\nb\nd\n", "same": "x\n"}
	d := Diff(want, got)
	for _, s := range []string{"section s differs: 4 lines want, 3 got; 2 missing, 1 extra", "- b", "- c", "+ d"} {
		if !strings.Contains(d, s) {
			t.Errorf("diff lacks %q:%s", s, d)
		}
	}
	if strings.Contains(d, "same") {
		t.Errorf("diff lists an equal section:%s", d)
	}
}

func TestMaskFirstCollectionOf(t *testing.T) {
	mask := MaskFirstCollectionOf("receita@2031-02-01#")
	f := make([]string, 15)
	for i := range f {
		f[i] = "receita@2031-02-01#1"
	}
	got := mask(SecRelationship, strings.Join(f, FieldSep))
	if strings.Split(got, FieldSep)[relationshipFirstCol] != "*" || strings.Count(got, "receita@") != 14 {
		t.Errorf("masked line %q: only the first collection must be masked", got)
	}
	f[relationshipFirstCol] = "receita@2031-01-01#1"
	if l := strings.Join(f, FieldSep); mask(SecRelationship, l) != l {
		t.Error("a relationship first written by another collection must not be masked")
	}
	if l := strings.Join(f, FieldSep); mask(SecEntity, l) != l {
		t.Error("other sections must not be masked")
	}
}
