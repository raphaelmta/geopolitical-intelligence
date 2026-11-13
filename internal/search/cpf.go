package search

import (
	"errors"
	"slices"
	"strings"
)

// Errors of a query that carries a CPF (see Find).
var (
	// ErrInvalidCPF: a full CPF (11 digits) whose check digits do not match,
	// or with all digits equal. Catches typos; the masked form has no check.
	ErrInvalidCPF = errors.New("invalid cpf")
	// ErrMultipleCPFs: the query carries two different CPFs.
	ErrMultipleCPFs = errors.New("more than one cpf")
)

// cpfInQuery is what extractCPF found in a query.
type cpfInQuery struct {
	// fragment: the 6 middle digits (positions 4 to 9), the only part of a
	// CPF the platform stores (person_record.cpf_fragment).
	fragment string
	// rest: the query with the CPF replaced by a space, for the name words.
	rest string
}

// extractCPF finds the CPF a query carries, before any word rule runs:
//
//   - the whole query is 11 digits once . / - and spaces are removed
//     (12345678909, 123 456 789 09, 123.456.789-09);
//   - inside a longer query, any spelling MaskCPF masks: a run of exactly 11
//     digits, or ddd S ddd S ddd S dd with each S at most one of . - / and
//     space (maria silva 123.456.789-09);
//   - the masked form the sources publish: *** S? ddd S? ddd S? ** with S
//     one of . - and space (***456789**, ***.456.789-**);
//
// in both cases standing apart, not glued to a letter or digit (see glued).
//
// A full CPF must have valid check digits (ErrInvalidCPF). Two different CPFs
// are ErrMultipleCPFs; the same one twice counts once. ok is false when the
// query carries no CPF; then it goes through the name and CNPJ rules as
// before. A bare 6-digit number is never a CPF (it is a CNPJ prefix).
func extractCPF(q string) (cpfInQuery, bool, error) {
	if d, numeric := numericQuery(q); numeric && len(d) == 11 {
		if !validCPF(d) {
			return cpfInQuery{}, false, ErrInvalidCPF
		}
		return cpfInQuery{fragment: d[3:9]}, true, nil
	}
	var found []string // distinct spellings, as digits (full) or "*" + fragment
	var frag string
	var rest strings.Builder
	invalid := false
	for i := 0; i < len(q); {
		if n, f := maskedCPFAt(q, i); n > 0 && !glued(q, i, n) {
			if key := "*" + f; !slices.Contains(found, key) {
				found = append(found, key)
			}
			frag = f
			rest.WriteByte(' ')
			i += n
			continue
		}
		if n := cpfAt(q, i); n > 0 && !glued(q, i, n) {
			d := digitsOf(q[i : i+n])
			if !validCPF(d) {
				invalid = true
			} else if !slices.Contains(found, d) {
				found = append(found, d)
			}
			frag = d[3:9]
			rest.WriteByte(' ')
			i += n
			continue
		}
		rest.WriteByte(q[i])
		i++
	}
	switch {
	case invalid:
		return cpfInQuery{}, false, ErrInvalidCPF
	case len(found) == 0:
		return cpfInQuery{}, false, nil
	case len(found) > 1:
		return cpfInQuery{}, false, ErrMultipleCPFs
	}
	return cpfInQuery{fragment: frag, rest: rest.String()}, true, nil
}

// maskedCPFAt returns the length of the masked CPF starting at i and its
// fragment, or 0. It must not touch another '*' or digit on either side.
func maskedCPFAt(s string, i int) (int, string) {
	if i > 0 && (s[i-1] == '*' || isDigit(s[i-1])) || !strings.HasPrefix(s[i:], "***") {
		return 0, ""
	}
	k := i + 3
	frag := make([]byte, 0, 6)
	for range 2 {
		if k < len(s) && isMaskSeparator(s[k]) {
			k++
		}
		for range 3 {
			if k >= len(s) || !isDigit(s[k]) {
				return 0, ""
			}
			frag = append(frag, s[k])
			k++
		}
	}
	if k < len(s) && isMaskSeparator(s[k]) {
		k++
	}
	if !strings.HasPrefix(s[k:], "**") {
		return 0, ""
	}
	k += 2
	if k < len(s) && (s[k] == '*' || isDigit(s[k])) {
		return 0, ""
	}
	return k - i, string(frag)
}

// glued reports whether s[i:i+n] touches a letter or digit on either side
// (any non-ASCII byte counts as a letter). A CPF glued to a word
// (MARIA12345678909) is not taken out: that token goes through the word
// rules, which drop it, as before CPF search existed.
func glued(s string, i, n int) bool {
	word := func(c byte) bool { return c >= 0x80 || isDigit(c) || c|0x20 >= 'a' && c|0x20 <= 'z' }
	return i > 0 && word(s[i-1]) || i+n < len(s) && word(s[i+n])
}

func isMaskSeparator(c byte) bool { return c == '.' || c == '-' || c == ' ' }

func digitsOf(s string) string {
	b := make([]byte, 0, 11)
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			b = append(b, s[i])
		}
	}
	return string(b)
}

// validCPF checks the two check digits of an 11-digit CPF (modulo 11, as the
// Receita defines them). All-equal digits (000.000.000-00, 111...) pass the
// arithmetic but are no CPF.
func validCPF(d string) bool {
	if len(d) != 11 || !allDigits(d) || strings.Count(d, d[:1]) == 11 {
		return false
	}
	check := func(n int) byte {
		sum := 0
		for i := 0; i < n; i++ {
			sum += int(d[i]-'0') * (n + 1 - i)
		}
		r := sum * 10 % 11
		if r == 10 {
			r = 0
		}
		return byte('0' + r)
	}
	return d[9] == check(9) && d[10] == check(10)
}
