package senado

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"geoint/internal/connector/legis"
)

// listPrefix is the JSON pointer of the senators of a legislature list.
const listPrefix = "/ListaParlamentarLegislatura/Parlamentares/Parlamentar"

// senators reads senador/lista/legislatura/{n}: the senators, undecoded.
func senators(file string, b []byte) (legis.Many[json.RawMessage], error) {
	var v struct {
		Lista *struct {
			Parlamentares *struct {
				Parlamentar *legis.Many[json.RawMessage] `json:"Parlamentar"`
			} `json:"Parlamentares"`
		} `json:"ListaParlamentarLegislatura"`
	}
	if err := legis.Decode(file, b, &v); err != nil {
		return legis.Many[json.RawMessage]{}, err
	}
	if v.Lista == nil || v.Lista.Parlamentares == nil || v.Lista.Parlamentares.Parlamentar == nil {
		return legis.Many[json.RawMessage]{}, &legis.ShapeError{File: file, What: "no ListaParlamentarLegislatura.Parlamentares.Parlamentar"}
	}
	return *v.Lista.Parlamentares.Parlamentar, nil
}

type legislatureRef struct {
	NumeroLegislatura string `json:"NumeroLegislatura"`
	DataInicio        string `json:"DataInicio"`
	DataFim           string `json:"DataFim"`
}

type exercise struct {
	DataInicio string `json:"DataInicio"`
	DataFim    string `json:"DataFim"`
	Causa      string `json:"SiglaCausaAfastamento"`
}

type mandate struct {
	Codigo        string          `json:"CodigoMandato"`
	Participacao  string          `json:"DescricaoParticipacao"`
	UfParlamentar string          `json:"UfParlamentar"`
	Primeira      *legislatureRef `json:"PrimeiraLegislaturaDoMandato"`
	Segunda       *legislatureRef `json:"SegundaLegislaturaDoMandato"`
	Exercicios    *struct {
		Exercicio legis.Many[exercise] `json:"Exercicio"`
	} `json:"Exercicios"`
}

type senatorItem struct {
	Identificacao *struct {
		Codigo *string `json:"CodigoParlamentar"`
		Nome   *string `json:"NomeCompletoParlamentar"`
	} `json:"IdentificacaoParlamentar"`
	Mandatos *struct {
		Mandato legis.Many[mandate] `json:"Mandato"`
	} `json:"Mandatos"`
}

func (m mandate) exercises() legis.Many[exercise] {
	if m.Exercicios == nil {
		return legis.Many[exercise]{}
	}
	return m.Exercicios.Exercicio
}

func (s senatorItem) mandates() legis.Many[mandate] {
	if s.Mandatos == nil {
		return legis.Many[mandate]{}
	}
	return s.Mandatos.Mandato
}

// code reads the senator's code; false when the item has none or it is
// not a number.
func (s senatorItem) code() (string, bool) {
	if s.Identificacao == nil || s.Identificacao.Codigo == nil {
		return "", false
	}
	c := strings.TrimSpace(*s.Identificacao.Codigo)
	if c == "" || len(c) > 12 || strings.Trim(c, "0123456789") != "" {
		return "", false
	}
	n, _ := strconv.ParseInt(c, 10, 64)
	if n <= 0 {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

func (s senatorItem) inOffice() bool {
	for _, m := range s.mandates().Items {
		if len(m.exercises().Items) > 0 {
			return true
		}
	}
	return false
}

// affiliation is one party membership of a senator.
type affiliation struct {
	party, name string
	from        time.Time
	to          *time.Time
}

// affiliations reads senador/{código}/filiacoes, ordered by start. reason
// is set when one is malformed (the senator is then skipped).
func affiliations(file string, b []byte) (out []affiliation, reason string, err error) {
	var v struct {
		F *struct {
			Parlamentar *struct {
				Filiacoes *struct {
					Filiacao legis.Many[struct {
						Partido *struct {
							Sigla string `json:"SiglaPartido"`
							Nome  string `json:"NomePartido"`
						} `json:"Partido"`
						DataFiliacao    string `json:"DataFiliacao"`
						DataDesfiliacao string `json:"DataDesfiliacao"`
					}] `json:"Filiacao"`
				} `json:"Filiacoes"`
			} `json:"Parlamentar"`
		} `json:"FiliacaoParlamentar"`
	}
	if err := legis.Decode(file, b, &v); err != nil {
		return nil, "", err
	}
	if v.F == nil || v.F.Parlamentar == nil {
		return nil, "", &legis.ShapeError{File: file, What: "no FiliacaoParlamentar.Parlamentar"}
	}
	if v.F.Parlamentar.Filiacoes == nil {
		return nil, "", nil
	}
	for _, f := range v.F.Parlamentar.Filiacoes.Filiacao.Items {
		if f.Partido == nil || strings.TrimSpace(f.Partido.Sigla) == "" {
			return nil, "no_party", nil
		}
		a := affiliation{party: legis.PartyKey(f.Partido.Sigla), name: strings.TrimSpace(f.Partido.Nome)}
		var ok bool
		if a.from, ok = legis.ParseDate(f.DataFiliacao); !ok {
			return nil, "bad_date", nil
		}
		if strings.TrimSpace(f.DataDesfiliacao) != "" {
			to, ok := legis.ParseDate(f.DataDesfiliacao)
			if !ok || to.Before(a.from) {
				return nil, "bad_date", nil
			}
			to = to.AddDate(0, 0, 1) // DataDesfiliacao is the last day: stored exclusive
			a.to = &to
		}
		if a.party == "" {
			a.name = ""
		}
		out = append(out, a)
	}
	slices.SortStableFunc(out, func(x, y affiliation) int { return x.from.Compare(y.from) })
	// An affiliation ends, at the latest, when the next one starts: a change
	// on day D (old ends D, new starts D) stores D, and one without an end
	// that is not the last ends on the change date.
	for i := range out[:max(0, len(out)-1)] {
		if next := out[i+1].from; out[i].to == nil || next.Before(*out[i].to) {
			out[i].to = &next
		}
	}
	return out, "", nil
}

// Exercise-end causes (SiglaCausaAfastamento), exactly as published
// (docs/sources/senado.md). A titular's exercise ended by a leave cause is
// followed by a licenciado stretch until his next exercise; a definitive
// cause ends the seat-holding. Substitutes' exercises end with any known
// cause. Any other cause is never guessed: unknown_leave_cause, and the
// stretch just ends.
var (
	leaveCauses = map[string]bool{
		"LCS": true, // Licença com convocação de suplente (sup 120 dias)
		"AFO": true, // Afastamento do exercício (ministro, secretário...)
		"LP":  true, // Licença Particular
		"LS":  true, // Licença saúde
		"LSP": true, // Licença Saúde-Particular
	}
	definitiveCauses = map[string]bool{
		"REN": true, // Renúncia
		"FAL": true, // Falecimento
		"CAS": true, // Cassação de registro/diploma pela Justiça Eleitoral
		"TER": true, // Término do mandato
	}
	returnOfTitular = "RET" // Retorno do titular (ends a substitute's exercise)
)

// period is a stretch of seat-holding (one status) of a mandate inside one
// legislature, before the split by party; to is EXCLUSIVE.
type period struct {
	status string
	from   time.Time
	to     *time.Time
	ref    string
}

type dated struct {
	k     int
	from  time.Time
	last  *time.Time // DataFim: the last day in exercise (inclusive)
	cause string
}

// span is one exercise of a substitute: from, the exclusive end (nil while
// in exercise) and the cause that ended it.
type span struct {
	from  time.Time
	to    *time.Time
	cause string
}

// substitutes are the exercises of the substitutes of each mandate
// (CodigoMandato) in one legislature list.
type substitutes map[string][]span

// indexSubstitutes collects the substitutes' exercises of a list;
// unreadable items or dates are left out (the item itself is skipped and
// counted when loaded).
func indexSubstitutes(list legis.Many[json.RawMessage]) substitutes {
	out := substitutes{}
	for _, item := range list.Items {
		var s senatorItem
		if json.Unmarshal(item, &s) != nil {
			continue
		}
		for _, m := range s.mandates().Items {
			code := strings.TrimSpace(m.Codigo)
			if code == "" || strings.EqualFold(strings.TrimSpace(m.Participacao), "Titular") {
				continue
			}
			for _, e := range m.exercises().Items {
				from, ok := legis.ParseDate(e.DataInicio)
				if !ok {
					continue
				}
				sp := span{from: from, cause: strings.TrimSpace(e.Causa)}
				if strings.TrimSpace(e.DataFim) != "" {
					t, ok := legis.ParseDate(e.DataFim)
					if !ok {
						continue
					}
					t = t.AddDate(0, 0, 1)
					sp.to = &t
				}
				out[code] = append(out[code], sp)
			}
		}
	}
	return out
}

// corroborate looks for exercises of the mandate's substitutes that started
// on or after the titular's last exercise began (since) and still run on
// leaveFrom. open: one is still in exercise, so the leave is current.
// Otherwise bound is the latest end among them (the best exact bound the
// API gives) and cause the cause that ended that exercise, or bound is nil
// when there is none (the leave is not corroborated).
func (s substitutes) corroborate(mandate string, since, leaveFrom time.Time) (open bool, bound *time.Time, cause string) {
	for _, sp := range s[mandate] {
		if sp.from.Before(since) {
			continue
		}
		if sp.to == nil {
			return true, nil, ""
		}
		if sp.to.After(leaveFrom) && (bound == nil || sp.to.After(*bound)) {
			t := *sp.to
			bound, cause = &t, sp.cause
		}
	}
	return false, bound, cause
}

// substituteGap is how long after the last substitute's exercise ended a
// leave without return stays open. Substitutes often follow each other with
// a gap of a few days (the next one takes office after the previous one's
// exercise ends, e.g. 5 days later): closing the leave at the first
// substitute's end would leave a closed licenciado row beside the new open
// one once the next substitute appears. Within the gap the leave is kept
// open (as of the collection date it is still most likely current); after
// it, or when the substitute's exercise ended with a terminal cause, it is
// bounded by that end.
const substituteGap = 30 * 24 * time.Hour

// terminalSubstituteEnd: a substitute's exercise that ended this way ends
// the leave itself: the titular came back (RET) or the seat ended
// (definitiveCauses).
func terminalSubstituteEnd(cause string) bool {
	return cause == returnOfTitular || definitiveCauses[cause]
}

// periods returns the seat-holding stretches of the senator's mandates
// that fall in legislature n, clipped to it, with the UF of each; reason is
// set when an item is malformed. Exercises are exercicio (titular) or
// suplente_exercicio; a titular's leave between two exercises is
// licenciado. A leave after the titular's last exercise is licenciado only
// when a substitute's exercise corroborates it: open while that exercise
// runs; once the last such exercise ended, bounded by that end when it ended
// with a terminal cause (terminalSubstituteEnd) or more than substituteGap
// before ref (leave_bounded_by_substitute), and kept open otherwise
// (leave_open_within_substitute_gap: a next substitute may take office a
// few days later); with no such exercise it is not written
// (leave_without_return). A definitive exit
// (REN, FAL, CAS, TER) is always the end of an exercise of the titular, so
// it can never leave a licenciado behind. A stretch running past the legislature is
// cut the day after its last day (the next legislature has its own row);
// one without end in a legislature already over by ref ends there too.
func periods(s senatorItem, n int, ref time.Time, file, base string, subs substitutes, stats map[string]int64) (out []period, ufs []string, reason string) {
	ms := s.mandates()
	for j, m := range ms.Items {
		uf := strings.TrimSpace(m.UfParlamentar)
		if !legis.ValidUF(uf) {
			return nil, nil, "bad_uf"
		}
		var leg *legislatureRef
		for _, l := range []*legislatureRef{m.Primeira, m.Segunda} {
			if l != nil && strings.TrimSpace(l.NumeroLegislatura) == fmt.Sprint(n) {
				leg = l
			}
		}
		if leg == nil {
			return nil, nil, "no_legislature"
		}
		legStart, ok1 := legis.ParseDate(leg.DataInicio)
		legEnd, ok2 := legis.ParseDate(leg.DataFim)
		if !ok1 || !ok2 || legEnd.Before(legStart) {
			return nil, nil, "bad_date"
		}
		legAfter := legEnd.AddDate(0, 0, 1)
		exs := m.exercises()
		var ds []dated
		for k, e := range exs.Items {
			from, ok := legis.ParseDate(e.DataInicio)
			if !ok {
				return nil, nil, "bad_date"
			}
			d := dated{k: k, from: from, cause: strings.TrimSpace(e.Causa)}
			if strings.TrimSpace(e.DataFim) != "" {
				t, ok := legis.ParseDate(e.DataFim)
				if !ok || t.Before(from) {
					return nil, nil, "bad_date"
				}
				d.last = &t
			}
			ds = append(ds, d)
		}
		slices.SortStableFunc(ds, func(a, b dated) int { return a.from.Compare(b.from) })
		titular := strings.EqualFold(strings.TrimSpace(m.Participacao), "Titular")
		status := legis.SubstituteInOffice
		if titular {
			status = legis.InOffice
		}
		var raw []period
		for i, d := range ds {
			rref := legis.Ref(file, fmt.Sprintf("%s%s/Mandatos/Mandato%s/Exercicios/Exercicio%s", listPrefix, base, ms.Pointer(j), exs.Pointer(d.k)))
			p := period{status: status, from: d.from, ref: rref}
			if d.last != nil {
				to := d.last.AddDate(0, 0, 1)
				p.to = &to
			}
			raw = append(raw, p)
			if d.last == nil {
				continue
			}
			known := definitiveCauses[d.cause] || leaveCauses[d.cause] || (!titular && d.cause == returnOfTitular)
			if !known && !d.last.Before(legStart) && d.last.Before(legAfter) {
				stats["unknown_leave_cause"]++
			}
			if !titular || !leaveCauses[d.cause] {
				continue
			}
			leave := period{status: legis.OnLeave, from: *p.to, ref: rref}
			if i+1 < len(ds) {
				// Bounded leave: the titular came back.
				next := ds[i+1].from
				if !next.After(leave.from) {
					continue
				}
				leave.to = &next
			} else {
				// Leave without return: only a substitute of the same
				// mandate in exercise since the titular's last exercise
				// corroborates it (docs/sources/senado.md).
				open, bound, cause := subs.corroborate(strings.TrimSpace(m.Codigo), d.from, leave.from)
				inLeg := !leave.from.Before(legStart) && leave.from.Before(legAfter)
				switch {
				case open:
				case bound != nil && !terminalSubstituteEnd(cause) && ref.Sub(*bound) <= substituteGap:
					// A recent end without a terminal cause: kept open.
					if inLeg {
						stats["leave_open_within_substitute_gap"]++
					}
				case bound != nil:
					leave.to = bound
					if inLeg {
						stats["leave_bounded_by_substitute"]++
					}
				default:
					if inLeg {
						stats["leave_without_return"]++
					}
					continue
				}
			}
			raw = append(raw, leave)
		}
		for _, p := range raw {
			if (p.to != nil && !p.to.After(legStart)) || !p.from.Before(legAfter) {
				continue
			}
			p.from = maxDate(p.from, legStart)
			switch {
			case p.to != nil && p.to.After(legAfter):
				p.to = &legAfter
			case p.to == nil && !ref.Before(legAfter):
				stats["closed_at_legislature_end"]++
				p.to = &legAfter
			}
			out = append(out, p)
			ufs = append(ufs, uf)
		}
	}
	return out, ufs, ""
}

// split cuts a stretch by the senator's affiliations (exclusive ends): one
// stretch per party, from the later of the two starts to the earlier of the
// two ends. Parts that no affiliation covers are not loaded (the party is
// unknown); uncovered reports them.
func split(p period, uf string, n int, affs []affiliation) (out []legis.Stretch, uncovered bool) {
	cursor := p.from // first day not covered yet
	for _, a := range affs {
		from := maxDate(p.from, a.from)
		to := minEnd(p.to, a.to)
		if to != nil && !to.After(from) {
			continue
		}
		if from.After(cursor) {
			uncovered = true
		}
		out = append(out, legis.Stretch{Legislature: n, Status: p.status, UF: uf, Party: a.party, PartyName: a.name,
			From: from, To: to, Ref: p.ref})
		if to == nil {
			return out, uncovered
		}
		cursor = maxDate(cursor, *to)
	}
	if p.to == nil || cursor.Before(*p.to) {
		uncovered = true
	}
	return out, uncovered
}

func maxDate(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// minEnd is the earlier of two ends, nil meaning "not ended".
func minEnd(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case a.Before(*b):
		return a
	default:
		return b
	}
}
