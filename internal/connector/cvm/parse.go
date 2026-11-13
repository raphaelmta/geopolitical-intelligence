package cvm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"geoint/internal/identity"
)

// The CVM CSV dialect (docs/sources/cvm.md): a header line, ';' between
// fields, no quoting at all (a double quote is ordinary text: real names
// contain `("CPFL Brasil")`), CRLF line ends, Latin-1 text. One physical
// line is one record; a line with another field count than the header is
// malformed.

// maxLineBytes bounds one record (real ones are under 1 KB).
const maxLineBytes = 64 << 10

// maxLoggedMalformed bounds the malformed-row warnings logged per entry.
const maxLoggedMalformed = 5

// entryName is the shareholding entry of a year's FRE zip.
func entryName(year int) string { return fmt.Sprintf("fre_cia_aberta_posicao_acionaria_%d.csv", year) }

// Columns read from the shareholding entry, in columnNames order.
const (
	colCompany = iota
	colRefDate
	colVersion
	colDocID
	colCompanyName
	colHolderID
	colHolder
	colHolderType
	colHolderDoc
	colRelatedID
	colPctON
	colPctPN
	colPctTotal
	colNationality
	colAbroad
	colController
	numColumns
)

// columnNames are the header names of the columns read (identical in the
// 2025 and 2026 files, checked on 2026-11-12). Every one is required.
var columnNames = []string{
	colCompany: "CNPJ_Companhia", colRefDate: "Data_Referencia", colVersion: "Versao", colDocID: "ID_Documento",
	colCompanyName: "Nome_Companhia", colHolderID: "ID_Acionista", colHolder: "Acionista", colHolderType: "Tipo_Pessoa_Acionista",
	colHolderDoc: "CPF_CNPJ_Acionista", colRelatedID: "ID_Acionista_Relacionado",
	colPctON: "Percentual_Acao_Ordinaria_Circulacao", colPctPN: "Percentual_Acao_Preferencial_Circulacao",
	colPctTotal: "Percentual_Total_Acoes_Circulacao", colNationality: "Nacionalidade",
	colAbroad: "Residente_Exterior", colController: "Acionista_Controlador",
}

// columnIndexes maps each wanted column name to its position in header.
// Missing columns (all of them named) and a duplicated wanted column are an
// error; other columns are ignored.
func columnIndexes(header, want []string) ([]int, error) {
	pos := make(map[string]int, len(header))
	dup := map[string]bool{}
	for i, h := range header {
		h = strings.TrimSpace(h)
		if _, ok := pos[h]; ok {
			dup[h] = true
		}
		pos[h] = i
	}
	out := make([]int, len(want))
	var missing []string
	for i, w := range want {
		if dup[w] {
			return nil, fmt.Errorf("column %s appears twice in the header", w)
		}
		p, ok := pos[w]
		if !ok {
			missing = append(missing, w)
			continue
		}
		out[i] = p
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing column(s) %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// latin1 decodes ISO-8859-1 bytes to a UTF-8 string: each byte is the code point.
func latin1(b []byte) string {
	ascii := true
	for _, c := range b {
		if c >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return string(b)
	}
	out := make([]byte, 0, len(b)+len(b)/4)
	for _, c := range b {
		out = utf8.AppendRune(out, rune(c))
	}
	return string(out)
}

// holderKind is what a shareholder line identifies.
type holderKind int

const (
	holderNone    holderKind = iota
	holderCompany            // a legal entity with a CNPJ
	holderPerson             // an individual (person record)
	holderForeign            // a foreign holder without a Brazilian document
)

// formKey identifies one delivered form (FRE) of a company. Year is the
// FRE zip the line was read from.
type formKey struct {
	RefDate string // Data_Referencia, AAAA-MM-DD
	Version int    // Versao
	DocID   int64  // ID_Documento
	Year    int
}

// newer reports whether a was delivered after b: the later reference date,
// then the higher version, then the higher document id, then the later zip.
func (a formKey) newer(b formKey) bool {
	if a.RefDate != b.RefDate {
		return a.RefDate > b.RefDate
	}
	if a.Version != b.Version {
		return a.Version > b.Version
	}
	if a.DocID != b.DocID {
		return a.DocID > b.DocID
	}
	return a.Year > b.Year
}

// holderRow is a parsed shareholding line. It has no CPF: only the fragment.
type holderRow struct {
	Line        int64
	Base        string // CNPJ base of the listed company
	CompanyName string
	Form        formKey
	// ID is ID_Acionista. ParentID is ID_Acionista_Relacionado: 0 for a
	// direct holder of the listed company; otherwise the line describes a
	// holder of the holder on line ParentID of the same form (the indirect
	// control chain).
	ID, ParentID int64
	// NotLoaded is the stats key of a line that is not a loadable
	// shareholder by design (aggregate_*, unidentified_shareholder);
	// Defect the malformed reason of a line that would be.
	NotLoaded, Defect string
	// PercentOK: Percent holds a valid total percentage (also set on
	// aggregate lines, which count in an indirect group's sum).
	PercentOK bool

	Kind                          holderKind
	Name, NameNorm                string
	HolderBase, Fragment          string
	Percent, PercentON, PercentPN string // canonical decimals; ON/PN "" when blank
	Controller                    bool
}

// identityKey is what makes two lines of one form the same shareholder.
func (r *holderRow) identityKey() string {
	switch r.Kind {
	case holderCompany:
		return "c:" + r.HolderBase
	case holderForeign:
		return "f:" + r.NameNorm
	default:
		return "p:" + r.NameNorm + "|" + r.Fragment
	}
}

// parseEntry reads one shareholding CSV. It returns the lines that identify
// their form (malformed lines that cannot are counted by reason in
// malformed and logged without content), or an error for a missing header,
// a missing required column or a read error.
func parseEntry(r io.Reader, name string, year int) (rows []holderRow, malformed map[string]int64, err error) {
	lr := &lineReader{br: bufio.NewReaderSize(r, 1<<20)}
	_, head, long, err := lr.next()
	head = bytes.TrimPrefix(head, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM, before Latin-1 decoding
	if errors.Is(err, io.EOF) {
		return nil, nil, errors.New("empty file (no header)")
	}
	if err != nil {
		return nil, nil, err
	}
	if long {
		return nil, nil, errors.New("header line too long")
	}
	header := strings.Split(latin1(head), ";")
	idx, err := columnIndexes(header, columnNames)
	if err != nil {
		return nil, nil, err
	}
	malformed = map[string]int64{}
	logged := 0
	fields := make([][]byte, 0, len(header))
	for {
		no, data, long, err := lr.next()
		if errors.Is(err, io.EOF) {
			return rows, malformed, nil
		}
		if err != nil {
			return nil, nil, err
		}
		reason := ""
		var row holderRow
		if long {
			reason = "malformed_line"
		} else {
			fields = splitFields(fields[:0], data)
			if len(fields) != len(header) {
				reason = "malformed_columns"
			} else {
				row, reason = parseRow(fields, idx, year)
			}
		}
		if reason != "" {
			malformed[reason]++
			if logged < maxLoggedMalformed {
				logged++
				// Never the row content: it holds personal data.
				slog.Warn("cvm: skipping malformed row", "file", name, "line", no, "reason", reason)
			}
			continue
		}
		row.Line = no
		rows = append(rows, row)
	}
}

func splitFields(dst [][]byte, line []byte) [][]byte {
	for {
		i := bytes.IndexByte(line, ';')
		if i < 0 {
			return append(dst, line)
		}
		dst = append(dst, line[:i])
		line = line[i+1:]
	}
}

// lineReader returns physical lines without their CRLF or LF, numbered from
// 1 (the header). A line over maxLineBytes is reported as long, truncated.
type lineReader struct {
	br   *bufio.Reader
	line int64
	buf  []byte
}

func (lr *lineReader) next() (no int64, data []byte, long bool, err error) {
	lr.buf = lr.buf[:0]
	for {
		chunk, err := lr.br.ReadSlice('\n')
		if len(lr.buf)+len(chunk) <= maxLineBytes+2 {
			lr.buf = append(lr.buf, chunk...)
		} else {
			long = true
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(lr.buf) == 0 && !long {
			return 0, nil, false, io.EOF
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, nil, false, err
		}
		break
	}
	data = bytes.TrimSuffix(lr.buf, []byte{'\n'})
	data = bytes.TrimSuffix(data, []byte{'\r'})
	lr.line++
	return lr.line, data, long, nil
}

// parseRow reads one line. reason is set when the line cannot even be
// attributed to a form (it is malformed and counted as skipped); defects of
// a line that would be loaded are left in row.Defect, because only lines of
// a company's latest form count as malformed.
func parseRow(f [][]byte, idx []int, year int) (row holderRow, reason string) {
	get := func(c int) string { return strings.TrimSpace(latin1(f[idx[c]])) }
	base, ok := identity.CNPJBase(get(colCompany))
	if !ok {
		return holderRow{}, "malformed_cnpj"
	}
	ref := get(colRefDate)
	if t, err := time.Parse(time.DateOnly, ref); err != nil || t.Year() < 1990 || t.Year() > 2100 {
		return holderRow{}, "malformed_date"
	}
	version, err := strconv.Atoi(get(colVersion))
	if err != nil || version < 0 || version > 32767 {
		return holderRow{}, "malformed_version"
	}
	docID, err := strconv.ParseInt(get(colDocID), 10, 64)
	if err != nil || docID <= 0 {
		return holderRow{}, "malformed_document"
	}
	id, err := strconv.ParseInt(get(colHolderID), 10, 64)
	if err != nil || id <= 0 {
		return holderRow{}, "malformed_shareholder_id"
	}
	var parent int64
	if p := get(colRelatedID); p != "" {
		if parent, err = strconv.ParseInt(p, 10, 64); err != nil || parent <= 0 {
			return holderRow{}, "malformed_shareholder_id"
		}
	}
	row = holderRow{Base: base, CompanyName: get(colCompanyName), Form: formKey{RefDate: ref, Version: version, DocID: docID, Year: year},
		ID: id, ParentID: parent}
	row.Name = get(colHolder)
	row.NameNorm = identity.NormalizeName(row.Name)
	row.Percent, row.PercentOK = canonicalPercent(get(colPctTotal))
	doc := get(colHolderDoc)
	noDoc := !hasNonZeroDigit(doc)
	if noDoc {
		if agg := aggregate(row.Name); agg != "" {
			row.NotLoaded = agg
			return row, ""
		}
	}
	if !row.PercentOK {
		row.Defect = "malformed_percent"
		return row, ""
	}
	for _, p := range []struct {
		col int
		dst *string
	}{{colPctON, &row.PercentON}, {colPctPN, &row.PercentPN}} {
		if s := get(p.col); s != "" {
			if *p.dst, ok = canonicalPercent(s); !ok {
				row.Defect = "malformed_percent"
				return row, ""
			}
		}
	}
	row.Controller = strings.EqualFold(get(colController), "S")
	nat := identity.NormalizeName(get(colNationality))
	foreign := strings.EqualFold(get(colAbroad), "S") || (nat != "" && nat != "BRASIL")
	switch strings.ToUpper(get(colHolderType)) {
	case "PJ":
		switch {
		case !noDoc:
			b, ok := identity.CNPJBase(doc)
			if !ok {
				row.Defect = "malformed_cnpj"
				return row, ""
			}
			row.Kind, row.HolderBase = holderCompany, b
		case foreign:
			row.Kind = holderForeign
		}
	case "PF":
		if noDoc && foreign {
			row.Kind = holderForeign
			break
		}
		// The CPF is reduced to its fragment here and never kept.
		row.Kind = holderPerson
		row.Fragment, _ = identity.CPFFragment(doc)
		if row.NameNorm == "" && row.Fragment == "" {
			row.Kind = holderNone
		}
	}
	if row.Kind == holderForeign && row.NameNorm == "" {
		row.Kind = holderNone
	}
	if row.Kind == holderNone {
		row.NotLoaded = "unidentified_shareholder"
	}
	return row, ""
}

// foreignKey is the natural key of a foreign shareholder entity. Foreign
// holders have no Brazilian document, so, as for the Receita's foreign
// partners, they are identified by name within the company they hold (the
// listed company, or the holding of an indirect line).
func foreignKey(base, nameNorm string) string { return "cvm:" + base + ":" + nameNorm }

// hasNonZeroDigit reports whether a document has a digit other than 0 (or a
// letter: alphanumeric CNPJs). Empty and all-zero documents
// ("00.000.000/0000-00", the CVM's filler for foreign holders) have none.
func hasNonZeroDigit(s string) bool {
	for _, r := range s {
		if (r >= '1' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			return true
		}
	}
	return false
}

// aggregate returns the stats key of a line that totals holders instead of
// naming one ("Outros", "Ações Tesouraria", "Total"), or "". It is applied
// only to lines without a document.
func aggregate(name string) string {
	n := identity.NormalizeName(name)
	switch {
	case n == "OUTROS" || strings.HasPrefix(n, "OUTROS "):
		return "aggregate_outros"
	case n == "ACOES TESOURARIA" || n == "ACOES EM TESOURARIA":
		return "aggregate_tesouraria"
	case n == "TOTAL":
		return "aggregate_total"
	}
	return ""
}

// canonicalPercent turns a percentage of the file ("51.370000"; a decimal
// comma is accepted too) into its canonical decimal string ("51.37"),
// keeping the digits exactly. Only a plain non-negative decimal with at
// most 3 integer digits, at most 10 decimals and a value of at most 100 is
// accepted.
func canonicalPercent(s string) (string, bool) {
	s = strings.TrimSpace(s)
	intPart, frac, hasFrac := strings.Cut(s, ".")
	if !hasFrac {
		intPart, frac, hasFrac = strings.Cut(s, ",")
	}
	if !allDigits(intPart) || len(intPart) > 3 {
		return "", false
	}
	if hasFrac && (!allDigits(frac) || len(frac) > 10) {
		return "", false
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	frac = strings.TrimRight(frac, "0")
	n, _ := strconv.Atoi(intPart)
	if n > 100 || (n == 100 && frac != "") {
		return "", false
	}
	if frac == "" {
		return intPart, true
	}
	return intPart + "." + frac, true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
