-- +goose Up

CREATE TABLE source (
    id            smallint PRIMARY KEY,
    key           text     NOT NULL UNIQUE,
    name          text     NOT NULL,
    publish_every interval NOT NULL,
    stale_after   interval NOT NULL,
    snapshot      boolean  NOT NULL
);

CREATE TABLE collection (
    id             bigserial PRIMARY KEY,
    source_id      smallint    NOT NULL REFERENCES source (id),
    reference_date date        NOT NULL,
    scope          jsonb       NOT NULL DEFAULT '{}',
    status         text        NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'failed')),
    started_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    stats          jsonb,
    error          text
);

CREATE TABLE entity (
    id          bigserial PRIMARY KEY,
    kind        text NOT NULL CHECK (kind IN ('person', 'company', 'party', 'public_body', 'foreign')),
    natural_key text,
    name        text NOT NULL DEFAULT '',
    name_norm   text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX entity_kind_natural_key_key ON entity (kind, natural_key) WHERE natural_key IS NOT NULL;
CREATE INDEX entity_natural_key_prefix ON entity (natural_key text_pattern_ops);

CREATE TABLE person_record (
    id                    bigserial PRIMARY KEY,
    source_id             smallint NOT NULL REFERENCES source (id),
    context_key           text     NOT NULL,
    name                  text     NOT NULL DEFAULT '',
    name_norm             text     NOT NULL DEFAULT '',
    cpf_fragment          text     NOT NULL DEFAULT '',
    entity_id             bigint REFERENCES entity (id),
    identity_confidence   real     NOT NULL DEFAULT 1,
    locked_by_decision_id bigint,
    first_collection_id   bigint   NOT NULL REFERENCES collection (id),
    record_ref            text     NOT NULL CHECK (record_ref <> ''),
    CHECK (identity_confidence >= 0 AND identity_confidence <= 1),
    UNIQUE (source_id, context_key, name_norm, cpf_fragment)
);

CREATE TABLE relationship (
    id                      bigserial PRIMARY KEY,
    rel_type                text   NOT NULL CHECK (rel_type IN ('partner_of', 'administrator_of', 'legal_rep_of',
                                'donated_to', 'candidate_for', 'holds_mandate', 'controls', 'shareholder_of')),
    src_entity_id           bigint NOT NULL REFERENCES entity (id),
    dst_entity_id           bigint NOT NULL REFERENCES entity (id),
    src_record_id           bigint REFERENCES person_record (id),
    dst_record_id           bigint REFERENCES person_record (id),
    attrs                   jsonb  NOT NULL DEFAULT '{}',
    kind                    text   NOT NULL CHECK (kind IN ('fact', 'inference')),
    confidence              real,
    valid_from              date,
    valid_to_after          date,
    valid_to_before         date,
    first_collection_id     bigint NOT NULL REFERENCES collection (id),
    record_ref              text   NOT NULL CHECK (record_ref <> ''),
    last_seen_collection_id bigint REFERENCES collection (id),
    closed_collection_id    bigint REFERENCES collection (id),
    recorded_at             timestamptz NOT NULL DEFAULT now(),
    CHECK (kind = 'fact' OR confidence IS NOT NULL),
    CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1))
);
CREATE INDEX relationship_src_entity_id ON relationship (src_entity_id);
CREATE INDEX relationship_dst_entity_id ON relationship (dst_entity_id);
-- Natural key among open relationships. NULLS NOT DISTINCT makes a missing
-- role or valid_from count as equal, so re-asserting the same fact is idempotent.
-- Both ends use the person record when there is one, never the entity it
-- currently points to: identity resolution re-points entities, and that must
-- never make two distinct claims collide.
CREATE UNIQUE INDEX relationship_natural_key ON relationship (
    rel_type, (coalesce(src_record_id, -src_entity_id)), (coalesce(dst_record_id, -dst_entity_id)), (attrs ->> 'role'), valid_from
) NULLS NOT DISTINCT WHERE closed_collection_id IS NULL;

CREATE TABLE company_version (
    id                      bigserial PRIMARY KEY,
    entity_id               bigint NOT NULL REFERENCES entity (id),
    name                    text   NOT NULL DEFAULT '',
    legal_nature            text   NOT NULL DEFAULT '',
    size                    text   NOT NULL DEFAULT '',
    uf                      text   NOT NULL DEFAULT '',
    city                    text   NOT NULL DEFAULT '',
    cnae                    text   NOT NULL DEFAULT '',
    status                  text   NOT NULL DEFAULT '',
    capital                 numeric(18, 2),
    first_collection_id     bigint NOT NULL REFERENCES collection (id),
    record_ref              text   NOT NULL CHECK (record_ref <> ''),
    last_seen_collection_id bigint REFERENCES collection (id),
    closed_collection_id    bigint REFERENCES collection (id),
    recorded_at             timestamptz NOT NULL DEFAULT now()
);
-- At most one open version per company.
CREATE UNIQUE INDEX company_version_open ON company_version (entity_id) WHERE closed_collection_id IS NULL;

-- +goose StatementBegin
CREATE FUNCTION forbid_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'claims are never deleted';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER entity_no_delete          BEFORE DELETE ON entity          FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER person_record_no_delete   BEFORE DELETE ON person_record   FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER relationship_no_delete    BEFORE DELETE ON relationship    FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER company_version_no_delete BEFORE DELETE ON company_version FOR EACH ROW EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER collection_no_delete      BEFORE DELETE ON collection      FOR EACH ROW EXECUTE FUNCTION forbid_delete();

CREATE TRIGGER entity_no_truncate          BEFORE TRUNCATE ON entity          FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER person_record_no_truncate   BEFORE TRUNCATE ON person_record   FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER relationship_no_truncate    BEFORE TRUNCATE ON relationship    FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER company_version_no_truncate BEFORE TRUNCATE ON company_version FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER collection_no_truncate      BEFORE TRUNCATE ON collection      FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();

-- Update guard, default-deny: every column except the allowlist passed as trigger
-- arguments must be unchanged, so a column added later is protected automatically.
-- Closing is one-way (a closed row stays closed) and last_seen never goes back.
-- +goose StatementBegin
CREATE FUNCTION guard_update() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    o jsonb := to_jsonb(OLD);
    n jsonb := to_jsonb(NEW);
BEGIN
    IF (n - TG_ARGV) IS DISTINCT FROM (o - TG_ARGV) THEN
        RAISE EXCEPTION '% claim columns are immutable', TG_TABLE_NAME;
    END IF;
    IF o ? 'closed_collection_id' AND o -> 'closed_collection_id' <> 'null'::jsonb
       AND n -> 'closed_collection_id' IS DISTINCT FROM o -> 'closed_collection_id' THEN
        RAISE EXCEPTION '% rows cannot be reopened', TG_TABLE_NAME;
    END IF;
    IF o ? 'last_seen_collection_id' AND o ->> 'last_seen_collection_id' IS NOT NULL
       AND ((n ->> 'last_seen_collection_id') IS NULL
            OR (n ->> 'last_seen_collection_id')::bigint < (o ->> 'last_seen_collection_id')::bigint) THEN
        RAISE EXCEPTION '% last_seen_collection_id cannot decrease', TG_TABLE_NAME;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER relationship_guard_update BEFORE UPDATE ON relationship FOR EACH ROW EXECUTE FUNCTION guard_update(
    'src_entity_id', 'dst_entity_id', 'last_seen_collection_id', 'closed_collection_id', 'valid_to_after', 'valid_to_before');
CREATE TRIGGER person_record_guard_update BEFORE UPDATE ON person_record FOR EACH ROW EXECUTE FUNCTION guard_update(
    'entity_id', 'identity_confidence', 'locked_by_decision_id');
CREATE TRIGGER company_version_guard_update BEFORE UPDATE ON company_version FOR EACH ROW EXECUTE FUNCTION guard_update(
    'last_seen_collection_id', 'closed_collection_id');

-- +goose Down
-- Intentionally empty: the schema holds claims, which are never deleted.
