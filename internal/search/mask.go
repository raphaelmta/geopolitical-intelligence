package search

import "strings"

// MaskCPF masks the CPFs in name, keeping the 6 middle digits (positions 4
// to 9) the way the Receita masks partner CPFs, and the separators:
//
//   - a run of exactly 11 ASCII digits, even glued to letters, becomes
//     ***NNNNNN** (the Receita publishes sole-proprietor company names with
//     the owner's full CPF appended);
//   - ddd S ddd S ddd S dd, where each S is at most one character among
//     . - / and space (mixed allowed), as in 123.456.789-01, 123456789-01,
//     123 456 789 01 or 123.456.789/01, becomes ***.456.789-** and so on.
//
// A separated form counts only when not preceded by a digit or '.' and not
// followed by a digit. Runs of other lengths (a 14-digit CNPJ, a phone
// number), CNPJ spellings (dd.ddd.ddd/dddd-dd, dddddddd/dddd-dd) and digits
// inside longer numbers are left alone. A CPF written any other way is shown
// as the source published it.
func MaskCPF(name string) string {
	if !hasCPF(name) {
		return name
	}
	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); {
		n := cpfAt(name, i)
		if n == 0 {
			b.WriteByte(name[i])
			i++
			continue
		}
		// Keep the separators, mask the first 3 and the last 2 digits.
		digits := 0
		for k := i; k < i+n; k++ {
			c := name[k]
			if isDigit(c) {
				digits++
				if digits <= 3 || digits >= 10 {
					c = '*'
				}
			}
			b.WriteByte(c)
		}
		i += n
	}
	return b.String()
}

// hasCPF reports whether s contains a CPF in any form MaskCPF masks. A query
// containing one skips the exact-name tier.
func hasCPF(s string) bool {
	for i := 0; i < len(s); i++ {
		if cpfAt(s, i) > 0 {
			return true
		}
	}
	return false
}

// cpfAt returns the length of the CPF starting at i, 0 if none. Bytes of
// multi-byte UTF-8 characters are never ASCII digits, so working on bytes is
// safe.
func cpfAt(s string, i int) int {
	if i > 0 && isDigit(s[i-1]) || !isDigit(s[i]) {
		return 0
	}
	j := i
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	if j-i == 11 {
		return 11
	}
	if i > 0 && s[i-1] == '.' {
		return 0
	}
	// Groups of 3, 3, 3 and 2 digits, each but the first after at most one
	// separator.
	k := i
	for g, size := range [...]int{3, 3, 3, 2} {
		if g > 0 && k < len(s) && isCPFSeparator(s[k]) {
			k++
		}
		for range size {
			if k >= len(s) || !isDigit(s[k]) {
				return 0
			}
			k++
		}
	}
	if k < len(s) && isDigit(s[k]) {
		return 0
	}
	return k - i
}

func isCPFSeparator(c byte) bool { return c == '.' || c == '-' || c == '/' || c == ' ' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
