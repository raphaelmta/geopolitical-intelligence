-- +goose Up

-- Audit trail of what authenticated users did (internal/audit). Append-only:
-- rows are never updated or deleted, not even by the application. entity_id
-- has no foreign key on purpose, so auditing never blocks and survives
-- anything that happens to the entity table.
-- detail holds structured, non-personal context only (ids, filters, counts):
-- never tokens, never free text about people.
CREATE TABLE audit_log (
    id        bigserial   PRIMARY KEY,
    at        timestamptz NOT NULL DEFAULT now(),
    user_sub  text        NOT NULL CHECK (btrim(user_sub, E' \t\r\n') <> ''),
    action    text        NOT NULL CHECK (btrim(action, E' \t\r\n') <> ''),
    entity_id bigint,
    detail    jsonb       NOT NULL DEFAULT '{}'
);

CREATE INDEX audit_log_at ON audit_log (at);
CREATE INDEX audit_log_user_sub_at ON audit_log (user_sub, at);
CREATE INDEX audit_log_entity_id_at ON audit_log (entity_id, at) WHERE entity_id IS NOT NULL;

-- No update at all, not even a no-op one (forbid_update() of 0002, as for
-- identity decisions), and no delete or truncate (forbid_delete() of 0001).
CREATE TRIGGER audit_log_no_update   BEFORE UPDATE   ON audit_log FOR EACH ROW       EXECUTE FUNCTION forbid_update();
CREATE TRIGGER audit_log_no_delete   BEFORE DELETE   ON audit_log FOR EACH ROW       EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();

-- +goose Down
-- Intentionally empty: the audit trail is never dropped by a rollback.
