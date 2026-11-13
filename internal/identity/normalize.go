// Package identity holds the pure functions that decide when two records
// describe the same person or company.
package identity

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// foldedRunes maps letters that do not decompose under NFKD to ASCII.
var foldedRunes = map[rune]string{
	'ß': "SS", 'ẞ': "SS",
	'Æ': "AE", 'æ': "AE",
	'Ø': "O", 'ø': "O",
	'Œ': "OE", 'œ': "OE",
	'Đ': "D", 'đ': "D",
	'Ł': "L", 'ł': "L",
	'ı': "I",
}

// separators are replaced by a space before decomposing. ª, º, ™ and ℠ would
// otherwise become letters under NFKD (ª→a, º→o, ™→TM, ℠→SM); ® and © do not
// decompose and would become separators anyway, but are listed so that every
// trademark-like mark is handled in one place.
var separators = strings.NewReplacer("ª", " ", "º", " ", "™", " ", "®", " ", "©", " ", "℠", " ")

// NormalizeName returns the canonical matching form of a person or company
// name: upper-case ASCII letters, digits and single spaces only, with no
// leading or trailing space.
//
// Text is decomposed with NFKD, so letters with diacritics fold to their base
// letter (ç→C, ã→A, ñ→N) and compatibility forms fold too (ﬁ→FI, full-width
// ＡＢＣ→ABC, ①→1). ß, Æ, Ø, Œ, Đ, Ł and ı are mapped explicitly because they
// do not decompose; the ordinal indicators ª and º and the marks ™ ® © ℠
// are separators.
// Every other character (punctuation, whitespace of any kind, control and
// zero-width characters, NUL bytes, other scripts, symbols, invalid UTF-8)
// becomes a separator. The result is idempotent.
func NormalizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	emit := func(str string) {
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteString(str)
	}
	for _, r := range norm.NFKD.String(separators.Replace(s)) {
		switch {
		case r >= 'a' && r <= 'z':
			emit(string(r - 'a' + 'A'))
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			emit(string(r))
		case unicode.Is(unicode.Mn, r):
			// combining mark left over after NFKD: drop it, keep the base letter
		default:
			if f, ok := foldedRunes[r]; ok {
				emit(f)
			} else {
				pendingSpace = true
			}
		}
	}
	return b.String()
}

// CPFFragment returns the six digits (positions 4 to 9) of a CPF that
// identify a person across sources, and whether one could be extracted.
//
// The fragment is a matching key, not a validated CPF: no check digits are
// verified, and implausible values such as 111111 are returned as-is. Only
// mask and punctuation characters may surround or separate the digits: '*',
// 'X', 'x', '.', '-', '/', whitespace and NUL (the Receita files contain NUL
// bytes). Any other character gives "", false. The Receita masks CPFs as
// "***123456**"; sources also use "XXX123456XX" or "111.222.333-44". Rules,
// applied to the ASCII digits:
//   - exactly 6 digits: that fragment (the masked form, or a bare fragment);
//   - exactly 11 digits: digits 4 to 9 of the full CPF;
//   - any other count (including none): "", false;
//   - the fragment "000000", which the Receita uses as filler, is "", false.
func CPFFragment(s string) (string, bool) {
	digits := make([]byte, 0, 11)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			if len(digits) == 11 {
				return "", false
			}
			digits = append(digits, c)
		case strings.IndexByte("*Xx.-/ \t\n\v\f\r\x00", c) >= 0:
		default:
			return "", false
		}
	}
	var frag string
	switch len(digits) {
	case 6:
		frag = string(digits)
	case 11:
		frag = string(digits[3:9])
	default:
		return "", false
	}
	if frag == "000000" {
		return "", false
	}
	return frag, true
}

// CNPJBase returns the 8-character base (root) of a CNPJ, which identifies
// the company across its branches, and whether one could be extracted.
//
// Since 2026 a CNPJ may be alphanumeric, so each character is a digit or an
// ASCII letter. After upper-casing and removing punctuation and whitespace
// (including NUL bytes), the value must have exactly 8 (base) or 14 (full)
// characters of [0-9A-Z]; the first 8 are returned. Anything else, including
// non-ASCII letters or digits, gives "", false. The all-zero base is valid
// (00.000.000 is Banco do Brasil); only an all-zero 14-character value is
// rejected, as filler.
func CNPJBase(s string) (string, bool) {
	out := make([]byte, 0, 14)
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z':
			out = append(out, byte(r))
		case r >= 'a' && r <= 'z':
			out = append(out, byte(r-'a'+'A'))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return "", false
		}
		if len(out) > 14 {
			return "", false
		}
	}
	if len(out) != 8 && len(out) != 14 {
		return "", false
	}
	if len(out) == 14 && string(out) == "00000000000000" {
		return "", false
	}
	return string(out[:8]), true
}
