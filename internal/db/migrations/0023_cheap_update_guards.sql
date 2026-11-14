-- +goose Up

-- Cheap update guards for the three big claim tables (relationship,
-- person_record, company_version). The rule does not change: the guard of
-- migration 0001 (guard_update, default-deny with an allow-list) stays the
-- one and only judge of every update it sees. What changes is that it no
-- longer sees the updates it would certainly let through: each trigger gets
-- a WHEN clause, evaluated by the executor without calling any function per
-- row, that is true whenever guard_update could refuse the row:
--
--   (a) a column outside the allow-list changes — ROW(OLD.c, …) IS DISTINCT
--       FROM ROW(NEW.c, …) over every such column, listed from the catalogue
--       when the trigger is installed;
--   (b) a closed row's closed_collection_id changes (reopening);
--   (c) a non-NULL last_seen_collection_id becomes NULL or smaller;
--   (d) the table's columns are no longer the ones (a) lists:
--       update_guard_columns_changed below. A column added later without
--       re-installing the guard makes (d) true for every row, so every
--       update goes through guard_update again and the new column is
--       immutable exactly as before this migration (default-deny at run
--       time); TestUpdateGuardCatalogue fails until a migration decides the
--       column (re-run install_update_guard, with the column on the
--       allow-list if it may change).
--
-- A row where the clause is true runs guard_update itself, with the same
-- arguments: same refusals, same order of checks, same messages and
-- SQLSTATE. A row where it is false would have passed guard_update: the
-- clause compares with each type's equality what guard_update compared as
-- jsonb (same result for the types of these tables: bigint, smallint, text,
-- date, timestamptz, real, numeric, jsonb). The "seen" updates of a re-load
-- (last_seen_collection_id moving forward) and the identity re-pointing
-- (entity ids) no longer call a function at all.
--
-- (d) is a plpgsql function declared IMMUTABLE on purpose although it reads
-- the catalogue: with constant arguments, the planner evaluates it once when
-- a statement starts (trigger WHEN clauses are prepared for each execution
-- of a statement and never kept in a cached plan), not once per row. It is
-- meant for these WHEN clauses only.
--
-- Safe on the national base (≈ 70 M company_version, 50 M relationship
-- rows): CREATE OR REPLACE TRIGGER rewrites one pg_trigger row, it neither
-- scans nor rewrites a table. It takes a SHARE ROW EXCLUSIVE lock on each of
-- the three tables (reads go on, writes wait) for milliseconds; the old
-- definition is replaced in place under the same name, inside this
-- migration's single transaction, so no moment exists without a guard.
-- `make migrate` stops the api and the worker first, so nothing waits.

-- +goose StatementBegin
CREATE FUNCTION update_guard_columns_changed(tbl regclass, cols text[]) RETURNS boolean
    LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    RETURN ARRAY(SELECT a.attname::text FROM pg_catalog.pg_attribute a
                 WHERE a.attrelid = tbl AND a.attnum > 0 AND NOT a.attisdropped
                 ORDER BY a.attnum) IS DISTINCT FROM cols;
END
$$;
-- +goose StatementEnd

-- Installs (or re-installs) <table>_guard_update: guard_update(allowed…)
-- behind the WHEN clause described above, generated from the table's
-- current columns. A later migration that adds a column to one of these
-- tables calls it again, with the column on the allow-list if it may change.
-- +goose StatementBegin
CREATE FUNCTION install_update_guard(tbl regclass, allowed text[]) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    cols   text[] := ARRAY(SELECT a.attname::text FROM pg_catalog.pg_attribute a
                           WHERE a.attrelid = tbl AND a.attnum > 0 AND NOT a.attisdropped
                           ORDER BY a.attnum);
    frozen text[];
    cond   text;
BEGIN
    IF cardinality(allowed) = 0 OR NOT allowed <@ cols THEN
        RAISE EXCEPTION 'install_update_guard(%): the allow-list % must name columns of the table', tbl, allowed;
    END IF;
    frozen := ARRAY(SELECT c FROM unnest(cols) WITH ORDINALITY AS u(c, n) WHERE c <> ALL (allowed) ORDER BY n);
    IF cardinality(frozen) = 0 THEN
        RAISE EXCEPTION 'install_update_guard(%): no immutable column left', tbl;
    END IF;
    cond := format('ROW(%s) IS DISTINCT FROM ROW(%s)',
        (SELECT string_agg('OLD.' || quote_ident(c), ', ' ORDER BY n) FROM unnest(frozen) WITH ORDINALITY AS u(c, n)),
        (SELECT string_agg('NEW.' || quote_ident(c), ', ' ORDER BY n) FROM unnest(frozen) WITH ORDINALITY AS u(c, n)));
    IF 'closed_collection_id' = ANY (cols) THEN
        cond := cond || ' OR (OLD.closed_collection_id IS NOT NULL'
                     || ' AND NEW.closed_collection_id IS DISTINCT FROM OLD.closed_collection_id)';
    END IF;
    IF 'last_seen_collection_id' = ANY (cols) THEN
        cond := cond || ' OR (OLD.last_seen_collection_id IS NOT NULL'
                     || ' AND (NEW.last_seen_collection_id IS NULL'
                     || ' OR NEW.last_seen_collection_id < OLD.last_seen_collection_id))';
    END IF;
    cond := cond || format(' OR update_guard_columns_changed(%L::regclass, %L::text[])', tbl::text, cols);
    EXECUTE format('CREATE OR REPLACE TRIGGER %I BEFORE UPDATE ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION guard_update(%s)',
        (SELECT relname FROM pg_catalog.pg_class WHERE oid = tbl) || '_guard_update', tbl, cond,
        (SELECT string_agg(quote_literal(c), ', ' ORDER BY n) FROM unnest(allowed) WITH ORDINALITY AS u(c, n)));
END
$$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION install_update_guard(regclass, text[]) FROM PUBLIC;

-- The allow-lists of migration 0001, unchanged.
SELECT install_update_guard('relationship', ARRAY['src_entity_id', 'dst_entity_id', 'last_seen_collection_id',
    'closed_collection_id', 'valid_to_after', 'valid_to_before']);
SELECT install_update_guard('person_record', ARRAY['entity_id', 'identity_confidence', 'locked_by_decision_id']);
SELECT install_update_guard('company_version', ARRAY['last_seen_collection_id', 'closed_collection_id']);

-- +goose Down
CREATE OR REPLACE TRIGGER relationship_guard_update BEFORE UPDATE ON relationship FOR EACH ROW EXECUTE FUNCTION guard_update(
    'src_entity_id', 'dst_entity_id', 'last_seen_collection_id', 'closed_collection_id', 'valid_to_after', 'valid_to_before');
CREATE OR REPLACE TRIGGER person_record_guard_update BEFORE UPDATE ON person_record FOR EACH ROW EXECUTE FUNCTION guard_update(
    'entity_id', 'identity_confidence', 'locked_by_decision_id');
CREATE OR REPLACE TRIGGER company_version_guard_update BEFORE UPDATE ON company_version FOR EACH ROW EXECUTE FUNCTION guard_update(
    'last_seen_collection_id', 'closed_collection_id');
DROP FUNCTION install_update_guard(regclass, text[]);
DROP FUNCTION update_guard_columns_changed(regclass, text[]);
