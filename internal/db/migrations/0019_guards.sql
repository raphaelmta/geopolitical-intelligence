-- +goose Up

-- Safe on a populated database: CREATE TRIGGER is a catalogue change (no
-- table scan) that takes a SHARE ROW EXCLUSIVE lock on its table for an
-- instant. Run with the loaders stopped.

-- collection: what every closing interval and staleness check depends on
-- (reference_date, observed_date, scope, source, started_at) never changes;
-- only the columns that finishing a run writes (collection.Finish, Fail,
-- FailDead and the scheduler's stale rule) may: status, finished_at, stats,
-- error. Default-deny, like the claim tables (guard_update, 0001).
CREATE TRIGGER collection_guard_update BEFORE UPDATE ON collection FOR EACH ROW EXECUTE FUNCTION guard_update(
    'status', 'finished_at', 'stats', 'error');

-- A finished collection (done or failed) keeps its status.
-- +goose StatementBegin
CREATE FUNCTION guard_collection_status() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status <> 'running' AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'collection % is % and cannot become %', OLD.id, OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER collection_status_one_way BEFORE UPDATE ON collection FOR EACH ROW EXECUTE FUNCTION guard_collection_status();

-- entity: kind, natural_key, name and name_norm never change (no column
-- may; guard_update with no argument would compare NULLs and allow anything,
-- hence a function of its own). A no-op update (claims.Store.UpsertEntity's
-- ON CONFLICT DO UPDATE that only returns the id) still passes.
-- +goose StatementBegin
CREATE FUNCTION guard_entity_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD) THEN
        RAISE EXCEPTION 'entity columns are immutable';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER entity_guard_update BEFORE UPDATE ON entity FOR EACH ROW EXECUTE FUNCTION guard_entity_update();

-- collection_request: the record behind "do not ask again"; history, never
-- deleted. Only published_at is stamped after the insert.
CREATE TRIGGER collection_request_guard_update BEFORE UPDATE ON collection_request FOR EACH ROW EXECUTE FUNCTION guard_update(
    'published_at');
CREATE TRIGGER collection_request_no_delete   BEFORE DELETE   ON collection_request FOR EACH ROW       EXECUTE FUNCTION forbid_delete();
CREATE TRIGGER collection_request_no_truncate BEFORE TRUNCATE ON collection_request FOR EACH STATEMENT EXECUTE FUNCTION forbid_delete();

-- +goose Down
DROP TRIGGER collection_request_no_truncate ON collection_request;
DROP TRIGGER collection_request_no_delete ON collection_request;
DROP TRIGGER collection_request_guard_update ON collection_request;
DROP TRIGGER entity_guard_update ON entity;
DROP FUNCTION guard_entity_update();
DROP TRIGGER collection_status_one_way ON collection;
DROP FUNCTION guard_collection_status();
DROP TRIGGER collection_guard_update ON collection;
