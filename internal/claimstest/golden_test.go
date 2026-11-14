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

func TestMaskRelationshipFirstCollection(t *testing.T) {
	f := make([]string, 15)
	for i := range f {
		f[i] = "f"
	}
	f[relationshipFirstCol] = "receita@2031-01-01#1"
	got := MaskRelationshipFirstCollection(SecRelationship, strings.Join(f, FieldSep))
	if strings.Contains(got, "receita@") || strings.Count(got, FieldSep) != 14 {
		t.Errorf("masked line %q", got)
	}
	if l := strings.Join(f, FieldSep); MaskRelationshipFirstCollection(SecEntity, l) != l {
		t.Error("other sections must not be masked")
	}
}
