package receita

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"geoint/internal/identity"
)

// The Receita CSV dialect (docs/sources/receita.md): no header, ';' between
// fields, every field in double quotes with inner quotes doubled, LF line
// ends, Latin-1 text, NUL bytes inside some fields, and newlines inside a few
// quoted fields. Every structural byte (';', '"', '\n') is ASCII, so records
// are split on the raw Latin-1 bytes and only the fields that are kept are
// decoded to UTF-8.

const (
	// maxRecordLines bounds how many physical lines one record may span. Real
	// records span at most two; a stray quote that never closes must not
	// swallow the rest of a file.
	maxRecordLines = 16
	// maxRecordBytes bounds one record (real ones are under 2 KB).
	maxRecordBytes = 64 << 10
)

// errMalformed reasons. They never carry field content (personal data).
var (
	errStrayQuote   = errors.New("stray quote")
	errUnterminated = errors.New("unterminated quoted field")
	errTooLong      = errors.New("record too long")
	errColumns      = errors.New("wrong number of columns")
)

// record is one parsed CSV record. Field i is buf[ends[i-1]:ends[i]], already
// unescaped and with NUL bytes removed, still Latin-1.
type record struct {
	Line int64 // physical line (1-based) where the record starts
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

// recordReader reads records of the Receita dialect from r. A malformed
// record is reported (with its start line) and skipped; reading goes on.
// When a malformed record spanned several physical lines, only its first
// line is dropped and the following lines are parsed again, so one bad quote
// costs one line, never the rest of the file.
type recordReader struct {
	br      *bufio.Reader
	cols    int
	line    int64      // physical lines handed out so far
	pending []physLine // lines pushed back after a malformed multi-line record
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
		if perr == nil && rr.rec.NumFields() != rr.cols {
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
			// Re-read everything after the first line as fresh records.
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
// trailing '\r'), in its own buffer (it stays valid while the record that
// holds it is parsed and when it is pushed back).
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

var errNeedMore = errors.New("record continues on the next line")

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

// Column counts and positions (0-based) from docs/sources/receita.md.
const (
	empresaCols = 7
	estabCols   = 30
	socioCols   = 11
)

// empresaRow is a parsed Empresas row.
type empresaRow struct {
	Base, Name, NameNorm, LegalNature, Size string
	Capital                                 *string // numeric text, nil when absent or out of range
	CapitalOutOfRange                       bool
}

func parseEmpresa(r *record) (empresaRow, bool) {
	base, ok := identity.CNPJBase(r.Field(0))
	if !ok {
		return empresaRow{}, false
	}
	name := strings.TrimSpace(r.Field(1))
	row := empresaRow{
		Base: base, Name: name, NameNorm: identity.NormalizeName(name),
		LegalNature: strings.TrimSpace(r.Field(2)), Size: strings.TrimSpace(r.Field(5)),
	}
	row.Capital, row.CapitalOutOfRange = parseCapital(r.Field(4))
	return row, true
}

// parseCapital turns "120000000000,00" into "120000000000.00". Values that
// are not a plain decimal give nil; values that do not fit numeric(18,2)
// (more than 16 integer digits) give nil and outOfRange.
func parseCapital(s string) (v *string, outOfRange bool) {
	s = strings.TrimSpace(s)
	intPart, frac, _ := strings.Cut(s, ",")
	if intPart == "" || !allDigits(intPart) || !allDigits(frac) {
		return nil, false
	}
	if len(strings.TrimLeft(intPart, "0")) > 16 {
		return nil, true
	}
	out := intPart
	if frac != "" {
		out += "." + frac
	}
	return &out, false
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// estabRow is a parsed Estabelecimentos row.
type estabRow struct {
	Base, Status, UF, City, CNAE string
	HeadOffice                   bool
}

func parseEstab(r *record) (estabRow, bool) {
	base, ok := identity.CNPJBase(r.Field(0))
	if !ok {
		return estabRow{}, false
	}
	return estabRow{
		Base:       base,
		HeadOffice: strings.TrimSpace(r.Field(3)) == "1",
		Status:     strings.TrimSpace(r.Field(5)),
		CNAE:       strings.TrimSpace(r.Field(11)),
		UF:         strings.TrimSpace(r.Field(19)),
		City:       strings.TrimSpace(r.Field(20)),
	}, true
}

// Partner types (identificador_socio).
const (
	partnerCompany = "1"
	partnerPerson  = "2"
	partnerForeign = "3"
)

// socioRow is a parsed Socios row.
type socioRow struct {
	Base, Type, Name, NameNorm string
	PartnerBase                string // company partners
	CPFFragment                string // person partners; "" when masked out or invalid
	ForeignKey                 string // foreign partners
	Qual                       string
	ValidFrom                  *time.Time
	HasRep                     bool
	RepName, RepNameNorm       string
	RepFragment, RepQual       string
}

// socioOutcome says why a Socios row cannot be loaded.
type socioOutcome int

const (
	socioOK socioOutcome = iota
	socioNoCompany
	// socioUnidentified: the partner cannot be identified (unknown type, or a
	// company partner without a usable CNPJ base).
	socioUnidentified
)

func parseSocio(r *record) (socioRow, socioOutcome) {
	base, ok := identity.CNPJBase(r.Field(0))
	if !ok {
		return socioRow{}, socioNoCompany
	}
	name := strings.TrimSpace(r.Field(2))
	row := socioRow{
		Base: base, Type: strings.TrimSpace(r.Field(1)), Name: name, NameNorm: identity.NormalizeName(name),
		Qual: strings.TrimSpace(r.Field(4)), ValidFrom: parseDate(r.Field(5)),
	}
	switch row.Type {
	case partnerCompany:
		pb, ok := identity.CNPJBase(r.Field(3))
		if !ok {
			return socioRow{}, socioUnidentified
		}
		row.PartnerBase = pb
	case partnerPerson:
		row.CPFFragment, _ = identity.CPFFragment(r.Field(3))
	case partnerForeign:
		row.ForeignKey = foreignKey(base, row.NameNorm, row.Qual)
	default:
		return socioRow{}, socioUnidentified
	}
	repName := strings.TrimSpace(r.Field(8))
	row.RepFragment, _ = identity.CPFFragment(r.Field(7))
	row.RepName, row.RepNameNorm = repName, identity.NormalizeName(repName)
	row.HasRep = row.RepNameNorm != "" || row.RepFragment != ""
	if row.HasRep {
		row.RepQual = strings.TrimSpace(r.Field(9))
	} else {
		row.RepName, row.RepNameNorm = "", ""
	}
	return row, socioOK
}

// foreignKey is the natural key of a foreign partner entity. Foreign partners
// have no document, so they are identified by name within one company. An
// unnamed one is keyed by its qualification code instead ("#" never occurs
// in a normalized name), so two unnamed foreign partners of one company with
// different qualifications stay apart; with the same code they are one entity.
func foreignKey(base, nameNorm, qual string) string {
	if nameNorm != "" {
		return "receita:" + base + ":" + nameNorm
	}
	return "receita:" + base + ":#" + qual
}

// parseDate reads AAAAMMDD. Empty, zero ("0", "00000000") and impossible
// dates give nil.
func parseDate(s string) *time.Time {
	s = strings.TrimSpace(s)
	if len(s) != 8 || !allDigits(s) {
		return nil
	}
	t, err := time.Parse("20060102", s)
	if err != nil || t.Year() < 1800 || t.Year() > 2100 {
		return nil
	}
	return &t
}
