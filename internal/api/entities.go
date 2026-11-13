package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"geoint/internal/claims"
	"geoint/internal/graph"
	"geoint/internal/identity"
	"geoint/internal/search"
)

// Limits of the entity page.
const (
	maxVersions     = 100
	maxRecords      = 500
	maxPossibleSame = 20
	// maxRelLimit is the default and largest page of relationships.
	maxRelLimit = 200
)

var knownRelTypes = map[claims.RelType]bool{
	claims.PartnerOf: true, claims.AdministratorOf: true, claims.LegalRepOf: true, claims.DonatedTo: true,
	claims.CandidateFor: true, claims.HoldsMandate: true, claims.Controls: true, claims.ShareholderOf: true,
}

// pathID parses a positive int64 path value.
func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, errBadRequest
	}
	return id, nil
}

// cpfMasked shows a stored CPF fragment the way the Receita publishes it
// (***NNNNNN**), or "" when there is no usable fragment; the bare stored
// form never leaves the API.
func cpfMasked(fragment string) string {
	f, ok := identity.CPFFragment(fragment)
	if !ok {
		return ""
	}
	return "***" + f + "**"
}

type versionJSON struct {
	ID                   int64   `json:"id"`
	Name                 string  `json:"name"`
	LegalNature          string  `json:"legal_nature"`
	Size                 string  `json:"size"`
	UF                   string  `json:"uf"`
	City                 string  `json:"city"`
	CNAE                 string  `json:"cnae"`
	Status               string  `json:"status"`
	Capital              *string `json:"capital"`
	Open                 bool    `json:"open"`
	FirstCollectionID    int64   `json:"first_collection_id"`
	LastSeenCollectionID *int64  `json:"last_seen_collection_id"`
	ClosedCollectionID   *int64  `json:"closed_collection_id"`
	// When the version was valid, as the sources saw it: the reference dates
	// of the collections that first saw it, last saw it and closed it (null
	// when there is no such collection). Never an invented date.
	FirstSeenReferenceDate *date     `json:"first_seen_reference_date"`
	LastSeenReferenceDate  *date     `json:"last_seen_reference_date"`
	ClosedReferenceDate    *date     `json:"closed_reference_date"`
	RecordRef              string    `json:"record_ref"`
	RecordedAt             time.Time `json:"recorded_at"`
}

type recordJSON struct {
	ID                 int64   `json:"id"`
	EntityID           int64   `json:"entity_id"`
	SourceKey          string  `json:"source_key"`
	ContextKey         string  `json:"context_key"`
	Name               string  `json:"name"`
	CPFMasked          string  `json:"cpf_masked,omitempty"`
	IdentityConfidence float64 `json:"identity_confidence"`
	// Locked: an analyst decision fixed this record's entity.
	Locked bool `json:"locked"`
	// Where the record was first read: the line or element of the source
	// file (as on relationships), the collection and its reference date.
	RecordRef              string `json:"record_ref"`
	FirstCollectionID      int64  `json:"first_collection_id"`
	FirstSeenReferenceDate *date  `json:"first_seen_reference_date"`
}

// candidateJSON is a possible same person, with the whole basis
// identity.PossibleSame has (there is no score: none is computed).
type candidateJSON struct {
	EntityID int64  `json:"entity_id"`
	Name     string `json:"name"`
	// SameName: a record of the candidate has the same normalized name as a
	// record of the entity. Always true: it is why a candidate is listed.
	SameName bool `json:"same_name"`
	// Records: the candidate's number of person records.
	Records int `json:"records"`
	// SharedFragment: also the same CPF fragment (never shown).
	SharedFragment bool `json:"shared_fragment"`
}

// recordSelect reads person records with their source key and first
// collection; callers add the WHERE clause. Confidences go through numeric
// so 0.99 reads as 0.99. The reference date is one collection_pkey probe.
const recordSelect = `SELECT pr.id, coalesce(pr.entity_id, 0), s.key, pr.context_key, pr.name, pr.cpf_fragment,
		pr.identity_confidence::numeric::float8, pr.locked_by_decision_id IS NOT NULL,
		pr.record_ref, pr.first_collection_id,
		(SELECT c.reference_date FROM collection c WHERE c.id = pr.first_collection_id)
	FROM person_record pr JOIN source s ON s.id = pr.source_id `

func scanRecords(rows pgx.Rows) ([]recordJSON, error) {
	out := []recordJSON{}
	var (
		rec   recordJSON
		frag  string
		first *time.Time
	)
	_, err := pgx.ForEachRow(rows, []any{&rec.ID, &rec.EntityID, &rec.SourceKey, &rec.ContextKey, &rec.Name, &frag,
		&rec.IdentityConfidence, &rec.Locked, &rec.RecordRef, &rec.FirstCollectionID, &first}, func() error {
		r := rec
		r.Name, r.CPFMasked = search.MaskCPF(r.Name), cpfMasked(frag)
		r.FirstSeenReferenceDate = toDate(first)
		// A record ref is a file and line (or element), but it is about a
		// person: an 11-digit run in it is masked like a name's.
		r.RecordRef = search.MaskCPF(r.RecordRef)
		out = append(out, r)
		return nil
	})
	return out, err
}

// Queries of the entity page and provenance (fixed text so a test can check
// their plans use the indexes).
const (
	// versionsSQL: company_version_entity_id (entity_id, id DESC), migration
	// 0009, in index order (no sort); the reference dates are one
	// collection_pkey probe each.
	versionsSQL = `SELECT v.id, v.name, v.legal_nature, v.size, v.uf, v.city, v.cnae, v.status, v.capital::text,
			v.closed_collection_id IS NULL, v.first_collection_id, v.last_seen_collection_id, v.closed_collection_id,
			v.record_ref, v.recorded_at,
			(SELECT c.reference_date FROM collection c WHERE c.id = v.first_collection_id),
			(SELECT c.reference_date FROM collection c WHERE c.id = v.last_seen_collection_id),
			(SELECT c.reference_date FROM collection c WHERE c.id = v.closed_collection_id)
		FROM company_version v WHERE v.entity_id = $1 ORDER BY v.id DESC LIMIT $2`
	// entityRecordsSQL: person_record_entity_id (0002).
	entityRecordsSQL = recordSelect + `WHERE pr.entity_id = $1 ORDER BY pr.id LIMIT $2`
)

// provenanceSQL: primary keys of relationship, collection (first and last
// seen), source and collection_file. The file name is record_ref up to its
// last ':' (the line number follows it). stale is computed as graph does
// (graph.SourceStaleSQL: the source's newest done full-scope collection, aged
// from its observed date for an edition source, else its reference date).
var provenanceSQL = `
	SELECT r.id, r.kind, r.confidence::numeric::float8, r.record_ref, r.recorded_at,
		r.last_seen_collection_id, r.closed_collection_id,
		s.key, s.name, c.id, c.reference_date, c.observed_date, c.finished_at, c.status,
		(c.scope ->> 'full')::boolean IS TRUE,
		` + graph.SourceStaleSQL("$2::timestamptz") + `,
		f.raw_key, f.sha256, f.size_bytes, f.source_url,
		l.id, l.reference_date, l.observed_date, l.finished_at
	FROM relationship r
	JOIN collection c ON c.id = r.first_collection_id
	JOIN source s ON s.id = c.source_id
	LEFT JOIN collection l ON l.id = r.last_seen_collection_id
	LEFT JOIN collection_file f ON f.collection_id = r.first_collection_id
		AND f.name = left(r.record_ref, length(r.record_ref) - position(':' in reverse(r.record_ref)))
	WHERE r.id = $1`

// sourceNotes are fixed, public hints shown next to a source_url that cannot
// be opened as it is. The Receita file URL is the WebDAV address of its
// public share; the share code travels as the user name of HTTP Basic
// authentication and is never stored (collection.SafeURL).
var sourceNotes = map[string]string{
	"receita": "Arquivo do compartilhamento público da Receita Federal (Nextcloud): o endereço WebDAV só abre com o " +
		"código público do compartilhamento como usuário; para baixar pelo navegador, abra " +
		"https://arquivos.receitafederal.gov.br/ e siga Dados > Cadastros > CNPJ > a pasta do mês.",
}

// entity: GET /api/v1/entities/{id}.
func (s *server) entity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	var (
		ref graph.EntityRef
		key *string
	)
	err = s.Pool.QueryRow(ctx, `SELECT id, kind, name, natural_key FROM entity WHERE id = $1`, id).
		Scan(&ref.ID, &ref.Kind, &ref.Name, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, errNotFound)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	if key != nil {
		ref.NaturalKey = *key
	}

	versions := []versionJSON{}
	rows, err := s.Pool.Query(ctx, versionsSQL, id, maxVersions)
	if err != nil {
		fail(w, r, err)
		return
	}
	var (
		v                   versionJSON
		first, last, closed *time.Time
	)
	_, err = pgx.ForEachRow(rows, []any{&v.ID, &v.Name, &v.LegalNature, &v.Size, &v.UF, &v.City, &v.CNAE, &v.Status,
		&v.Capital, &v.Open, &v.FirstCollectionID, &v.LastSeenCollectionID, &v.ClosedCollectionID, &v.RecordRef, &v.RecordedAt,
		&first, &last, &closed}, func() error {
		c := v
		c.Name, c.RecordedAt = search.MaskCPF(c.Name), c.RecordedAt.UTC()
		c.FirstSeenReferenceDate, c.LastSeenReferenceDate, c.ClosedReferenceDate = toDate(first), toDate(last), toDate(closed)
		versions = append(versions, c)
		return nil
	})
	if err != nil {
		fail(w, r, err)
		return
	}

	rows, err = s.Pool.Query(ctx, entityRecordsSQL, id, maxRecords+1)
	if err != nil {
		fail(w, r, err)
		return
	}
	records, err := scanRecords(rows)
	if err != nil {
		fail(w, r, err)
		return
	}
	truncated := len(records) > maxRecords
	if truncated {
		records = records[:maxRecords]
	}

	possible := []candidateJSON{}
	if ref.Kind == claims.KindPerson {
		cands, err := s.Identity.PossibleSame(ctx, id, maxPossibleSame)
		if err != nil {
			fail(w, r, err)
			return
		}
		for _, c := range cands {
			possible = append(possible, candidateJSON{EntityID: c.EntityID, Name: search.MaskCPF(c.Name), SameName: true,
				Records: c.Records, SharedFragment: c.SharedFragment})
		}
	}

	if !s.audited(w, r, "entity.read", &id, nil) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entity": toEntity(ref), "versions": versions, "records": records, "records_truncated": truncated,
		"possible_same": possible,
	})
}

// relationships: GET /api/v1/entities/{id}/relationships.
func (s *server) relationships(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	q := r.URL.Query()
	var o graph.Options
	state := q.Get("state")
	switch state {
	case "", "open":
		state = "open"
	case "all":
		o.IncludeClosed = true
	case "closed":
		o.ClosedOnly = true
	default:
		fail(w, r, errBadRequest)
		return
	}
	types := []string{}
	for _, v := range q["type"] {
		for _, t := range strings.Split(v, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if !knownRelTypes[claims.RelType(t)] {
				fail(w, r, errBadRequest)
				return
			}
			o.RelTypes = append(o.RelTypes, claims.RelType(t))
			types = append(types, t)
		}
	}
	detail := map[string]any{"state": state, "types": types}
	if v := q.Get("at"); v != "" {
		at, err := time.Parse(time.DateOnly, v)
		if err != nil {
			fail(w, r, errBadRequest)
			return
		}
		o.At = &at
		detail["at"] = at.Format(time.DateOnly)
	}
	limit := maxRelLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(w, r, errBadRequest)
			return
		}
		limit = min(n, maxRelLimit)
	}
	edges, next, err := s.Graph.Neighbors(r.Context(), id, o, q.Get("cursor"), limit)
	if err != nil {
		fail(w, r, err)
		return
	}
	detail["count"] = len(edges)
	if !s.audited(w, r, "entity.relationships.read", &id, detail) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": toEdges(edges), "next_cursor": next})
}

// provenance: GET /api/v1/relationships/{id}/provenance, with the raw file
// (collection_file) the claim's record_ref points at, when it was recorded.
func (s *server) provenance(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	type collectionJSON struct {
		ID            int64      `json:"id"`
		ReferenceDate date       `json:"reference_date"`
		ObservedDate  date       `json:"observed_date"`
		CollectedAt   *time.Time `json:"collected_at"`
		Status        string     `json:"status"`
		FullScope     bool       `json:"full_scope"`
		Stale         bool       `json:"stale"`
	}
	// lastSeenJSON is the collection that last saw the claim.
	type lastSeenJSON struct {
		ID            int64      `json:"id"`
		ReferenceDate date       `json:"reference_date"`
		ObservedDate  date       `json:"observed_date"`
		CollectedAt   *time.Time `json:"collected_at"`
	}
	type sourceJSON struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	var (
		out struct {
			RelationshipID       int64          `json:"relationship_id"`
			Kind                 string         `json:"kind"`
			Confidence           *float64       `json:"confidence"`
			RecordRef            string         `json:"record_ref"`
			RecordedAt           time.Time      `json:"recorded_at"`
			LastSeenCollectionID *int64         `json:"last_seen_collection_id"`
			ClosedCollectionID   *int64         `json:"closed_collection_id"`
			Source               sourceJSON     `json:"source"`
			Collection           collectionJSON `json:"collection"`
			// The preserved raw file the claim was read from; absent for
			// collections loaded before files were recorded.
			RawKey    *string `json:"raw_key,omitempty"`
			RawSHA256 *string `json:"raw_sha256,omitempty"`
			RawSize   *int64  `json:"raw_size,omitempty"`
			// SourceURL is where that file was downloaded from (null for files
			// recorded before URLs were, or when no file is recorded);
			// SourceNote a fixed hint when the URL cannot be opened as it is.
			SourceURL  *string `json:"source_url"`
			SourceNote *string `json:"source_note,omitempty"`
			// LastSeenCollection: the collection that last saw the claim (null
			// when none is recorded).
			LastSeenCollection *lastSeenJSON `json:"last_seen_collection"`
		}
		ref, observed                   time.Time
		lastID                          *int64
		lastRef, lastObserved, lastDone *time.Time
	)
	err = s.Pool.QueryRow(r.Context(), provenanceSQL, id, s.Now()).
		Scan(&out.RelationshipID, &out.Kind, &out.Confidence, &out.RecordRef, &out.RecordedAt,
			&out.LastSeenCollectionID, &out.ClosedCollectionID,
			&out.Source.Key, &out.Source.Name, &out.Collection.ID, &ref, &observed, &out.Collection.CollectedAt,
			&out.Collection.Status, &out.Collection.FullScope, &out.Collection.Stale,
			&out.RawKey, &out.RawSHA256, &out.RawSize, &out.SourceURL,
			&lastID, &lastRef, &lastObserved, &lastDone)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, errNotFound)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	out.Collection.ReferenceDate, out.Collection.ObservedDate = date(ref), date(observed)
	out.RecordedAt = out.RecordedAt.UTC()
	out.Collection.CollectedAt = utc(out.Collection.CollectedAt)
	if lastID != nil && lastRef != nil && lastObserved != nil {
		out.LastSeenCollection = &lastSeenJSON{ID: *lastID, ReferenceDate: date(*lastRef), ObservedDate: date(*lastObserved),
			CollectedAt: utc(lastDone)}
	}
	if note, ok := sourceNotes[out.Source.Key]; ok && out.SourceURL != nil {
		out.SourceNote = &note
	}
	if !s.audited(w, r, "relationship.provenance.read", nil, map[string]any{"relationship_id": id}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
