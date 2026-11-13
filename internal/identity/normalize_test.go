package identity_test

import (
	"strings"
	"testing"

	"geoint/internal/identity"
)

var nameCases = map[string]string{
	"José  da Silva":         "JOSE DA SILVA",
	" joão d'ávila júnior ":  "JOAO D AVILA JUNIOR",
	"MARIA-APARECIDA":        "MARIA APARECIDA",
	"":                       "",
	"   ":                    "",
	"Conceição":              "CONCEICAO",
	"Muñoz Müller":           "MUNOZ MULLER",
	"ÇÃÕÉÜ":                  "CAOEU",
	"Straße":                 "STRASSE",
	"Æther Øster Œuvre":      "AETHER OSTER OEUVRE",
	"Đorđe Łukasz":           "DORDE LUKASZ",
	"ACME 2000 Ltda":         "ACME 2000 LTDA",
	"A\tB\nC\r\fD\vE":        "A B C D E",
	"A B":                    "A B",
	"A​B":                    "A B",
	"A　B":                    "A B",
	"JOSE\x00\x00 SILVA\x00": "JOSE SILVA",
	"A\x01\x7fB":             "A B",
	"Ωmega Жук 日本 SA":        "MEGA SA",
	"€uro ™ © Ltda":          "URO LTDA",
	"ACME™":                  "ACME",
	"ACME®":                  "ACME",
	"ACME© ℠":                "ACME",
	"bad\xff\xfeutf8 \xc3":   "BAD UTF8",
	"éle":                   "ELE",
	"A.B/C,D;E(F)G&H":        "A B C D E F G H",
	"--__--":                 "",
	"ＡＢＣ":                    "ABC",
	"ﬁlho":                   "FILHO",
	"2ª Vara":                "2 VARA",
	"nº 5":                   "N 5",
	"ı":                      "I",
	"İstanbul":               "ISTANBUL",
	"D\u2019ÁVILA":           "D AVILA",
	"A\u00a0B ":              "A B",
	"A\x00B":                 "A B",
	"①②":                     "12",
}

func TestNormalizeName(t *testing.T) {
	for in, want := range nameCases {
		if got := identity.NormalizeName(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func checkNameInvariants(t *testing.T, in string) {
	t.Helper()
	got := identity.NormalizeName(in)
	for _, r := range got {
		ok := r == ' ' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z')
		if !ok {
			t.Fatalf("%q: output %q contains %q", in, got, r)
		}
	}
	if got != strings.TrimSpace(got) || strings.Contains(got, "  ") {
		t.Fatalf("%q: output %q has stray spaces", in, got)
	}
	if again := identity.NormalizeName(got); again != got {
		t.Fatalf("%q: not idempotent: %q then %q", in, got, again)
	}
}

func TestNormalizeNameProperties(t *testing.T) {
	for in, want := range nameCases {
		checkNameInvariants(t, in)
		checkNameInvariants(t, want)
	}
}

func FuzzNormalizeName(f *testing.F) {
	for in := range nameCases {
		f.Add(in)
	}
	f.Add("\x00\xff\xc0\xaf")
	f.Add("ﬁ ǅ ǆ İ ı ẞ ᴀ ª º ① ｆｕｌｌ")
	f.Fuzz(func(t *testing.T, in string) { checkNameInvariants(t, in) })
}

func TestCPFFragment(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"***123456**", "123456", true},
		{"XXX123456XX", "123456", true},
		{"***.123.456-**", "123456", true},
		{"xxx123456xx", "123456", true},
		{"ABC123456DE", "", false},
		{"12a34b56", "", false},
		{"•••123456••", "", false},
		{"123456\u200b", "", false},
		{"123456", "123456", true},
		{" 123456 ", "123456", true},
		{"11122233344", "222333", true},
		{"111.222.333-44", "222333", true},
		{" 111.222.333-44\x00", "222333", true},
		{"111111111-11", "111111", true},
		{"123456789", "", false},
		{"000000", "", false},
		{"***000000**", "", false},
		{"00000000000", "", false},
		{"", "", false},
		{"-4", "", false},
		{"12345", "", false},
		{"1234567", "", false},
		{"123456789012", "", false},
		{"***", "", false},
		{"١٢٣٤٥٦", "", false}, // non-ASCII digits are not digits here
	} {
		got, ok := identity.CPFFragment(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got (%q,%v) want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCNPJBase(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"12.345.678/0001-90", "12345678", true},
		{"12345678000190", "12345678", true},
		{"12345678", "12345678", true},
		{" 12.345.678 ", "12345678", true},
		{"12.ABC.345/01DE-35", "12ABC345", true},
		{"ab12cd34", "AB12CD34", true},
		{"AB12CD340001EF", "AB12CD34", true},
		{"00.000.000/0001-91", "00000000", true},
		{"00000000000191", "00000000", true},
		{"00000000", "00000000", true},
		{"00000000000000", "", false},
		{"1234", "", false},
		{"", "", false},
		{"123456789", "", false},
		{"1234567800019", "", false},
		{"123456780001900", "", false},
		{"1234567Ç", "", false},
		{"12345678\x00", "12345678", true},
		{"12*45678", "", false},
	} {
		got, ok := identity.CNPJBase(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got (%q,%v) want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
