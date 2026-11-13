package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"geoint/internal/graph"
	"geoint/internal/identity"
)

// Limits of the identity routes.
const (
	proposalPage    = 50
	maxOtherRecords = 50
	maxRecordIDs    = 100
	maxReasonRunes  = 500
	// maxExpectedRecords bounds expected_record_ids: a proposal lists every
	// record of one key, which may be more than an analyst operation's 100.
	maxExpectedRecords = 10000
	// maxEntityIDs bounds a merge's entity_ids.
	maxEntityIDs = 10
)

var proposalStatuses = map[string]bool{"pending": true, "confirmed": true, "rejected": true, "obsolete": true}

type proposalCursor struct {
	connections int
	id          int64
}

// The cursor is base64 of "connections:id" of the last proposal of a page.
func (c proposalCursor) encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(c.connections) + ":" + strconv.FormatInt(c.id, 10)))
}

func decodeProposalCursor(s string) (*proposalCursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errBadRequest
	}
	a, b, ok := strings.Cut(string(raw), ":")
	if !ok {
		return nil, errBadRequest
	}
	conn, err1 := strconv.Atoi(a)
	id, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || conn < 0 || id <= 0 {
		return nil, errBadRequest
	}
	return &proposalCursor{connections: conn, id: id}, nil
}

type otherRecordJSON struct {
	ID         int64  `json:"id"`
	SourceKey  string `json:"source_key"`
	ContextKey string `json:"context_key"`
}

type targetJSON struct {
	Entity       *entityJSON       `json:"entity"`
	OtherRecords []otherRecordJSON `json:"other_records"`
	// OtherRecordsTruncated: the entity has more records than listed.
	OtherRecordsTruncated bool `json:"other_records_truncated"`
}

// proposalRecordJSON is a record of a proposal. Free: linked to an entity
// and not locked by an analyst decision, so a decision on the proposal acts
// on it (a confirmation moves it into the target); a record that is not free
// is left where it is. The free records are what expected_record_ids lists.
type proposalRecordJSON struct {
	recordJSON
	Free bool `json:"free"`
}

type proposalJSON struct {
	ID          int64                `json:"id"`
	Status      string               `json:"status"`
	Name        string               `json:"name"`
	Records     []proposalRecordJSON `json:"records"`
	Connections int                  `json:"connections"`
	Reason      string               `json:"reason"`
	// ReasonCode is the stable form of Reason (reasonCode): common_name,
	// impure_target, ambiguous_key or other.
	ReasonCode string `json:"reason_code"`
	// TargetEntity is the entity a confirmation would merge the records
	// into (that of the lowest-id record still free), with its other records.
	TargetEntity targetJSON `json:"target_entity"`
}

// Reason codes of a proposal (proposalJSON.ReasonCode).
const (
	// reasonCommonName: more distinct CPF fragments share the name than the
	// automatic-merge threshold allows.
	reasonCommonName = "common_name"
	// reasonImpureTarget: the records would join an entity an analyst built.
	reasonImpureTarget = "impure_target"
	// reasonAmbiguousKey: an analyst split records with this name and CPF
	// fragment apart before.
	reasonAmbiguousKey = "ambiguous_key"
	// reasonOther: any other text (none today).
	reasonOther = "other"
)

// commonNameReason is the resolver's common-name reason, with its numbers.
var commonNameReason = regexp.MustCompile(`^[0-9]+ distinct CPF fragments share this name \(automatic merge up to [0-9]+\)$`)

// reasonCode maps the resolver's reason text (internal/identity/resolver.go,
// the CASE of its identity_proposal INSERT) to a stable code: the one place
// clients' logic should depend on. TestReasonCodesCoverTheResolver fails when
// the resolver writes a reason this mapping does not know.
func reasonCode(reason string) string {
	switch {
	case commonNameReason.MatchString(reason):
		return reasonCommonName
	case reason == "the records would join an entity an analyst built":
		return reasonImpureTarget
	case reason == "an analyst split records with this name and CPF fragment apart before":
		return reasonAmbiguousKey
	}
	return reasonOther
}

// proposalsSQL selects a page of proposals in queue order (connections
// DESC, id). Pending uses the literal status so the partial index
// identity_proposal_pending_queue (0002) applies; the other statuses use
// identity_proposal_status_queue (0009). After the first page the keyset
// starts with "connections <= $c", an index condition on both indexes; the
// rest of the keyset is a filter on the few rows with that connection count.
func proposalsSQL(status string, cur *proposalCursor, limit int) (string, []any) {
	var a []any
	arg := func(v any) string {
		a = append(a, v)
		return "$" + strconv.Itoa(len(a))
	}
	where := "status = 'pending'"
	if status != "pending" {
		where = "status = " + arg(status)
	}
	if cur != nil {
		c := arg(cur.connections)
		where += " AND connections <= " + c + " AND (connections < " + c + " OR id > " + arg(cur.id) + ")"
	}
	return `SELECT id, status, record_ids, reason, connections FROM identity_proposal
		WHERE ` + where + ` ORDER BY connections DESC, id LIMIT ` + arg(limit), a
}

// otherRecordsSQL lists, for each (proposal, target entity) pair, the
// target's records outside that proposal, up to $3 per pair, in one statement.
const otherRecordsSQL = `
	SELECT t.pid, x.id, x.key, x.context_key
	FROM unnest($1::bigint[], $2::bigint[]) AS t(pid, eid)
	JOIN identity_proposal p ON p.id = t.pid
	CROSS JOIN LATERAL (
		SELECT pr.id, s.key, pr.context_key FROM person_record pr JOIN source s ON s.id = pr.source_id
		WHERE pr.entity_id = t.eid AND pr.id <> ALL (p.record_ids)
		ORDER BY pr.id LIMIT $3) x
	ORDER BY t.pid, x.id`

// proposals: GET /api/v1/identity/proposals?status=&cursor=, most connected
// first, 50 per page. The proposal's group key (name and CPF fragment) is
// never returned.
func (s *server) proposals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = "pending"
	}
	if !proposalStatuses[status] {
		fail(w, r, errBadRequest)
		return
	}
	cur, err := decodeProposalCursor(q.Get("cursor"))
	if err != nil {
		fail(w, r, err)
		return
	}
	items := []proposalJSON{}
	next := ""
	err = s.readTx(ctx, func(tx pgx.Tx) error {
		sql, args := proposalsSQL(status, cur, proposalPage+1)
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		type prop struct {
			proposalJSON
			ids []int64
		}
		var props []prop
		var p prop
		if _, err := pgx.ForEachRow(rows, []any{&p.ID, &p.Status, &p.ids, &p.Reason, &p.Connections}, func() error {
			c := p
			c.ids = slices.Clone(p.ids)
			props = append(props, c)
			return nil
		}); err != nil {
			return err
		}
		if len(props) > proposalPage {
			props = props[:proposalPage]
			last := props[len(props)-1]
			next = proposalCursor{connections: last.Connections, id: last.ID}.encode()
		}
		var all []int64
		for _, p := range props {
			all = append(all, p.ids...)
		}
		rows, err = tx.Query(ctx, recordSelect+`WHERE pr.id = ANY($1) ORDER BY pr.id`, all)
		if err != nil {
			return err
		}
		recs, err := scanRecords(rows)
		if err != nil {
			return err
		}
		byID := map[int64]recordJSON{}
		for _, rec := range recs {
			byID[rec.ID] = rec
		}
		// The target of each proposal: the entity of its lowest-id record
		// still free (what Decide merges into), else of its lowest record.
		var pids, eids []int64
		for i := range props {
			item := &props[i].proposalJSON
			item.Records = []proposalRecordJSON{}
			item.TargetEntity = targetJSON{OtherRecords: []otherRecordJSON{}}
			var target, fallback int64
			for _, id := range slices.Sorted(slices.Values(props[i].ids)) {
				rec, ok := byID[id]
				if !ok {
					continue
				}
				item.Records = append(item.Records, proposalRecordJSON{rec, rec.EntityID != 0 && !rec.Locked})
				if rec.EntityID != 0 && fallback == 0 {
					fallback = rec.EntityID
				}
				if rec.EntityID != 0 && !rec.Locked && target == 0 {
					target = rec.EntityID
				}
			}
			if len(item.Records) > 0 {
				item.Name = item.Records[0].Name
			}
			if target == 0 {
				target = fallback
			}
			if target != 0 {
				pids, eids = append(pids, item.ID), append(eids, target)
			}
		}
		refs, err := loadRefs(ctx, tx, eids)
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, otherRecordsSQL, pids, eids, maxOtherRecords+1)
		if err != nil {
			return err
		}
		others := map[int64][]otherRecordJSON{}
		var (
			pid int64
			o   otherRecordJSON
		)
		if _, err := pgx.ForEachRow(rows, []any{&pid, &o.ID, &o.SourceKey, &o.ContextKey}, func() error {
			others[pid] = append(others[pid], o)
			return nil
		}); err != nil {
			return err
		}
		for i := range pids {
			ref, ok := refs[eids[i]]
			if !ok {
				continue
			}
			for k := range props {
				if props[k].ID != pids[i] {
					continue
				}
				t := &props[k].TargetEntity
				e := toEntity(ref)
				t.Entity = &e
				if o := others[pids[i]]; len(o) > 0 {
					t.OtherRecords = o
				}
				if len(t.OtherRecords) > maxOtherRecords {
					t.OtherRecords, t.OtherRecordsTruncated = t.OtherRecords[:maxOtherRecords], true
				}
			}
		}
		for _, p := range props {
			p.ReasonCode = reasonCode(p.Reason)
			items = append(items, p.proposalJSON)
		}
		return nil
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	if !s.audited(w, r, "identity.proposals.read", nil, map[string]any{"status": status, "count": len(items)}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// loadRefs returns the entities with these ids.
func loadRefs(ctx context.Context, tx pgx.Tx, ids []int64) (map[int64]graph.EntityRef, error) {
	out := map[int64]graph.EntityRef{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, kind, name, natural_key FROM entity WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	var (
		ref graph.EntityRef
		key *string
	)
	_, err = pgx.ForEachRow(rows, []any{&ref.ID, &ref.Kind, &ref.Name, &key}, func() error {
		ref.NaturalKey = ""
		if key != nil {
			ref.NaturalKey = *key
		}
		out[ref.ID] = ref
		return nil
	})
	return out, err
}

// readTx runs f in a read-only, repeatable-read transaction.
func (s *server) readTx(ctx context.Context, f func(pgx.Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // read-only
	return f(tx)
}

// decide: POST /api/v1/identity/proposals/{id}/decision
// {"confirm":bool,"expected_target_entity_id"?:int|null,"expected_record_ids"?:[int]} → 204.
// The expected fields are what the client was shown (the target entity and
// the free records); when present, identity checks them atomically with the
// decision and a difference is 409 with nothing done.
func (s *server) decide(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		fail(w, r, err)
		return
	}
	var body struct {
		Confirm *bool `json:"confirm"`
		// Raw so that null ("no target was shown") differs from absent.
		ExpectedTarget  json.RawMessage `json:"expected_target_entity_id"`
		ExpectedRecords []int64         `json:"expected_record_ids"`
	}
	if err := decodeBody(r, &body, false); err != nil {
		fail(w, r, err)
		return
	}
	if body.Confirm == nil || len(body.ExpectedRecords) > maxExpectedRecords || !positive(body.ExpectedRecords) {
		fail(w, r, errBadRequest)
		return
	}
	var exp identity.DecisionExpectation
	detail := map[string]any{"proposal_id": id, "confirm": *body.Confirm}
	if body.ExpectedTarget != nil {
		exp.CheckTarget = true
		if !bytes.Equal(body.ExpectedTarget, []byte("null")) {
			if err := json.Unmarshal(body.ExpectedTarget, &exp.TargetEntityID); err != nil || exp.TargetEntityID <= 0 {
				fail(w, r, errBadRequest)
				return
			}
			detail["expected_target_entity_id"] = exp.TargetEntityID
		} else {
			detail["expected_target_entity_id"] = nil
		}
	}
	if body.ExpectedRecords != nil {
		exp.RecordIDs = body.ExpectedRecords
		detail["expected_record_ids"] = body.ExpectedRecords
	}
	if !s.auditAttempt(w, r, "identity.decide", nil, detail) {
		return
	}
	if err := s.Identity.DecideExpecting(r.Context(), id, *body.Confirm, user(r).Sub, exp); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// positive: every id is > 0.
func positive(ids []int64) bool {
	for _, id := range ids {
		if id <= 0 {
			return false
		}
	}
	return true
}

type recordsBody struct {
	RecordIDs []int64 `json:"record_ids"`
	Reason    string  `json:"reason"`
	// EntityIDs (merge only): the entities the client saw the records in.
	EntityIDs []int64 `json:"entity_ids"`
	// EntityID (split only): the entity the client saw the records in.
	EntityID *int64 `json:"entity_id"`
}

// decodeRecords reads {"record_ids","reason"} with minIDs..100 positive ids
// and a reason of 1..500 characters, plus the optional expectation fields.
func decodeRecords(r *http.Request, minIDs int) (recordsBody, error) {
	var b recordsBody
	if err := decodeBody(r, &b, false); err != nil {
		return b, err
	}
	b.Reason = strings.TrimSpace(b.Reason)
	if len(b.RecordIDs) < minIDs || len(b.RecordIDs) > maxRecordIDs ||
		b.Reason == "" || utf8.RuneCountInString(b.Reason) > maxReasonRunes || !utf8.ValidString(b.Reason) {
		return b, errBadRequest
	}
	if !positive(b.RecordIDs) || !positive(b.EntityIDs) || (b.EntityID != nil && *b.EntityID <= 0) {
		return b, errBadRequest
	}
	return b, nil
}

// merge: POST /api/v1/identity/merge {"record_ids":[2..100],"reason","entity_ids"?:[1..10]} → {"entity_id"}.
// With entity_ids, record_ids must be exactly all the records of those
// entities when the merge runs (checked atomically with it), else 409.
func (s *server) merge(w http.ResponseWriter, r *http.Request) {
	b, err := decodeRecords(r, 2)
	if err == nil && (b.EntityID != nil || (b.EntityIDs != nil && (len(b.EntityIDs) == 0 || len(b.EntityIDs) > maxEntityIDs))) {
		err = errBadRequest
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	detail := map[string]any{"record_ids": b.RecordIDs}
	if b.EntityIDs != nil {
		detail["entity_ids"] = b.EntityIDs
	}
	s.recordsOp(w, r, "identity.merge", detail, func(ctx context.Context) (int64, error) {
		return s.Identity.MergeExpecting(ctx, b.RecordIDs, b.EntityIDs, user(r).Sub, b.Reason)
	})
}

// split: POST /api/v1/identity/split {"record_ids":[1..100],"reason","entity_id"?} → {"entity_id"}.
// With entity_id, every record must be in that entity and the entity must
// keep another record when the split runs (checked atomically with it), else 409.
func (s *server) split(w http.ResponseWriter, r *http.Request) {
	b, err := decodeRecords(r, 1)
	if err == nil && b.EntityIDs != nil {
		err = errBadRequest
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	detail := map[string]any{"record_ids": b.RecordIDs}
	var entity int64
	if b.EntityID != nil {
		entity = *b.EntityID
		detail["entity_id"] = entity
	}
	s.recordsOp(w, r, "identity.split", detail, func(ctx context.Context) (int64, error) {
		return s.Identity.SplitExpecting(ctx, b.RecordIDs, entity, user(r).Sub, b.Reason)
	})
}

// recordsOp audits the attempt, then runs op → {"entity_id"}. The reason is
// free text: it stays in identity_decision, not in the audit.
func (s *server) recordsOp(w http.ResponseWriter, r *http.Request, action string, detail map[string]any,
	op func(ctx context.Context) (int64, error)) {
	if !s.auditAttempt(w, r, action, nil, detail) {
		return
	}
	entity, err := op(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"entity_id": entity})
}
