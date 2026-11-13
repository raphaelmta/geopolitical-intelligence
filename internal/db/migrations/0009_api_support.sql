-- +goose Up

-- Run with the loaders stopped: plain CREATE INDEX inside the migration
-- transaction blocks writes on the indexed tables while the indexes build.

-- The API entity page lists a company's versions, newest first
-- (WHERE entity_id = $1 ORDER BY id DESC LIMIT n). company_version_open only
-- covers the open version; without this the page scans the table.
CREATE INDEX company_version_entity_id ON company_version (entity_id, id DESC);

-- The API lists decided and obsolete proposals as it lists the pending ones
-- (WHERE status = $1 ORDER BY connections DESC, id, keyset on that order).
-- identity_proposal_pending_queue (0002) is partial on pending; this one
-- serves the other statuses with the same order.
CREATE INDEX identity_proposal_status_queue ON identity_proposal (status, connections DESC, id);

-- The raw files each collection loaded: where the preserved object is
-- (raw_key in the raw store) and its SHA-256, so every claim can be traced to
-- the bytes it was read from. name is the file part of the claims'
-- record_ref (record_ref = name || ':' || line). Provenance: insert-only.
CREATE TABLE collection_file (
    collection_id bigint NOT NULL REFERENCES collection (id),
    name          text   NOT NULL CHECK (name <> ''),
    raw_key       text   NOT NULL CHECK (raw_key <> ''),
    sha256        text   NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes    bigint NOT NULL CHECK (size_bytes >= 0),
    PRIMARY KEY (collection_id, name)
);

CREATE TRIGGER collection_file_no_delete   BEFORE DELETE   ON collection_file FOR EACH ROW       EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER collection_file_no_truncate BEFORE TRUNCATE ON collection_file FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER collection_file_no_update   BEFORE UPDATE   ON collection_file FOR EACH ROW       EXECUTE FUNCTION forbid_update();

-- +goose Down
-- collection_file is provenance and is never dropped by a rollback.
DROP INDEX identity_proposal_status_queue;
DROP INDEX company_version_entity_id;
