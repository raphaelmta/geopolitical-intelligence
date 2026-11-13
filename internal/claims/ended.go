package claims

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AssertEndedRelationship records a relationship claim that the source
// declares ended (a finished mandate, a party left). end is EXCLUSIVE: the
// first day on which the relationship no longer holds (see the package
// doc); it must be after in.ValidFrom when that is set (ErrEmptyPeriod).
// Such a claim is
// never open: "open" means closed_collection_id IS NULL everywhere in the
// platform, and an ended link must not show as a current one. It returns the
// id of the claim. With the natural key of AssertRelationship (type, source,
// destination, role, valid_from):
//
//   - an OPEN claim with the key (asserted while the source still listed it
//     as current) is closed now: closed_collection_id = this collection,
//     valid_to_after = valid_to_before = end;
//   - else a CLOSED claim with the key and the same declared end is only
//     marked as seen again (last_seen_collection_id advances);
//   - else a new claim is inserted already closed by this collection, with
//     valid_to_after = valid_to_before = end.
//
// A later end for the same start is a new claim, kept beside the old one.
// Ends that are person records resolve their entity as in AssertRelationship.
//
// Writers of one claim are serialised here by a transaction advisory lock,
// but AssertRelationship takes none: callers must not assert the same key
// open and ended concurrently. The Câmara and Senado connectors guarantee
// it by holding a per-source load lock for the whole load (legis.LoadLock)
// and writing sequentially.
func (s *Store) AssertEndedRelationship(ctx context.Context, in RelationshipInput, end time.Time) (int64, error) {
	attrs, err := validRelationship(in)
	if err != nil {
		return 0, err
	}
	if in.ValidFrom != nil && !end.After(*in.ValidFrom) {
		return 0, ErrEmptyPeriod
	}
	src, dst := endKey(in.SrcRecordID, in.SrcEntityID), endKey(in.DstRecordID, in.DstEntityID)
	var roleArg *string
	if v, ok := in.Attrs["role"]; ok {
		r, isString := v.(string)
		if !isString {
			return 0, errors.New("claims: role must be a string")
		}
		roleArg = &r
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("claims: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Serialises writers of this one claim: the unique index covers open
	// rows only, so nothing else stops two closed duplicates.
	lockKey := fmt.Sprintf("ended|%s|%s|%s|%s|%s", in.Type, src, dst, optional(roleArg), optionalDate(in.ValidFrom))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return 0, fmt.Errorf("claims: lock claim: %w", err)
	}

	srcPred, srcArg := endPredicate("src", in.SrcRecordID, in.SrcEntityID)
	dstPred, dstArg := endPredicate("dst", in.DstRecordID, in.DstEntityID)
	var (
		id   int64
		open bool
	)
	err = tx.QueryRow(ctx, `
		SELECT id, closed_collection_id IS NULL FROM relationship
		WHERE rel_type = $1 AND `+srcPred+` AND `+dstPred+`
			AND (attrs ->> 'role') IS NOT DISTINCT FROM $4 AND valid_from IS NOT DISTINCT FROM $5::date
			AND (closed_collection_id IS NULL OR (valid_to_after = $6::date AND valid_to_before = $6::date))
		ORDER BY closed_collection_id IS NULL DESC, id
		LIMIT 1
		FOR UPDATE`,
		in.Type, srcArg, dstArg, roleArg, in.ValidFrom, end).Scan(&id, &open)
	switch {
	case err == nil && open:
		_, err = tx.Exec(ctx, `UPDATE relationship SET closed_collection_id = $2, valid_to_after = $3, valid_to_before = $3,
			last_seen_collection_id = GREATEST(last_seen_collection_id, $2) WHERE id = $1`, id, in.Prov.CollectionID, end)
		if err != nil {
			return 0, fmt.Errorf("claims: close ended relationship: %w", err)
		}
		return id, commit(ctx, tx)
	case err == nil:
		_, err = tx.Exec(ctx, `UPDATE relationship SET last_seen_collection_id = GREATEST(last_seen_collection_id, $2)
			WHERE id = $1`, id, in.Prov.CollectionID)
		if err != nil {
			return 0, fmt.Errorf("claims: mark ended relationship seen: %w", err)
		}
		return id, commit(ctx, tx)
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("claims: find ended relationship: %w", err)
	}

	err = tx.QueryRow(ctx, `
		WITH rec AS (
			SELECT id, entity_id FROM person_record WHERE id IN ($4::bigint, $5::bigint) ORDER BY id FOR SHARE
		)
		INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, src_record_id, dst_record_id,
			attrs, kind, confidence, valid_from, valid_to_after, valid_to_before,
			first_collection_id, record_ref, last_seen_collection_id, closed_collection_id)
		SELECT $1::text,
			CASE WHEN $4::bigint IS NULL THEN $2::bigint ELSE (SELECT entity_id FROM rec WHERE id = $4::bigint) END,
			CASE WHEN $5::bigint IS NULL THEN $3::bigint ELSE (SELECT entity_id FROM rec WHERE id = $5::bigint) END,
			$4::bigint, $5::bigint, $6::jsonb, $7::text, $8::real, $9::date, $12::date, $12::date,
			$10::bigint, $11::text, $10::bigint, $10::bigint
		RETURNING id`,
		in.Type, in.SrcEntityID, in.DstEntityID, in.SrcRecordID, in.DstRecordID,
		attrs, in.Kind, in.Confidence, in.ValidFrom, in.Prov.CollectionID, in.Prov.RecordRef, end).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23502" &&
		((pgErr.ColumnName == "src_entity_id" && in.SrcRecordID != nil) ||
			(pgErr.ColumnName == "dst_entity_id" && in.DstRecordID != nil)) {
		return 0, fmt.Errorf("claims: assert ended relationship: %w", ErrUnknownRecord)
	}
	if err != nil {
		return 0, fmt.Errorf("claims: assert ended relationship: %w", err)
	}
	return id, commit(ctx, tx)
}

// validRelationship checks a relationship input and returns its attrs as JSON.
func validRelationship(in RelationshipInput) (string, error) {
	if !in.Prov.valid() {
		return "", ErrNoProvenance
	}
	if in.Kind == Inference && in.Confidence == nil {
		return "", ErrInferenceNeedsConfidence
	}
	// !(0 <= c <= 1) also catches NaN, which every comparison rejects.
	if in.Confidence != nil && !(*in.Confidence >= 0 && *in.Confidence <= 1) {
		return "", ErrConfidenceRange
	}
	attrs := in.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	raw, err := json.Marshal(attrs)
	if err != nil {
		return "", fmt.Errorf("claims: encode attrs: %w", err)
	}
	return string(raw), nil
}

// endKey is one end of the natural key: the record when there is one.
func endKey(record *int64, entity int64) string {
	if record != nil {
		return "r" + strconv.FormatInt(*record, 10)
	}
	return "e" + strconv.FormatInt(entity, 10)
}

// endPredicate matches one end of the natural key with an indexed column:
// the record id when there is one, else the entity of a row without record.
// The second argument is $2 (src) or $3 (dst) of the lookup.
func endPredicate(side string, record *int64, entity int64) (string, int64) {
	n := "$2"
	if side == "dst" {
		n = "$3"
	}
	if record != nil {
		return side + "_record_id = " + n + "::bigint", *record
	}
	return "(" + side + "_record_id IS NULL AND " + side + "_entity_id = " + n + "::bigint)", entity
}

func optional(s *string) string {
	if s == nil {
		return "-"
	}
	return strconv.Quote(*s)
}

func optionalDate(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.DateOnly)
}

// FindPersonRecordByContext returns the oldest person record of the source
// with this context key, whatever its name or CPF fragment. Sources whose
// context key identifies one person (a parliamentarian's id) use it to keep
// one record per person when a later response spells the name differently.
func (s *Store) FindPersonRecordByContext(ctx context.Context, sourceID int16, contextKey string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM person_record WHERE source_id = $1 AND context_key = $2
		ORDER BY id LIMIT 1`, sourceID, contextKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("claims: find person record by context: %w", err)
	}
	return id, true, nil
}
