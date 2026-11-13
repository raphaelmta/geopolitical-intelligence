package claims

import (
	"context"
	"errors"
	"fmt"

	"geoint/internal/identity"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store writes claims. All writes are single-row and idempotent: asserting
// the same claim again only records that it was seen again.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store on the pool.
func NewStore(p *pgxpool.Pool) *Store { return &Store{pool: p} }

// UpsertEntity returns the id of the entity with this kind and natural key,
// creating it when missing. An existing entity keeps its name.
func (s *Store) UpsertEntity(ctx context.Context, kind EntityKind, naturalKey, name string) (int64, error) {
	if naturalKey == "" {
		return 0, errors.New("claims: entity natural key is required")
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM entity WHERE kind=$1 AND natural_key=$2`, kind, naturalKey).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("claims: find entity: %w", err)
	}
	// DO UPDATE with no change makes RETURNING work when another writer won the race.
	err = s.pool.QueryRow(ctx, `
		INSERT INTO entity (kind, natural_key, name, name_norm) VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, natural_key) WHERE natural_key IS NOT NULL
		DO UPDATE SET natural_key = entity.natural_key
		RETURNING id`, kind, naturalKey, name, identity.NormalizeName(name)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("claims: upsert entity: %w", err)
	}
	return id, nil
}

// UpsertPersonRecord returns the person record for this source, context, name
// and CPF fragment, creating it together with its own singleton person entity
// when new. Identity resolution may later repoint the record to another entity.
func (s *Store) UpsertPersonRecord(ctx context.Context, in PersonRecordInput) (recordID, entityID int64, err error) {
	if !in.Prov.valid() {
		return 0, 0, ErrNoProvenance
	}
	norm := identity.NormalizeName(in.Name)
	// Common case first: an existing record is never rewritten.
	if recordID, entityID, found, err := s.findPersonRecord(ctx, s.pool, in, norm); err != nil || found {
		return recordID, entityID, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("claims: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// The record goes in first, without an entity, so that losing a race never
	// leaves an orphan entity behind (entities cannot be deleted).
	err = tx.QueryRow(ctx, `
		INSERT INTO person_record (source_id, context_key, name, name_norm, cpf_fragment, first_collection_id, record_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (source_id, context_key, name_norm, cpf_fragment) DO NOTHING
		RETURNING id`,
		in.SourceID, in.ContextKey, in.Name, norm, in.CPFFragment, in.Prov.CollectionID, in.Prov.RecordRef).
		Scan(&recordID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another writer won the race and has committed by now.
		recordID, entityID, found, err := s.findPersonRecord(ctx, tx, in, norm)
		if err != nil {
			return 0, 0, err
		}
		if !found {
			return 0, 0, errors.New("claims: person record vanished after conflict")
		}
		return recordID, entityID, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("claims: insert person record: %w", err)
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO entity (kind, name, name_norm) VALUES ('person', $1, $2) RETURNING id`, in.Name, norm).Scan(&entityID)
	if err != nil {
		return 0, 0, fmt.Errorf("claims: create person entity: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE person_record SET entity_id=$2 WHERE id=$1`, recordID, entityID); err != nil {
		return 0, 0, fmt.Errorf("claims: link person entity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("claims: commit: %w", err)
	}
	return recordID, entityID, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) findPersonRecord(ctx context.Context, q queryRower, in PersonRecordInput, norm string) (recordID, entityID int64, found bool, err error) {
	var ent *int64
	err = q.QueryRow(ctx, `SELECT id, entity_id FROM person_record
		WHERE source_id=$1 AND context_key=$2 AND name_norm=$3 AND cpf_fragment=$4`,
		in.SourceID, in.ContextKey, norm, in.CPFFragment).Scan(&recordID, &ent)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("claims: find person record: %w", err)
	}
	if ent != nil {
		entityID = *ent
	}
	return recordID, entityID, true, nil
}

// AssertRelationship records a relationship claim, or marks it as seen again
// in this collection when the same open claim already exists. It returns the
// id of the claim. The natural key (type, source, destination, role,
// valid_from) excludes kind, confidence and record_ref: re-asserting an
// existing open claim only advances last_seen and never changes its kind.
// Each end of the key is the person record when one is given, else the entity.
//
// When SrcRecordID or DstRecordID is set, the entity id stored for that end is
// the record's current entity, read under a share lock in the same statement;
// the caller's SrcEntityID/DstEntityID for that end is ignored (it may be zero
// or stale). Identity resolution moves records between entities concurrently,
// and the lock makes the claim either see the move or wait for it. Records are
// locked in id order, like the resolver does, so the two never deadlock.
// A record that does not exist or has no entity gives ErrUnknownRecord.
func (s *Store) AssertRelationship(ctx context.Context, in RelationshipInput) (int64, error) {
	raw, err := validRelationship(in)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `
		WITH rec AS (
			SELECT id, entity_id FROM person_record WHERE id IN ($4::bigint, $5::bigint) ORDER BY id FOR SHARE
		)
		INSERT INTO relationship (rel_type, src_entity_id, dst_entity_id, src_record_id, dst_record_id,
			attrs, kind, confidence, valid_from, first_collection_id, record_ref, last_seen_collection_id)
		SELECT $1::text,
			CASE WHEN $4::bigint IS NULL THEN $2::bigint ELSE (SELECT entity_id FROM rec WHERE id = $4::bigint) END,
			CASE WHEN $5::bigint IS NULL THEN $3::bigint ELSE (SELECT entity_id FROM rec WHERE id = $5::bigint) END,
			$4::bigint, $5::bigint, $6::jsonb, $7::text, $8::real, $9::date, $10::bigint, $11::text, $10::bigint
		ON CONFLICT (rel_type, (coalesce(src_record_id, -src_entity_id)), (coalesce(dst_record_id, -dst_entity_id)),
			(attrs ->> 'role'), valid_from)
			WHERE closed_collection_id IS NULL
		DO UPDATE SET last_seen_collection_id = GREATEST(relationship.last_seen_collection_id, EXCLUDED.last_seen_collection_id)
		RETURNING id`,
		in.Type, in.SrcEntityID, in.DstEntityID, in.SrcRecordID, in.DstRecordID,
		raw, in.Kind, in.Confidence, in.ValidFrom, in.Prov.CollectionID, in.Prov.RecordRef).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23502" &&
		((pgErr.ColumnName == "src_entity_id" && in.SrcRecordID != nil) ||
			(pgErr.ColumnName == "dst_entity_id" && in.DstRecordID != nil)) {
		// The record's entity came back NULL: no such record, or not linked.
		return 0, fmt.Errorf("claims: assert relationship: %w", ErrUnknownRecord)
	}
	if err != nil {
		return 0, fmt.Errorf("claims: assert relationship: %w", err)
	}
	return id, nil
}

// PutCompanyVersion records the registry data of a company. If it equals the
// open version, that version is marked as seen again; otherwise the open
// version is closed (and kept) and a new one is opened.
func (s *Store) PutCompanyVersion(ctx context.Context, in CompanyVersionInput) error {
	if !in.Prov.valid() {
		return ErrNoProvenance
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("claims: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var openID int64
	var same bool
	err = tx.QueryRow(ctx, `
		SELECT id, (name, legal_nature, size, uf, city, cnae, status, capital)
			IS NOT DISTINCT FROM ($2::text, $3::text, $4::text, $5::text, $6::text, $7::text, $8::text, $9::numeric(18,2))
		FROM company_version WHERE entity_id=$1 AND closed_collection_id IS NULL FOR UPDATE`,
		in.EntityID, in.Name, in.LegalNature, in.Size, in.UF, in.City, in.CNAE, in.Status, in.Capital).Scan(&openID, &same)
	switch {
	case err == nil && same:
		_, err = tx.Exec(ctx, `UPDATE company_version
			SET last_seen_collection_id = GREATEST(last_seen_collection_id, $2) WHERE id=$1`, openID, in.Prov.CollectionID)
		if err != nil {
			return fmt.Errorf("claims: mark company version seen: %w", err)
		}
		return commit(ctx, tx)
	case err == nil:
		if _, err := tx.Exec(ctx, `UPDATE company_version SET closed_collection_id=$2 WHERE id=$1`, openID, in.Prov.CollectionID); err != nil {
			return fmt.Errorf("claims: close company version: %w", err)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("claims: find open company version: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO company_version (entity_id, name, legal_nature, size, uf, city, cnae, status, capital,
			first_collection_id, record_ref, last_seen_collection_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $10)`,
		in.EntityID, in.Name, in.LegalNature, in.Size, in.UF, in.City, in.CNAE, in.Status, in.Capital,
		in.Prov.CollectionID, in.Prov.RecordRef)
	if err != nil {
		return fmt.Errorf("claims: insert company version: %w", err)
	}
	return commit(ctx, tx)
}

func commit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("claims: commit: %w", err)
	}
	return nil
}
