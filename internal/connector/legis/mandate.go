package legis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"geoint/internal/claims"
	"geoint/internal/connector"
)

// noParty are the APIs' markers for a parliamentarian without a party.
var noParty = map[string]bool{"S.PART.": true, "S/PARTIDO": true, "S/PART.": true, "SEM PARTIDO": true}

// PartyKey is a party's natural key, exactly the TSE connector's: the
// sigla trimmed, upper-cased and single-spaced ("PCdoB" -> "PCDOB", as the
// TSE's own recent files). "No party" markers give "".
func PartyKey(s string) string {
	k := strings.Join(strings.Fields(strings.ToUpper(s)), " ")
	if noParty[k] {
		return ""
	}
	return k
}

// ValidUF reports whether s is a two-letter state code.
func ValidUF(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// ParseDate reads YYYY-MM-DD, optionally followed by a time
// ("2023-02-01T12:05"), keeping the day. Anything else gives false.
func ParseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return time.Time{}, false
	}
	if len(s) > 10 && s[10] != 'T' && s[10] != ' ' {
		return time.Time{}, false
	}
	t, err := time.Parse(time.DateOnly, s[:10])
	if err != nil || t.Year() < 1800 || t.Year() > 2200 {
		return time.Time{}, false
	}
	return t, true
}

// Seat-holding statuses (attrs.status): holds_mandate means holding the
// seat, in exercise or on a temporary leave.
const (
	InOffice           = "exercicio"          // titular in exercise
	OnLeave            = "licenciado"         // titular on a temporary leave (the seat stays his)
	SubstituteInOffice = "suplente_exercicio" // substitute exercising the mandate
)

// Stretch is a period in which a parliamentarian held a seat with one
// status, party and state, within one legislature.
type Stretch struct {
	Legislature int
	Status      string
	UF, Party   string // Party is a PartyKey; "" = without party
	PartyName   string // the party's real name; "" when the source gives none
	From        time.Time
	// To is the EXCLUSIVE end the source declares (the first day the seat
	// is no longer held this way); nil while current.
	To  *time.Time
	Ref string // record_ref of the item that opened it
}

// Role is the role attribute: legislature, party, state and status, so that
// two stretches of one person in one legislature differ in the natural key.
func (s Stretch) Role() string {
	return fmt.Sprintf("%d:%s:%s:%s", s.Legislature, s.Party, s.UF, s.Status)
}

// Parliamentarian is one person of one house with the stretches to write.
type Parliamentarian struct {
	ContextKey  string // "dep:<id>" or "sen:<código>"
	Name        string
	CPFFragment string
	Ref         string // record_ref of the person record
	Stretches   []Stretch
}

// Writer writes parliamentarians with the row-level claims API, each call a
// short transaction.
type Writer struct {
	store    *claims.Store
	sourceID int16
	col      int64
	body     int64
	parties  map[string]int64
	nameless map[string]bool
	records  map[string]int64
	stats    connector.Stats
}

// NewWriter creates the house's public_body entity and returns a writer.
func NewWriter(ctx context.Context, store *claims.Store, sourceID int16, col int64, bodyKey, bodyName string, stats connector.Stats) (*Writer, error) {
	body, err := store.UpsertEntity(ctx, claims.KindPublicBody, bodyKey, bodyName)
	if err != nil {
		return nil, err
	}
	return &Writer{store: store, sourceID: sourceID, col: col, body: body,
		parties: map[string]int64{}, nameless: map[string]bool{}, records: map[string]int64{}, stats: stats}, nil
}

// Write records the person (reusing the source's record with the same
// context key, whatever its name or fragment: first seen wins) and one
// holds_mandate claim per stretch, open while current and closed at the
// declared end otherwise. A person without stretches is not written.
//
// Party entities are created only with a real name (never the sigla as
// name): they are not linked by these connectors (the party is an attribute
// of the mandate) and exist so that every source's parties share one
// entity. A sigla without a name gets none (party_without_name, counted
// once per sigla).
func (w *Writer) Write(ctx context.Context, p Parliamentarian) error {
	if len(p.Stretches) == 0 {
		return nil
	}
	rec, err := w.record(ctx, p)
	if err != nil {
		return err
	}
	for _, s := range p.Stretches {
		if err := w.party(ctx, s); err != nil {
			return err
		}
		from := s.From
		in := claims.RelationshipInput{
			Type: claims.HoldsMandate, SrcRecordID: &rec, DstEntityID: w.body,
			Attrs: map[string]any{"legislature": s.Legislature, "uf": s.UF, "party": s.Party, "status": s.Status, "role": s.Role()},
			Kind:  claims.Fact, ValidFrom: &from,
			Prov: claims.Provenance{CollectionID: w.col, RecordRef: s.Ref},
		}
		if s.To == nil {
			_, err = w.store.AssertRelationship(ctx, in)
			w.stats["mandates_current"]++
		} else {
			_, err = w.store.AssertEndedRelationship(ctx, in, *s.To)
			w.stats["mandates_ended"]++
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) party(ctx context.Context, s Stretch) error {
	if s.Party == "" {
		return nil
	}
	if _, ok := w.parties[s.Party]; ok {
		return nil
	}
	if s.PartyName == "" {
		if !w.nameless[s.Party] {
			w.nameless[s.Party] = true
			w.stats["party_without_name"]++
		}
		return nil
	}
	id, err := w.store.UpsertEntity(ctx, claims.KindParty, s.Party, s.PartyName)
	if err != nil {
		return err
	}
	w.parties[s.Party] = id
	return nil
}

func (w *Writer) record(ctx context.Context, p Parliamentarian) (int64, error) {
	if id, ok := w.records[p.ContextKey]; ok {
		return id, nil
	}
	id, found, err := w.store.FindPersonRecordByContext(ctx, w.sourceID, p.ContextKey)
	if err != nil {
		return 0, err
	}
	if found {
		w.stats["person_records_reused"]++
	} else {
		id, _, err = w.store.UpsertPersonRecord(ctx, claims.PersonRecordInput{
			SourceID: w.sourceID, ContextKey: p.ContextKey, Name: p.Name, CPFFragment: p.CPFFragment,
			Prov: claims.Provenance{CollectionID: w.col, RecordRef: p.Ref},
		})
		if err != nil {
			return 0, err
		}
		w.stats["person_records_new"]++
	}
	w.records[p.ContextKey] = id
	return id, nil
}

// Many is a JSON value that is a list, or a single object where the API
// collapses a list of one (XML-derived JSON); Single records which.
type Many[T any] struct {
	Items  []T
	Single bool
}

// UnmarshalJSON accepts an array, an object or null.
func (m *Many[T]) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || string(b) == "null":
		*m = Many[T]{}
		return nil
	case b[0] == '[':
		m.Single = false
		return json.Unmarshal(b, &m.Items)
	default:
		var one T
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		m.Items, m.Single = []T{one}, true
		return nil
	}
}

// Pointer is the JSON pointer segment of item i: "/i", or "" when the
// value was a single object.
func (m Many[T]) Pointer(i int) string {
	if m.Single {
		return ""
	}
	return "/" + strconv.Itoa(i)
}

// RelTypes are the relationship types this connector writes, each with
// whether it may join two entity-only ends (no person record at either
// end). No two connectors may write the same type between entity-only ends:
// such rows share one open natural key across sources, and CloseUnseen
// lets only the source that wrote a row first close it
// (internal/connector reltypes_test.go checks both rules).
var RelTypes = map[claims.RelType]bool{claims.HoldsMandate: false}
