package tse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"geoint/internal/identity"
)

// The TSE CSV dialect (docs/sources/tse.md): a header line, ';' between
// fields, every field in double quotes (inner quotes doubled), CRLF line
// ends, Latin-1 text. Every structural byte is ASCII, so records are split
// on the raw bytes and only the fields that are kept are decoded. The reader
// is the Receita one (internal/connector/receita/parse.go) with a header:
// cols 0 accepts any column count (used for the header line).

const (
	// maxRecordLines bounds how many physical lines one record may span; a
	// stray quote that never closes must not swallow the rest of a file.
	maxRecordLines = 16
	// maxRecordBytes bounds one record (real ones are under 2 KB).
	maxRecordBytes = 64 << 10
)

// Malformed-record reasons. They never carry field content (personal data).
var (
	errStrayQuote   = errors.New("stray quote")
	errUnterminated = errors.New("unterminated quoted field")
	errTooLong      = errors.New("record too long")
	errColumns      = errors.New("wrong number of columns")
	errNeedMore     = errors.New("record continues on the next line")
)

// record is one parsed CSV record. Field i is buf[ends[i-1]:ends[i]], already
// unescaped and with NUL bytes removed, still Latin-1.
type record struct {
	Line int64 // physical line (1-based, the header is line 1) where the record starts
	buf  []byte
	ends []int
}

func (r *record) NumFields() int { return len(r.ends) }

func (r *record) raw(i int) []byte {
	start := 0
	if i > 0 {
		start = r.ends[i-1]
	}
	return r.buf[start:r.ends[i]]
}

// Field returns field i decoded from Latin-1 to UTF-8.
func (r *record) Field(i int) string { return latin1(r.raw(i)) }

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

// recordReader reads records of the dialect from r. A malformed record is
// reported (with its start line) and skipped; reading goes on. When a
// malformed record spanned several physical lines, only its first line is
// dropped and the following lines are parsed again.
type recordReader struct {
	br      *bufio.Reader
	cols    int // expected fields per record; 0 accepts any count
	line    int64
	pending []physLine
	lines   []physLine
	joined  []byte
	rec     record
}

type physLine struct {
	no   int64
	data []byte
	long bool // longer than maxRecordBytes: the rest was dropped
}

func newRecordReader(r io.Reader, cols int) *recordReader {
	return &recordReader{br: bufio.NewReaderSize(r, 1<<20), cols: cols}
}

// Next returns the next record. err is io.EOF at the end, a malformed-record
// reason (with rec.Line set) for a skipped record, or a read error.
// The returned record is valid until the next call.
func (rr *recordReader) Next() (rec *record, malformed error, err error) {
	first, err := rr.readLine()
	if err != nil {
		return nil, nil, err
	}
	rr.lines = append(rr.lines[:0], first)
	size := len(first.data)
	if first.long {
		size = maxRecordBytes + 1
	}
	for {
		rr.join()
		perr := rr.parse()
		if size > maxRecordBytes {
			perr = errTooLong
		}
		if perr == nil && rr.cols > 0 && rr.rec.NumFields() != rr.cols {
			perr = errColumns
		}
		if perr == errNeedMore && len(rr.lines) < maxRecordLines && size <= maxRecordBytes {
			next, err := rr.readLine()
			if err == nil {
				rr.lines = append(rr.lines, next)
				size += len(next.data) + 1
				if next.long {
					size = maxRecordBytes + 1
				}
				continue
			}
			if err != io.EOF {
				return nil, nil, err
			}
			perr = errUnterminated
		} else if perr == errNeedMore {
			perr = errTooLong
		}
		rr.rec.Line = first.no
		if perr != nil {
			if len(rr.lines) > 1 {
				rest := make([]physLine, 0, len(rr.lines)-1+len(rr.pending))
				rest = append(rest, rr.lines[1:]...)
				rr.pending = append(rest, rr.pending...)
			}
			return &rr.rec, perr, nil
		}
		return &rr.rec, nil, nil
	}
}

// readLine returns the next physical line without its '\n' (and without a
// trailing '\r'), in its own buffer.
func (rr *recordReader) readLine() (physLine, error) {
	if len(rr.pending) > 0 {
		l := rr.pending[0]
		rr.pending = rr.pending[1:]
		return l, nil
	}
	var data []byte
	long := false
	for {
		chunk, err := rr.br.ReadSlice('\n')
		if len(data)+len(chunk) <= maxRecordBytes+1 {
			data = append(data, chunk...)
		} else {
			long = true
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(data) == 0 {
			return physLine{}, io.EOF
		}
		if err != nil && err != io.EOF {
			return physLine{}, err
		}
		break
	}
	data = bytes.TrimSuffix(data, []byte{'\n'})
	data = bytes.TrimSuffix(data, []byte{'\r'})
	rr.line++
	return physLine{no: rr.line, data: data, long: long}, nil
}

func (rr *recordReader) join() {
	rr.joined = rr.joined[:0]
	for i, l := range rr.lines {
		if i > 0 {
			rr.joined = append(rr.joined, '\n')
		}
		rr.joined = append(rr.joined, l.data...)
	}
}

// parse splits rr.joined into rr.rec.
func (rr *recordReader) parse() error {
	b := rr.joined
	rr.rec.buf = rr.rec.buf[:0]
	rr.rec.ends = rr.rec.ends[:0]
	i := 0
	for {
		if i < len(b) && b[i] == '"' {
			i++
			closed := false
			for i < len(b) {
				c := b[i]
				if c == '"' {
					if i+1 < len(b) && b[i+1] == '"' {
						rr.rec.buf = append(rr.rec.buf, '"')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				if c != 0 {
					rr.rec.buf = append(rr.rec.buf, c)
				}
				i++
			}
			if !closed {
				return errNeedMore
			}
			rr.rec.ends = append(rr.rec.ends, len(rr.rec.buf))
			if i == len(b) {
				return nil
			}
			if b[i] != ';' {
				return errStrayQuote
			}
			i++
			continue
		}
		// Unquoted field (tolerated): up to the next ';'.
		j := bytes.IndexByte(b[i:], ';')
		end := len(b)
		if j >= 0 {
			end = i + j
		}
		for _, c := range b[i:end] {
			switch c {
			case '"':
				return errStrayQuote
			case 0:
			default:
				rr.rec.buf = append(rr.rec.buf, c)
			}
		}
		rr.rec.ends = append(rr.rec.ends, len(rr.rec.buf))
		if j < 0 {
			return nil
		}
		i = end + 1
	}
}

// fileKind is what a CSV entry holds.
type fileKind int

const (
	kindCand    fileKind = iota // consulta_cand_<year>_<UF>.csv
	kindReceipt                 // receitas_candidatos_<year>_<UF>.csv
)

// entryPattern matches the entries of one year that are loaded: one per UF
// plus BR (two letters). _BRASIL (the aggregate of all of them), the
// original-donor and expense files and the PDFs never match, so no row is
// loaded twice (docs/sources/tse.md).
func entryPattern(k fileKind, year int) *regexp.Regexp {
	prefix := "consulta_cand"
	if k == kindReceipt {
		prefix = "receitas_candidatos"
	}
	return regexp.MustCompile(fmt.Sprintf(`^%s_%d_[A-Z]{2}\.csv$`, prefix, year))
}

// Fields read from consulta_cand, in candLayout order.
const (
	cSQ = iota
	cTurno
	cDate
	cOfficeCode
	cOffice
	cUF
	cUE
	cUEName
	cName
	cBallot
	cCPF
	cParty
	cPartyName
	cResult
	cPartyNumber
	cFields
)

// Fields read from receitas_candidatos, in receiptLayout order.
const (
	rSQ = iota
	rOriginCode
	rOrigin
	rDoc
	rDonor
	rDonorRFB
	rParty
	rPartyName
	rDonorSQ
	rReceipt
	rDate
	rAmount
	rPartyNumber
	rFields
)

// Column names per year. Every year from 2018 to 2026 uses the same names
// (the TSE republished the older years in the current layout; checked on
// 2026-11-11). A year without its own entry uses the newest layout.
var (
	candLayout2018 = []string{
		cSQ: "SQ_CANDIDATO", cTurno: "NR_TURNO", cDate: "DT_ELEICAO", cOfficeCode: "CD_CARGO", cOffice: "DS_CARGO",
		cUF: "SG_UF", cUE: "SG_UE", cUEName: "NM_UE", cName: "NM_CANDIDATO", cBallot: "NM_URNA_CANDIDATO",
		cCPF: "NR_CPF_CANDIDATO", cParty: "SG_PARTIDO", cPartyName: "NM_PARTIDO", cResult: "DS_SIT_TOT_TURNO",
		cPartyNumber: "NR_PARTIDO",
	}
	receiptLayout2018 = []string{
		rSQ: "SQ_CANDIDATO", rOriginCode: "CD_ORIGEM_RECEITA", rOrigin: "DS_ORIGEM_RECEITA",
		rDoc: "NR_CPF_CNPJ_DOADOR", rDonor: "NM_DOADOR", rDonorRFB: "NM_DOADOR_RFB", rParty: "SG_PARTIDO_DOADOR",
		rPartyName: "NM_PARTIDO_DOADOR", rDonorSQ: "SQ_CANDIDATO_DOADOR", rReceipt: "SQ_RECEITA",
		rDate: "DT_RECEITA", rAmount: "VR_RECEITA", rPartyNumber: "NR_PARTIDO_DOADOR",
	}
	candLayouts    = map[int][]string{2018: candLayout2018, 2020: candLayout2018, 2022: candLayout2018, 2024: candLayout2018, 2026: candLayout2018}
	receiptLayouts = map[int][]string{2018: receiptLayout2018, 2020: receiptLayout2018, 2022: receiptLayout2018, 2024: receiptLayout2018, 2026: receiptLayout2018}
)

func candLayout(year int) []string {
	if l, ok := candLayouts[year]; ok {
		return l
	}
	return candLayout2018
}

func receiptLayout(year int) []string {
	if l, ok := receiptLayouts[year]; ok {
		return l
	}
	return receiptLayout2018
}

// columnIndexes maps each wanted column name to its position in header.
// Missing columns (all of them named) and a duplicated wanted column are an
// error; other columns are ignored.
func columnIndexes(header, want []string) ([]int, error) {
	pos := make(map[string]int, len(header))
	dup := map[string]bool{}
	for i, h := range header {
		h = strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF"))
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

// fields reads the wanted columns of a record through the index map.
type fields struct {
	rec *record
	idx []int
}

func (f fields) get(i int) string { return f.rec.Field(f.idx[i]) }

// sentinels are the TSE's markers for blank, unrecorded or withheld values
// (leiame): they all load as empty.
var sentinels = map[string]bool{
	"#NULO": true, "#NULO#": true, "#NE": true, "#NE#": true, "-1": true, "-3": true, "-4": true, "NÃO DIVULGÁVEL": true,
}

// clean trims s and turns sentinels into "".
func clean(s string) string {
	s = strings.TrimSpace(s)
	if sentinels[s] {
		return ""
	}
	return s
}

// sigla is a party's natural key: the acronym upper-cased, single-spaced.
func sigla(s string) string { return strings.Join(strings.Fields(strings.ToUpper(clean(s))), " ") }

// sequence returns a TSE sequential id (digits only) or "".
func sequence(s string) string {
	s = clean(s)
	if s == "" || len(s) > 20 || !allDigits(s) {
		return ""
	}
	return s
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// parseDateBR reads DD/MM/AAAA; anything else (empty, sentinel, impossible
// date) gives nil.
func parseDateBR(s string) *time.Time {
	s = strings.TrimSpace(s)
	if len(s) != 10 || s[2] != '/' || s[5] != '/' {
		return nil
	}
	t, err := time.Parse("02/01/2006", s)
	if err != nil || t.Year() < 1900 || t.Year() > 2100 {
		return nil
	}
	return &t
}

// parseAmount turns "1500,00" into "1500.00", keeping the digits exactly.
// Only a plain non-negative decimal with up to two decimals and at most 16
// integer digits (numeric(18,2)) is accepted.
func parseAmount(s string) (string, bool) {
	s = strings.TrimSpace(s)
	intPart, frac, hasFrac := strings.Cut(s, ",")
	if !allDigits(intPart) || len(strings.TrimLeft(intPart, "0")) > 16 {
		return "", false
	}
	if !hasFrac {
		return intPart, true
	}
	if !allDigits(frac) || len(frac) > 2 {
		return "", false
	}
	return intPart + "." + frac, true
}

// candRow is a parsed consulta_cand row. It has no CPF: only the fragment.
type candRow struct {
	SQ                                     string
	Turno                                  int16
	Date                                   *time.Time
	OfficeCode, Office, UF, City, CityName string
	Name, NameNorm, Ballot, Fragment       string
	Party, PartyName, PartyNameNorm        string
	PartyNumber                            string
	Result                                 string
}

// parseCand reads a candidacy row; reason is set when the row cannot be
// loaded (it is counted as skipped).
func parseCand(f fields) (row candRow, reason string) {
	sq := sequence(f.get(cSQ))
	if sq == "" {
		return candRow{}, "invalid SQ_CANDIDATO"
	}
	turno := clean(f.get(cTurno))
	if turno != "1" && turno != "2" {
		return candRow{}, "invalid NR_TURNO"
	}
	name := clean(f.get(cName))
	partyName := clean(f.get(cPartyName))
	row = candRow{
		SQ: sq, Turno: int16(turno[0] - '0'), Date: parseDateBR(f.get(cDate)),
		OfficeCode: clean(f.get(cOfficeCode)), Office: clean(f.get(cOffice)), UF: clean(f.get(cUF)),
		Name: name, NameNorm: identity.NormalizeName(name), Ballot: clean(f.get(cBallot)),
		Party: sigla(f.get(cParty)), PartyName: partyName, PartyNameNorm: identity.NormalizeName(partyName),
		Result: clean(f.get(cResult)), PartyNumber: sequence(f.get(cPartyNumber)),
	}
	// SG_UE is the municipality code in municipal elections, the UF (or BR) otherwise.
	if ue := clean(f.get(cUE)); allDigits(ue) {
		row.City, row.CityName = ue, clean(f.get(cUEName))
	}
	// The CPF is reduced to its fragment here and never kept.
	row.Fragment, _ = identity.CPFFragment(clean(f.get(cCPF)))
	return row, ""
}

// Donor kinds of a loaded receipt.
const (
	donorPerson    = "person"
	donorCompany   = "company"
	donorParty     = "party"
	donorCandidate = "candidate"
)

// Origins (CD_ORIGEM_RECEITA) and how they load; docs/sources/tse.md.
var origins = map[string]string{
	"10010200": donorPerson,  // Recursos de pessoas físicas
	"10010400": donorPerson,  // Doações pela Internet
	"10010300": donorCompany, // Recursos de pessoas jurídicas (none since 2016)
	"10020000": donorParty,   // Recursos de partido político
	"10040000": donorCandidate,
	"10010100": "own_resources", // Recursos próprios: never a self-loop
	"10020500": "crowdfunding",  // the CNPJ is the platform's
}

// receiptRow is a parsed receitas row that is loaded. It has no CPF or CNPJ:
// only the fragment or the CNPJ base.
type receiptRow struct {
	SQ, ReceiptID, OriginCode, Origin  string
	Kind, Key                          string
	DonorName, DonorNameNorm, Fragment string
	Base, Party, PartyName             string
	PartyNameNorm, DonorSQ             string
	PartyNumber                        string
	Amount                             string
	Date                               *time.Time
}

// parseReceipt reads a receipt row. A row that cannot be read gives reason
// (counted as skipped); a valid row that is not loaded by design gives
// notLoaded, the stats key that counts it.
func parseReceipt(f fields) (row receiptRow, reason, notLoaded string) {
	sq := sequence(f.get(rSQ))
	if sq == "" {
		return receiptRow{}, "invalid SQ_CANDIDATO", ""
	}
	code := strings.TrimSpace(f.get(rOriginCode))
	kind, ok := origins[code]
	if !ok {
		return receiptRow{}, "", "other_origin"
	}
	switch kind {
	case donorPerson, donorCompany, donorParty, donorCandidate:
	default:
		return receiptRow{}, "", kind
	}
	id := sequence(f.get(rReceipt))
	if id == "" {
		return receiptRow{}, "", "no_receipt_id"
	}
	amount, ok := parseAmount(f.get(rAmount))
	if !ok {
		return receiptRow{}, "invalid VR_RECEITA", ""
	}
	row = receiptRow{
		SQ: sq, ReceiptID: id, OriginCode: code, Origin: clean(f.get(rOrigin)), Kind: kind,
		Amount: amount, Date: parseDateBR(f.get(rDate)),
	}
	name := clean(f.get(rDonorRFB))
	if name == "" {
		name = clean(f.get(rDonor))
	}
	switch kind {
	case donorPerson:
		// The CPF is reduced to its fragment here and never kept.
		row.Fragment, _ = identity.CPFFragment(clean(f.get(rDoc)))
		row.DonorName, row.DonorNameNorm = name, identity.NormalizeName(name)
		if row.DonorNameNorm == "" && row.Fragment == "" {
			return receiptRow{}, "", "unidentified_donor"
		}
		row.Key = row.DonorNameNorm + "|" + row.Fragment
	case donorCompany:
		base, ok := identity.CNPJBase(clean(f.get(rDoc)))
		if !ok {
			return receiptRow{}, "", "unidentified_donor"
		}
		row.Base, row.Key = base, base
		row.DonorName, row.DonorNameNorm = name, identity.NormalizeName(name)
	case donorParty:
		// Without a sigla, the party number is mapped to a sigla in SQL
		// (the candidacies of the same year); without either, unknown.
		row.Party = sigla(f.get(rParty))
		if row.Party == "" {
			row.PartyNumber = sequence(f.get(rPartyNumber))
		}
		if row.Party == "" && row.PartyNumber == "" {
			return receiptRow{}, "", "party_unknown"
		}
		row.PartyName = clean(f.get(rPartyName))
		row.PartyNameNorm = identity.NormalizeName(row.PartyName)
		row.Key = row.Party
	case donorCandidate:
		row.DonorSQ = sequence(f.get(rDonorSQ))
		if row.DonorSQ == "" {
			// The TSE published no SQ_CANDIDATO_DOADOR: the donor candidacy
			// cannot be named. The row keeps its candidacy and receipt id so
			// the loader counts it once per receipt (a receipt may span
			// several lines).
			return receiptRow{SQ: sq, ReceiptID: id}, "", "donor_candidate_unpublished"
		}
		row.Key = row.DonorSQ
	}
	return row, "", ""
}
