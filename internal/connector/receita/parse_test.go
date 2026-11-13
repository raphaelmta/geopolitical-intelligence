package receita

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type parsed struct {
	line      int64
	fields    []string
	malformed error
}

func readAll(t *testing.T, in string, cols int) []parsed {
	t.Helper()
	rr := newRecordReader(strings.NewReader(in), cols)
	var out []parsed
	for {
		rec, bad, err := rr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		p := parsed{line: rec.Line, malformed: bad}
		if bad == nil {
			for i := 0; i < rec.NumFields(); i++ {
				p.fields = append(p.fields, rec.Field(i))
			}
		}
		out = append(out, p)
	}
}

func TestRecordReaderDialect(t *testing.T) {
	in := "\"a\";\"say \"\"hi\"\"\";\"x\"\n" + // doubled quotes
		"\"b\";\"two\nlines\";\"y\"\n" + // newline inside a quoted field
		"\"c\";\"nul\x00byte\";\"z\"\r\n" + // NUL removed, CR tolerated
		"\"d\";\"\";\"\"" // no final newline
	got := readAll(t, in, 3)
	want := [][]string{{"a", `say "hi"`, "x"}, {"b", "two\nlines", "y"}, {"c", "nulbyte", "z"}, {"d", "", ""}}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].malformed != nil || strings.Join(got[i].fields, "|") != strings.Join(w, "|") {
			t.Errorf("record %d = %+v, want %q", i, got[i], w)
		}
	}
	if got[2].line != 4 || got[3].line != 5 {
		t.Errorf("start lines = %d, %d; want 4, 5 (line numbers are physical)", got[2].line, got[3].line)
	}
}

func TestRecordReaderSkipsMalformedAndResyncs(t *testing.T) {
	in := "\"1\";\"ok\";\"x\"\n" +
		"\"2\";\"stray \"quote\";\"x\"\n" + // stray quote
		"\"3\";\"short\"\n" + // missing column
		"\"4\";\"never closed;\"x\"\n" + // opens a quote that swallows the next line
		"\"5\";\"ok\";\"x\"\n" +
		"\"6\";\"unterminated at EOF\n" +
		"\"7\";\"ok\";\"x\"\n"
	got := readAll(t, in, 3)
	var good []string
	var bad []int64
	for _, p := range got {
		if p.malformed != nil {
			bad = append(bad, p.line)
		} else {
			good = append(good, p.fields[0])
		}
	}
	if strings.Join(good, ",") != "1,5,7" {
		t.Errorf("good records = %v, want 1,5,7", good)
	}
	if len(bad) < 4 || bad[0] != 2 || bad[1] != 3 || bad[2] != 4 {
		t.Errorf("malformed lines = %v, want 2, 3, 4 and the unterminated one", bad)
	}
	if !errors.Is(got[1].malformed, errStrayQuote) || !errors.Is(got[2].malformed, errColumns) {
		t.Errorf("reasons = %v, %v", got[1].malformed, got[2].malformed)
	}
}

func TestRecordReaderBoundsRunawayRecord(t *testing.T) {
	var b strings.Builder
	b.WriteString("\"x\";\"opens and never closes\n")
	for i := 0; i < 40; i++ {
		b.WriteString("\"r\";\"ok\";\"x\"\n")
	}
	got := readAll(t, b.String(), 3)
	good := 0
	for _, p := range got[1:] {
		if p.malformed == nil {
			good++
		}
	}
	if got[0].malformed == nil || good != 40 {
		t.Errorf("first malformed=%v, good after it=%d; want malformed and 40", got[0].malformed, good)
	}
}

func TestLatin1Decoding(t *testing.T) {
	// "JOSÉ CONCEIÇÃO" in ISO-8859-1.
	in := []byte{'J', 'O', 'S', 0xC9, ' ', 'C', 'O', 'N', 'C', 'E', 'I', 0xC7, 0xC3, 'O'}
	if got := latin1(in); got != "JOSÉ CONCEIÇÃO" {
		t.Errorf("latin1 = %q", got)
	}
}

func TestParseDate(t *testing.T) {
	for in, want := range map[string]string{
		"20100315": "2010-03-15", "00000000": "", "0": "", "": "", "20230230": "", "17000101": "", "2010031x": "",
	} {
		got := parseDate(in)
		switch {
		case want == "" && got != nil:
			t.Errorf("parseDate(%q) = %v, want nil", in, got)
		case want != "" && (got == nil || got.Format("2006-01-02") != want):
			t.Errorf("parseDate(%q) = %v, want %s", in, got, want)
		}
	}
}

func TestParseCapital(t *testing.T) {
	cases := []struct {
		in, want string
		out      bool
	}{
		{"120000000000,00", "120000000000.00", false},
		{"0,00", "0.00", false},
		{"15", "15", false},
		{"", "", false},
		{"1.000,00", "", false},
		{"12345678901234567,00", "", true},
	}
	for _, c := range cases {
		got, out := parseCapital(c.in)
		g := ""
		if got != nil {
			g = *got
		}
		if g != c.want || out != c.out {
			t.Errorf("parseCapital(%q) = %q, %v; want %q, %v", c.in, g, out, c.want, c.out)
		}
	}
}

func TestForeignKey(t *testing.T) {
	if got := foreignKey("98990001", "NORTHWIND HOLDINGS LIMITED", "37"); got != "receita:98990001:NORTHWIND HOLDINGS LIMITED" {
		t.Errorf("named = %q", got)
	}
	if got := foreignKey("98990009", "", "38"); got != "receita:98990009:#38" {
		t.Errorf("unnamed = %q", got)
	}
}
