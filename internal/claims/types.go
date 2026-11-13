// Package claims stores statements about entities. Every claim carries its
// provenance, nothing is deleted, and changes are kept as history.
//
// Validity dates: valid_from is the first day a relationship holds.
// valid_to_after and valid_to_before bound when it stopped holding, and
// valid_to_before is EXCLUSIVE: the first day on which the relationship no
// longer holds. A relationship is valid on day D when valid_from <= D and
// D < valid_to_before. A source-declared exact end is stored as
// valid_to_after = valid_to_before = E, E being that exclusive day: a source
// that publishes the last day of validity (inclusive) is stored as last day
// + 1; a change effective on day D (the old ends D, the new starts D) stores
// D, so consecutive periods have neither a gap nor an overlap day.
package claims

import (
	"errors"
	"time"
)

// EntityKind is the kind of an entity.
type EntityKind string

// Entity kinds.
const (
	KindPerson     EntityKind = "person"
	KindCompany    EntityKind = "company"
	KindParty      EntityKind = "party"
	KindPublicBody EntityKind = "public_body"
	KindForeign    EntityKind = "foreign"
)

// RelType is the type of a relationship.
type RelType string

// Relationship types.
const (
	PartnerOf       RelType = "partner_of"
	AdministratorOf RelType = "administrator_of"
	LegalRepOf      RelType = "legal_rep_of"
	DonatedTo       RelType = "donated_to"
	CandidateFor    RelType = "candidate_for"
	HoldsMandate    RelType = "holds_mandate"
	Controls        RelType = "controls"
	ShareholderOf   RelType = "shareholder_of"
)

// ClaimKind says whether a claim was read from a source or derived.
type ClaimKind string

// Claim kinds.
const (
	Fact      ClaimKind = "fact"
	Inference ClaimKind = "inference"
)

// Errors returned before anything reaches the database.
var (
	ErrNoProvenance             = errors.New("claim has no provenance: collection and record reference are required")
	ErrInferenceNeedsConfidence = errors.New("an inference needs a confidence")
	ErrConfidenceRange          = errors.New("confidence must be between 0 and 1")
	// ErrEmptyPeriod: an ended relationship whose (exclusive) end is not
	// after its valid_from would be valid on no day.
	ErrEmptyPeriod = errors.New("ended relationship must end after valid_from")
	// ErrUnknownRecord: a relationship cites a person record that does not
	// exist or is not linked to an entity yet.
	ErrUnknownRecord = errors.New("person record does not exist or has no entity")
)

// Provenance says where a claim came from.
type Provenance struct {
	CollectionID int64
	RecordRef    string
}

func (p Provenance) valid() bool { return p.CollectionID != 0 && p.RecordRef != "" }

// PersonRecordInput is a person as one source lists them in one context
// (for Receita, the context is the company's cnpj_basico).
type PersonRecordInput struct {
	SourceID    int16
	ContextKey  string
	Name        string
	CPFFragment string
	Prov        Provenance
}

// RelationshipInput is a relationship claim between two entities.
type RelationshipInput struct {
	Type                     RelType
	SrcEntityID, DstEntityID int64
	SrcRecordID, DstRecordID *int64
	Attrs                    map[string]any
	Kind                     ClaimKind
	Confidence               *float64
	ValidFrom                *time.Time
	Prov                     Provenance
}

// CompanyVersionInput is the registry data of a company at one collection.
type CompanyVersionInput struct {
	EntityID                                        int64
	Name, LegalNature, Size, UF, City, CNAE, Status string
	Capital                                         *float64
	Prov                                            Provenance
}
