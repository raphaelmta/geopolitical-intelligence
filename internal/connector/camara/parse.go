package camara

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"geoint/internal/connector/legis"
	"geoint/internal/identity"
)

// envelope is every response of the API: {"dados": ..., "links": [...]}.
type envelope struct {
	Dados json.RawMessage `json:"dados"`
	Links []struct {
		Rel  string `json:"rel"`
		Href string `json:"href"`
	} `json:"links"`
}

func (e envelope) hasNext() bool {
	for _, l := range e.Links {
		if l.Rel == "next" {
			return true
		}
	}
	return false
}

// decode reads the envelope of file; "dados" must be an array (list) or an
// object (!list).
func decode(file string, b []byte, list bool) (envelope, error) {
	var e envelope
	if err := legis.Decode(file, b, &e); err != nil {
		return envelope{}, err
	}
	d := bytes.TrimSpace(e.Dados)
	want := byte('{')
	if list {
		want = '['
	}
	if len(d) == 0 || d[0] != want {
		return envelope{}, &legis.ShapeError{File: file, What: `"dados" missing or of the wrong type`}
	}
	return e, nil
}

// items returns the elements of a list response, undecoded, and whether
// the response links to a next page.
func items(file string, b []byte) ([]json.RawMessage, bool, error) {
	e, err := decode(file, b, true)
	if err != nil {
		return nil, false, err
	}
	var out []json.RawMessage
	if err := legis.Decode(file, e.Dados, &out); err != nil {
		return nil, false, err
	}
	return out, e.hasNext(), nil
}

// deputyID reads the id of a deputies-list item; false when it has none.
func deputyID(item json.RawMessage) (int64, bool) {
	var v struct {
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(item, &v); err != nil || v.ID == nil || *v.ID <= 0 {
		return 0, false
	}
	return *v.ID, true
}

// legislature reads legislaturas/{n}: its dates.
func legislature(file string, b []byte, n int) (start, end time.Time, err error) {
	e, err := decode(file, b, false)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	var v struct {
		ID         int    `json:"id"`
		DataInicio string `json:"dataInicio"`
		DataFim    string `json:"dataFim"`
	}
	if err := legis.Decode(file, e.Dados, &v); err != nil {
		return time.Time{}, time.Time{}, err
	}
	start, ok1 := legis.ParseDate(v.DataInicio)
	end, ok2 := legis.ParseDate(v.DataFim)
	if v.ID != n || !ok1 || !ok2 || end.Before(start) {
		return time.Time{}, time.Time{}, &legis.ShapeError{File: file, What: "legislature without its id or dates"}
	}
	return start, end, nil
}

// person is what is loaded from deputados/{id}: the civil name and the CPF
// fragment. The full CPF never leaves deputy().
type person struct {
	Name, Fragment string
}

func deputy(file string, b []byte, id int64) (person, error) {
	e, err := decode(file, b, false)
	if err != nil {
		return person{}, err
	}
	var v struct {
		ID        int64   `json:"id"`
		NomeCivil *string `json:"nomeCivil"`
		CPF       *string `json:"cpf"`
	}
	if err := legis.Decode(file, e.Dados, &v); err != nil {
		return person{}, err
	}
	if v.ID != id {
		return person{}, &legis.ShapeError{File: file, What: "deputy id differs from the requested one"}
	}
	var p person
	if v.NomeCivil != nil {
		p.Name = strings.Join(strings.Fields(*v.NomeCivil), " ")
	}
	if v.CPF != nil {
		p.Fragment, _ = identity.CPFFragment(*v.CPF)
		*v.CPF = ""
	}
	return p, nil
}

// event is one status change of a deputy (deputados/{id}/historico).
type event struct {
	idx      int // position in "dados": the record_ref pointer
	leg      int
	at       time.Time // dataHora, for ordering
	day      time.Time // the date of dataHora
	party    string    // legis.PartyKey
	uf       string
	situacao string
	cond     string // condicaoEleitoral
	desc     string
	reason   string // why the event is malformed, "" when it is not
}

type rawEvent struct {
	IDLegislatura   *int    `json:"idLegislatura"`
	SiglaPartido    *string `json:"siglaPartido"`
	SiglaUf         *string `json:"siglaUf"`
	DataHora        *string `json:"dataHora"`
	Situacao        *string `json:"situacao"`
	Condicao        *string `json:"condicaoEleitoral"`
	DescricaoStatus *string `json:"descricaoStatus"`
}

// history reads deputados/{id}/historico. It returns the events that carry
// a situation (the "Nome/Partido no início da legislatura" notes have none
// and are not status changes), each with the reason it is malformed, if
// any. reason is set when an event cannot be read at all: its legislature
// is unknown, so the whole deputy is skipped.
func history(file string, b []byte) (evs []event, read int, reason string, err error) {
	raw, next, err := items(file, b)
	if err != nil {
		return nil, 0, "", err
	}
	if next {
		return nil, 0, "", &legis.ShapeError{File: file, What: "the history is paginated"}
	}
	for i, item := range raw {
		var r rawEvent
		if err := json.Unmarshal(item, &r); err != nil || r.IDLegislatura == nil {
			return nil, len(raw), "bad_event", nil
		}
		if r.Situacao == nil {
			continue
		}
		e := event{idx: i, leg: *r.IDLegislatura, situacao: strings.TrimSpace(*r.Situacao)}
		if r.DescricaoStatus != nil {
			e.desc = strings.TrimSpace(*r.DescricaoStatus)
		}
		if r.Condicao != nil {
			e.cond = strings.TrimSpace(*r.Condicao)
		}
		var ok bool
		switch {
		case r.DataHora == nil:
			e.reason = "bad_date"
		default:
			if e.at, ok = timestamp(*r.DataHora); !ok {
				e.reason = "bad_date"
			}
			e.day = effectiveDay(e.at)
		}
		if r.SiglaUf != nil {
			e.uf = strings.TrimSpace(*r.SiglaUf)
		}
		if e.reason == "" && !legis.ValidUF(e.uf) {
			e.reason = "bad_uf"
		}
		if r.SiglaPartido == nil || strings.TrimSpace(*r.SiglaPartido) == "" {
			if e.reason == "" {
				e.reason = "no_party"
			}
		} else {
			e.party = legis.PartyKey(*r.SiglaPartido)
		}
		evs = append(evs, e)
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].at.Before(evs[j].at) })
	return evs, len(raw), "", nil
}

// timestamp reads dataHora: "2006-01-02T15:04", with optional seconds, or
// a plain date.
func timestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil && t.Year() >= 1800 && t.Year() <= 2200 {
			return t, true
		}
	}
	return time.Time{}, false
}

// effectiveDay is the first day an event's new situation holds: its date,
// or the next day for an event at 23:59 (still the old situation that day).
// The end of the legislature does not rely on it (see termEnd).
func effectiveDay(at time.Time) time.Time {
	d := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	if at.Hour() == 23 && at.Minute() >= 59 {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// Situations and descriptions of the history (docs/sources/camara.md).
const (
	inOffice       = "Exercício"
	entryPrefix    = "Entrada"
	definitiveExit = "Saída - Afastamento definitivo"
	substitute     = "Suplente"
	// possessAsTitular is in "Entrada - Posse de Suplente - Posse como
	// Titular": a substitute taking the seat as titular (observed with
	// condição EFETIVADO).
	possessAsTitular = "Posse como Titular"
	termEndDesc      = "Término da Legislatura"
)

// termEnd reports whether e is the end of the legislature: FIM_MANDATO or
// "… Término da Legislatura", dated on the legislature's last day or the
// next (recorded at 23:59 in recent histories, at 00:00 in older ones).
func termEnd(e event, legEnd time.Time) bool {
	if e.situacao != "FIM_MANDATO" && !strings.Contains(e.desc, termEndDesc) {
		return false
	}
	d := time.Date(e.at.Year(), e.at.Month(), e.at.Day(), 0, 0, 0, 0, time.UTC)
	return !d.Before(legEnd) && !d.After(legEnd.AddDate(0, 0, 1))
}

// temporaryExits prefix the description of a temporary leave; the cause
// follows (up to " / " when the API joins two descriptions).
var temporaryExits = []string{"Saída - Afastamento com prazo determinado - ", "Saída - Afastamento sem prazo determinado - "}

// definitiveSituations end the seat-holding whatever the description.
var definitiveSituations = map[string]bool{"FIM_MANDATO": true, "VACANCIA": true}

// leaveCauses are the temporary-leave causes of a titular: the seat stays
// his (licenciado). Exactly the strings the API publishes; any other cause
// is never guessed (unknown_leave_cause: the stretch ends, nothing opens).
var leaveCauses = map[string]bool{
	"Ministro de Estado":                          true,
	"Secretário de Estado":                        true,
	"Secretário de Prefeitura de Capital":         true,
	"Afastamento Conjunto Consecutivo":            true, // licenças somadas (saúde, interesse particular...)
	"Licença para Tratamento de Saúde":            true,
	"Licença para Tratar de Interesse Particular": true,
	"Suspensão":                        true, // mandato suspenso: o assento continua dele
	"Afastamento por Decisão Judicial": true,
	"Afastamento por Decisão da Presidência sem Convocação de Suplente": true,
}

// leaveCause returns the cause of a temporary leave, or false when the
// description is not one.
func leaveCause(desc string) (string, bool) {
	for _, p := range temporaryExits {
		if c, ok := strings.CutPrefix(desc, p); ok {
			c, _, _ = strings.Cut(c, " / ")
			return strings.TrimSpace(c), true
		}
	}
	return "", false
}

// stretches turns the events of one legislature into the stretches in which
// the deputy held the seat (docs/sources/camara.md), one per status, party
// and state. Ends are EXCLUSIVE (the first day the stretch no longer holds):
//
//   - an "Entrada…" event in exercise opens exercicio (titular or
//     EFETIVADO) or suplente_exercicio (condição Suplente);
//   - a definitive exit (FIM_MANDATO, VACANCIA, "Saída - Afastamento
//     definitivo…") ends the seat-holding;
//   - a titular's temporary leave with a cause in leaveCauses ends exercicio
//     and opens licenciado on the same day, until an event in exercise; a
//     leave with another cause, or any other event out of exercise, ends
//     the stretch (unknown_leave_cause);
//   - a substitute's exit ends suplente_exercicio;
//   - an event in exercise with another status, party or state ends the
//     stretch and opens the next one on the same day (party change; a
//     substitute made titular becomes exercicio);
//   - an event out of exercise while licenciado (another leave, a party
//     change during the leave) keeps licenciado, split on party or state.
//
// A stretch still open when the legislature had already ended by ref ends
// the day after the legislature's last day. A malformed event of the
// legislature makes the whole legislature skipped (reason), never guessed.
func stretches(evs []event, leg int, legEnd, ref time.Time, file string, stats map[string]int64) ([]legis.Stretch, string) {
	for _, e := range evs {
		if e.leg == leg && e.reason != "" {
			return nil, e.reason
		}
	}
	var out []legis.Stretch
	var cur *legis.Stretch
	end := func(d time.Time) {
		if d.After(cur.From) {
			cur.To = &d
			out = append(out, *cur)
		} else {
			stats["empty_stretches"]++
		}
		cur = nil
	}
	open := func(e event, status string) {
		cur = &legis.Stretch{Legislature: leg, Status: status, UF: e.uf, Party: e.party, From: e.day,
			Ref: legis.Ref(file, fmt.Sprintf("/dados/%d", e.idx))}
	}
	for _, e := range evs {
		if e.leg != leg {
			continue
		}
		in := e.situacao == inOffice
		status := legis.InOffice
		if e.cond == substitute && !strings.Contains(e.desc, possessAsTitular) {
			status = legis.SubstituteInOffice
		}
		switch {
		case cur == nil && in && strings.HasPrefix(e.desc, entryPrefix):
			open(e, status)
		case cur == nil && in:
			stats["exercise_without_entry"]++
		case cur == nil:
		case termEnd(e, legEnd):
			// End of the legislature: the seat is held through its last
			// day, whatever time of day the event was recorded at.
			end(legEnd.AddDate(0, 0, 1))
		case definitiveSituations[e.situacao] || strings.HasPrefix(e.desc, definitiveExit):
			end(e.day)
		case in:
			if status != cur.Status || e.party != cur.Party || e.uf != cur.UF {
				end(e.day)
				open(e, status)
			}
		case cur.Status == legis.OnLeave:
			if e.party != cur.Party || e.uf != cur.UF {
				end(e.day)
				open(e, legis.OnLeave)
			}
		case cur.Status == legis.SubstituteInOffice:
			// Any exit ends a substitute's exercise; not a titular's
			// leave, so not unknown_leave_cause.
			if _, ok := leaveCause(e.desc); !ok {
				stats["substitute_exit_other"]++
			}
			end(e.day)
		default: // titular in exercise, event out of exercise
			cause, ok := leaveCause(e.desc)
			if ok && leaveCauses[cause] {
				end(e.day)
				open(e, legis.OnLeave)
				continue
			}
			stats["unknown_leave_cause"]++
			end(e.day)
		}
	}
	if cur != nil {
		if after := legEnd.AddDate(0, 0, 1); !ref.Before(after) {
			stats["closed_at_legislature_end"]++
			end(after)
		} else {
			out = append(out, *cur)
		}
	}
	return out, ""
}
