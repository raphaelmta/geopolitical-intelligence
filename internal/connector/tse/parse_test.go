package tse

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHeaderIsReadByName(t *testing.T) {
	header := []string{"EXTRA", "SQ_CANDIDATO", "NR_TURNO"}
	idx, err := columnIndexes(header, []string{"NR_TURNO", "SQ_CANDIDATO"})
	if err != nil || idx[0] != 2 || idx[1] != 1 {
		t.Fatalf("idx = %v, err = %v", idx, err)
	}
	_, err = columnIndexes(header, []string{"SQ_CANDIDATO", "VR_RECEITA", "DT_RECEITA"})
	if err == nil || !strings.Contains(err.Error(), "VR_RECEITA") || !strings.Contains(err.Error(), "DT_RECEITA") {
		t.Errorf("err = %v, want both missing columns named", err)
	}
	_, err = columnIndexes([]string{"SQ_CANDIDATO", "SQ_CANDIDATO"}, []string{"SQ_CANDIDATO"})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate header: err = %v", err)
	}
	// Every supported year has a layout naming every field.
	for _, y := range DefaultYears {
		for _, l := range [][]string{candLayout(y), receiptLayout(y)} {
			for i, n := range l {
				if n == "" {
					t.Errorf("%d: field %d has no column name", y, i)
				}
			}
		}
	}
}

func TestRecordReaderReadsTheTSEDialect(t *testing.T) {
	in := "\"A\";\"B\"\r\n\"1\";\"x\"\"y\"\r\n\"2\";\"bad\"quote\"\r\n\"3\";\"\xe7\xe3o\"\r\n\"4\"\r\n"
	rr := newRecordReader(strings.NewReader(in), 0)
	h, bad, err := rr.Next()
	if err != nil || bad != nil || h.NumFields() != 2 || h.Field(1) != "B" {
		t.Fatalf("header: %v %v", bad, err)
	}
	rr.cols = 2
	var got []string
	for {
		rec, bad, err := rr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if bad != nil {
			got = append(got, "bad@"+strconv.FormatInt(rec.Line, 10))
			continue
		}
		got = append(got, strconv.FormatInt(rec.Line, 10)+":"+rec.Field(1))
	}
	if joined(got) != `2:x"y,bad@3,4:ção,bad@5` {
		t.Errorf("records = %v", got)
	}
}

func TestCleanSentinels(t *testing.T) {
	for in, want := range map[string]string{
		"#NULO": "", "#NULO#": "", "#NE": "", "#NE#": "", "-1": "", "-3": "", "-4": "", " NÃO DIVULGÁVEL ": "",
		"  PT ": "PT", "0": "0",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sigla(" PC do  B "); got != "PC DO B" {
		t.Errorf("sigla = %q", got)
	}
}

func TestParseAmount(t *testing.T) {
	for in, want := range map[string]string{
		"1500,00": "1500.00", "0,5": "0.5", "12": "12", "500,50": "500.50", " 7,10 ": "7.10",
	} {
		got, ok := parseAmount(in)
		if !ok || got != want {
			t.Errorf("parseAmount(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "12a,00", "1.500,00", "-5,00", ",5", "1,234", "12345678901234567,00", "#NULO"} {
		if got, ok := parseAmount(in); ok {
			t.Errorf("parseAmount(%q) = %q, want rejected", in, got)
		}
	}
}

func TestParseDateBR(t *testing.T) {
	if d := parseDateBR("15/09/2022"); d == nil || !d.Equal(time.Date(2022, 9, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date = %v", d)
	}
	for _, in := range []string{"", "#NULO", "31/02/2022", "2022-09-15", "15/09/22", "00/00/0000"} {
		if d := parseDateBR(in); d != nil {
			t.Errorf("parseDateBR(%q) = %v, want nil", in, d)
		}
	}
}

func TestEntryNamesSelectUFAndBROnly(t *testing.T) {
	cases := map[string]bool{
		"consulta_cand_2022_AC.csv":                         true,
		"consulta_cand_2022_BR.csv":                         true,
		"consulta_cand_2022_BRASIL.csv":                     false,
		"consulta_cand_2018_AC.csv":                         false, // another year
		"leiame.pdf":                                        false,
		"dir/consulta_cand_2022_AC.csv":                     false,
		"receitas_candidatos_2022_AC.csv":                   false, // another kind
		"receitas_candidatos_doador_originario_2022_AC.csv": false,
	}
	re := entryPattern(kindCand, 2022)
	for name, want := range cases {
		if got := re.MatchString(name); got != want {
			t.Errorf("%s: match = %v, want %v", name, got, want)
		}
	}
	if !entryPattern(kindReceipt, 2022).MatchString("receitas_candidatos_2022_SP.csv") ||
		entryPattern(kindReceipt, 2022).MatchString("receitas_candidatos_doador_originario_2022_SP.csv") {
		t.Error("receipt entry pattern")
	}
}

// A donation from another candidacy without SQ_CANDIDATO_DOADOR is
// donor_candidate_unpublished, counted once per receipt (here receipt 7 has
// two lines), never loaded.
func TestUnpublishedDonorCandidateCountsPerReceipt(t *testing.T) {
	layout := receiptLayout(2022)
	line := func(receipt, donorSQ string) string {
		f := make([]string, len(layout))
		f[rSQ], f[rOriginCode], f[rOrigin], f[rReceipt], f[rAmount], f[rDate] =
			"10001600001", "10040000", "Recursos de outros candidatos", receipt, "10,00", "01/09/2022"
		f[rDonorSQ] = donorSQ
		for i := range f {
			f[i] = `"` + f[i] + `"`
		}
		return strings.Join(f, ";") + "\r\n"
	}
	header := make([]string, len(layout))
	for i, n := range layout {
		header[i] = `"` + n + `"`
	}
	in := strings.Join(header, ";") + "\r\n" + line("7", "") + line("7", "") + line("8", "") + line("9", "10001600004")
	rr := newRecordReader(strings.NewReader(in), 0)
	if _, _, err := rr.Next(); err != nil {
		t.Fatal(err)
	}
	rr.cols = len(layout)
	idx := make([]int, len(layout))
	for i := range idx {
		idx[i] = i
	}
	s := &rowSource{rr: rr, fileName: "r.csv", idx: idx, year: 2022, kind: kindReceipt, values: make([]any, len(receiptColumns))}
	loaded := 0
	for s.Next() {
		loaded++
	}
	if s.Err() != nil {
		t.Fatal(s.Err())
	}
	if loaded != 1 || s.notLoaded["donor_candidate_unpublished"] != 2 || s.read != 4 {
		t.Fatalf("loaded %d, unpublished %d, read %d; want 1, 2 (receipts 7 and 8), 4", loaded, s.notLoaded["donor_candidate_unpublished"], s.read)
	}
}
