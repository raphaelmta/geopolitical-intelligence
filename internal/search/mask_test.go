package search_test

import (
	"testing"

	"geoint/internal/search"
)

func TestMaskCPF(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"JOANA FICTICIA":                     "JOANA FICTICIA",
		"JOANA FICTICIA 12345678901":         "JOANA FICTICIA ***456789**",
		"12345678901 JOANA FICTICIA":         "***456789** JOANA FICTICIA",
		"12345678901":                        "***456789**",
		"A 12345678901 B 98765432100 C":      "A ***456789** B ***654321** C",
		"10 DIGITS 1234567890":               "10 DIGITS 1234567890",
		"12 DIGITS 123456789012":             "12 DIGITS 123456789012",
		"CNPJ 12345678000190":                "CNPJ 12345678000190",
		"GLUED ABC12345678901XYZ":            "GLUED ABC***456789**XYZ",
		"AÇÚCAR 12345678901 Ç":               "AÇÚCAR ***456789** Ç",
		"TWO RUNS 12345678901-98765432100":   "TWO RUNS ***456789**-***654321**",
		"MARIA12345678901":                   "MARIA***456789**",
		"JOANA 123.456.789-01":               "JOANA ***.456.789-**",
		"123.456.789-01 JOANA":               "***.456.789-** JOANA",
		"A 123.456.789-01, B 987.654.321-00": "A ***.456.789-**, B ***.654.321-**",
		"CNPJ 12.345.678/0001-90":            "CNPJ 12.345.678/0001-90",
		"LONGER 1123.456.789-01":             "LONGER 1123.456.789-01",
		"LONGER 123.456.789-012":             "LONGER 123.456.789-012",
		"DOTTED 9.123.456.789-01":            "DOTTED 9.123.456.789-01",
		"DASHED 123456789-01":                "DASHED ***456789-**",
		"123456789-01 START":                 "***456789-** START",
		"SPACED 123 456 789 01":              "SPACED *** 456 789 **",
		"SPACED END 123 456 789 01 X":        "SPACED END *** 456 789 ** X",
		"LONGER 1123456789-01":               "LONGER 1123456789-01",
		"LONGER 123456789-012":               "LONGER 123456789-012",
		"DOT 9.123456789-01":                 "DOT 9.123456789-01",
		"LONGER 123 456 789 012":             "LONGER 123 456 789 012",
		"DOUBLE  123  456  789  01":          "DOUBLE  123  456  789  01",
		"CNPJ DIGITS 12345678000190":         "CNPJ DIGITS 12345678000190",
		"SLASH 123.456.789/01":               "SLASH ***.456.789/**",
		"DASHES 123-456-789-01":              "DASHES ***-456-789-**",
		"DOT SPACE 123.456.789 01":           "DOT SPACE ***.456.789 **",
		"MIXED 123 456.789/01":               "MIXED *** 456.789/**",
		"CNPJ SLASH 12345678/0001-90":        "CNPJ SLASH 12345678/0001-90",
		"CNPJ SPACED 12 345 678 0001 90":     "CNPJ SPACED 12 345 678 0001 90",
		"PHONE 11 91234-5678":                "PHONE 11 91234-5678",
	}
	for in, want := range cases {
		if got := search.MaskCPF(in); got != want {
			t.Errorf("MaskCPF(%q) = %q, want %q", in, got, want)
		}
	}
}
