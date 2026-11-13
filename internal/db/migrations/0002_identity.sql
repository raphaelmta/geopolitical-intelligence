-- +goose Up

-- Audit trail of every identity change: who (the resolver or an analyst)
-- grouped, split or rejected which person records, and with what confidence.
-- Rows are immutable and never deleted.
CREATE TABLE identity_decision (
    id         bigserial PRIMARY KEY,
    kind       text        NOT NULL CHECK (kind IN ('auto', 'analyst')),
    action     text        NOT NULL CHECK (action IN ('merge', 'split', 'reject')),
    record_ids bigint[]    NOT NULL CHECK (cardinality(record_ids) > 0),
    -- The entity the records were placed in; NULL only for a reject.
    entity_id  bigint REFERENCES entity (id),
    confidence real CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1)),
    reason     text        NOT NULL DEFAULT '',
    decided_by text        NOT NULL CHECK (decided_by <> ''),
    decided_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((action = 'reject') = (entity_id IS NULL))
);

-- A group of records sharing name and CPF fragment whose name is too common
-- to merge automatically, waiting for (or decided by) an analyst. One per group.
CREATE TABLE identity_proposal (
    id          bigserial PRIMARY KEY,
    -- name_norm || '|' || cpf_fragment; unambiguous because name_norm has no '|'.
    group_key   text     NOT NULL UNIQUE,
    record_ids  bigint[] NOT NULL CHECK (cardinality(record_ids) > 1),
    reason      text     NOT NULL DEFAULT '',
    -- obsolete: closed by the resolver because nothing is left to decide (the
    -- records came together or got locked); it reopens if the group splits again.
    status      text     NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'confirmed', 'rejected', 'obsolete')),
    -- Number of relationships citing the records; the pending queue is ordered by it.
    connections int      NOT NULL DEFAULT 0 CHECK (connections >= 0),
    decided_by  text,
    decided_at  timestamptz,
    -- The analyst decision that confirmed or rejected the proposal. Not in the
    -- original column list: a confirmed key keeps attracting new records, and
    -- the resolver finds the confirmed entity through the records this decision locked.
    decision_id bigint REFERENCES identity_decision (id),
    CHECK ((status = 'pending') = (decided_by IS NULL AND decided_at IS NULL)),
    CHECK ((status IN ('confirmed', 'rejected')) = (decision_id IS NOT NULL))
);

-- Keys (name_norm || '|' || cpf_fragment) that an analyst Split found on both
-- sides: the same key belongs to more than one person, so new records with it
-- are proposed, never merged automatically.
CREATE TABLE identity_ambiguous_key (
    group_key   text PRIMARY KEY,
    decision_id bigint      NOT NULL REFERENCES identity_decision (id),
    flagged_at  timestamptz NOT NULL DEFAULT now()
);

-- Collections whose Resolve completed. A collection with person records and no
-- row here is resolved again by the next Resolve (a failed run is recovered).
CREATE TABLE identity_resolve_done (
    collection_id bigint PRIMARY KEY REFERENCES collection (id),
    resolved_at   timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE person_record
    ADD CONSTRAINT person_record_locked_by_decision_id_fkey
    FOREIGN KEY (locked_by_decision_id) REFERENCES identity_decision (id);

-- Indexes for identity resolution at national scale (~18M person records,
-- ~27M relationships in one Receita collection).

-- Groups are (name_norm, cpf_fragment): Resolve loads every record of each
-- touched name through this index, and PossibleSame looks names up with it.
CREATE INDEX person_record_name_norm_cpf_fragment ON person_record (name_norm, cpf_fragment);
-- Resolve walks the distinct names a collection touched in keyset batches
-- (WHERE first_collection_id = $1 AND name_norm > $cursor ORDER BY name_norm),
-- an index-only scan instead of a full scan of person_record per batch.
CREATE INDEX person_record_first_collection_name_norm ON person_record (first_collection_id, name_norm);
-- PossibleSame starts from an entity's records; analyst operations check
-- which records are left in an entity after moving some out.
CREATE INDEX person_record_entity_id ON person_record (entity_id);
-- Re-pointing relationships after a merge or split finds them by the record
-- they cite. Partial: only person-side endpoints carry a record id.
CREATE INDEX relationship_src_record_id ON relationship (src_record_id) WHERE src_record_id IS NOT NULL;
CREATE INDEX relationship_dst_record_id ON relationship (dst_record_id) WHERE dst_record_id IS NOT NULL;
-- The consistency sweep at the end of Resolve checks only the relationships
-- the resolved collection created; without this a small collection would scan
-- every relationship of the national load.
CREATE INDEX relationship_first_collection_id ON relationship (first_collection_id);
-- The analyst queue: pending proposals, most connected first.
CREATE INDEX identity_proposal_pending_queue ON identity_proposal (connections DESC, id) WHERE status = 'pending';

-- Decisions are an audit trail: never deleted, never changed.
-- +goose StatementBegin
CREATE FUNCTION forbid_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME;
END
$$;
-- +goose StatementEnd

-- A proposal may be refreshed while pending or obsolete (records, reason,
-- connections, reopening) and decided once by an analyst; a confirmed or
-- rejected proposal never changes. group_key never changes.
-- +goose StatementBegin
CREATE FUNCTION guard_proposal_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.group_key IS DISTINCT FROM OLD.group_key THEN
        RAISE EXCEPTION 'identity_proposal identity columns are immutable';
    END IF;
    IF OLD.status IN ('confirmed', 'rejected') AND to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD) THEN
        RAISE EXCEPTION 'identity_proposal % is decided and cannot change', OLD.id;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER identity_decision_no_delete   BEFORE DELETE   ON identity_decision FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER identity_decision_no_truncate BEFORE TRUNCATE ON identity_decision FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER identity_decision_no_update   BEFORE UPDATE   ON identity_decision FOR EACH ROW EXECUTE FUNCTION forbid_update();
CREATE TRIGGER identity_proposal_no_delete   BEFORE DELETE   ON identity_proposal FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER identity_proposal_no_truncate BEFORE TRUNCATE ON identity_proposal FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER identity_ambiguous_key_no_delete    BEFORE DELETE   ON identity_ambiguous_key FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER identity_ambiguous_key_no_truncate  BEFORE TRUNCATE ON identity_ambiguous_key FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER identity_ambiguous_key_no_update    BEFORE UPDATE   ON identity_ambiguous_key FOR EACH ROW EXECUTE FUNCTION forbid_update();
CREATE TRIGGER identity_resolve_done_no_delete     BEFORE DELETE   ON identity_resolve_done FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER identity_resolve_done_no_truncate   BEFORE TRUNCATE ON identity_resolve_done FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER identity_resolve_done_no_update     BEFORE UPDATE   ON identity_resolve_done FOR EACH ROW EXECUTE FUNCTION forbid_update();
CREATE TRIGGER identity_proposal_guard_update BEFORE UPDATE  ON identity_proposal FOR EACH ROW EXECUTE FUNCTION guard_proposal_update();

-- +goose Down
-- Intentionally empty: identity decisions are an audit trail and are never deleted.
