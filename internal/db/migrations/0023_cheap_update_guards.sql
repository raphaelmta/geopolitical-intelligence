-- +goose Up

-- Cheap update guards for the three big claim tables (relationship,
-- person_record, company_version), and search_path pinned on every trigger
-- function of the schema.
--
-- 1. The guard. The rule does not change: the guard of migration 0001
-- (guard_update, default-deny with an allow-list) stays the one and only
-- judge of every update it sees. What changes is that it no longer sees the
-- updates it would certainly let through: each trigger gets a WHEN clause,
-- evaluated by the executor without calling any function per row, that is
-- true whenever guard_update could refuse the row:
--
--   (a) a column outside the allow-list changes — ROW(OLD.c, …) IS DISTINCT
--       FROM ROW(NEW.c, …) over every such column, listed from the catalogue
--       when the trigger is installed (install_update_guard);
--   (b) a closed row's closed_collection_id changes (reopening);
--   (c) a non-NULL last_seen_collection_id becomes NULL or smaller;
--   (d) second layer only: the table's columns are no longer the ones (a)
--       lists (update_guard_columns_changed, below).
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
-- 2. Columns added later: default-deny at run time, at every isolation
-- level. The event trigger geoint_update_guard_reinstall (ddl_command_end,
-- ALTER TABLE) re-installs <table>_guard_update, with its current
-- allow-list, in the same transaction as any ALTER TABLE of a guarded
-- table. A new column is therefore compared by (a) from the moment it
-- exists: every statement that can see the column also sees the new WHEN
-- clause (both come from the relation cache, not from the transaction's
-- snapshot), so it is immutable until a migration puts it on the
-- allow-list. An ALTER TABLE that would leave a guarded table without its
-- guard (DROP COLUMN … CASCADE of a column the WHEN clause compares) is
-- refused. TestUpdateGuardCatalogue fails until the column's mutability is
-- decided in the test's lists (and, if it may change, by a migration that
-- calls install_update_guard with the column on the allow-list).
--   (d) only matters when the event trigger does not run (single-user mode,
--   event_triggers = off, or the re-install switch below): every row of the
--   table then goes through guard_update again. It reads pg_attribute
--   through the statement's snapshot, so a REPEATABLE READ transaction whose
--   snapshot predates the ALTER does not see the new column there: it is a
--   best-effort fallback, not the guarantee. It is a plpgsql function
--   declared IMMUTABLE on purpose although it reads the catalogue: with
--   constant arguments, the planner evaluates it once when a statement
--   starts (trigger WHEN clauses are prepared for each execution of a
--   statement and never kept in a cached plan), not once per row.
--
--   The event trigger is database-wide, so it lives in a schema of its own
--   (geoint_guard, owner only) and serves every schema migrated by this
--   file (one per test). CREATE EVENT TRIGGER needs a superuser: the owner
--   is one (0021). The function skips its own nested ALTER TABLE (restoring
--   the trigger's enabled state) through the transaction-local setting
--   geoint.update_guard_installing; only the tables' owner can ALTER them,
--   so the setting gives no one else anything.
--   The event trigger runs as whoever issues the ALTER — for these tables
--   the owner, a superuser — and calls <schema>.install_update_guard in a
--   schema where the worker may CREATE. It calls it only when that function
--   is owned by the table's owner and by the event trigger's owner; a
--   function of that name and signature owned by anyone else (planted, for
--   instance after `goose down` of this file) makes the ALTER fail, naming
--   it, and is never executed. No legitimate state has such a function: only
--   this file creates it, as the owner. (Changing the owner of a guarded
--   table with ALTER TABLE … OWNER TO is refused for the same reason;
--   REASSIGN OWNED moves tables and functions together.)
--   Dropping or renaming a column: a column the WHEN clause compares cannot
--   be dropped (the trigger depends on it), and DROP … CASCADE is refused
--   above. A migration that drops one runs, in one transaction: SET LOCAL
--   geoint.update_guard_installing = 'on'; ALTER TABLE … DROP COLUMN …
--   CASCADE; SELECT install_update_guard('<table>'::regclass, ARRAY[…]::text[]).
--   Dropping or renaming a column on the allow-list makes every later ALTER
--   TABLE of the table fail with "install_update_guard(<table>): the
--   allow-list {…} must name columns of the table" (the event trigger
--   re-installs with the old allow-list, read from the trigger's arguments):
--   do the same three steps with the new allow-list.
--   Known limits: a column added later is compared with its type's equality
--   (a type whose = is looser than its text — interval, a nondeterministic
--   collation — or that has no = needs a decision, which the catalogue test
--   forces); an ADD COLUMN that reaches a guarded table by recursion from a
--   parent (after an owner-made INHERIT or ATTACH PARTITION) is reported for
--   the parent and does not re-install the guard (the catalogue test fails).
--
-- 3. search_path. The worker may CREATE in this schema (0021). A trigger
-- function that resolves a name through the caller's search_path could be
-- given the worker's function instead of the built-in one (for instance an
-- exact-type to_jsonb(relationship) beats pg_catalog.to_jsonb(anyelement)),
-- switching the rule off. Every trigger function of the schema, and the
-- functions here, run with search_path = pg_catalog, pg_temp; nothing they
-- use lives in the application schema except what they name with it.
-- TestTriggerFunctionsPinSearchPath fails on a trigger function, event
-- trigger function or SECURITY DEFINER function without it. The statements
-- of this file run as the owner under the schema's search_path, so every
-- call here names its argument types exactly (casts) or its schema
-- (pg_catalog.…): an exact-type function the worker created beforehand
-- would otherwise be preferred to the intended one.
--
-- Safe on the national base (≈ 70 M company_version, 50 M relationship
-- rows): CREATE OR REPLACE TRIGGER rewrites one pg_trigger row, ALTER
-- FUNCTION one pg_proc row; nothing scans or rewrites a table. It takes a
-- SHARE ROW EXCLUSIVE lock on each of the three tables (reads go on, writes
-- wait) for milliseconds; the old definition is replaced in place under the
-- same name, inside this migration's single transaction, so no moment
-- exists without a guard. `make migrate` stops the api and the worker
-- first; lock_timeout makes a migration run beside a writer fail and be
-- retried rather than queue every other writer behind it.
SET LOCAL lock_timeout = '5s';

-- Trigger functions of earlier migrations: no body changes.
ALTER FUNCTION forbid_delete() SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION forbid_update() SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION guard_update() SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION guard_proposal_update() SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION guard_collection_status() SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION guard_entity_update() SET search_path = pg_catalog, pg_temp;

-- +goose StatementBegin
CREATE FUNCTION update_guard_columns_changed(tbl regclass, cols text[]) RETURNS boolean
    LANGUAGE plpgsql IMMUTABLE SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    RETURN ARRAY(SELECT a.attname::text FROM pg_catalog.pg_attribute a
                 WHERE a.attrelid = tbl AND a.attnum > 0 AND NOT a.attisdropped
                 ORDER BY a.attnum) IS DISTINCT FROM cols;
END
$$;
-- +goose StatementEnd
COMMENT ON FUNCTION update_guard_columns_changed(regclass, text[]) IS
    'Reads the catalogue: declared IMMUTABLE only so that the WHEN clause of an update guard (0023) folds it once per statement. Never use it in an index, a generated column, a view, a default or a constraint.';
-- Called with the privileges of whoever updates the table (the WHEN clause
-- is prepared as that role): granted explicitly rather than through PUBLIC,
-- so that revoking PUBLIC's EXECUTE from the schema's functions later does
-- not make every update of the three tables fail.
REVOKE EXECUTE ON FUNCTION update_guard_columns_changed(regclass, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION update_guard_columns_changed(regclass, text[]) TO geoint_worker, geoint_api;

-- Installs (or re-installs) <table>_guard_update: guard_update(allowed…)
-- behind the WHEN clause described above, generated from the table's
-- current columns, keeping the trigger's enabled state (CREATE OR REPLACE
-- TRIGGER alone would reset it to ENABLE). A later migration that puts a new
-- column on the allow-list calls it again.
-- +goose StatementBegin
CREATE FUNCTION install_update_guard(tbl regclass, allowed text[]) RETURNS void
    LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
    nsp    text := (SELECT n.nspname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
                    WHERE c.oid = tbl);
    trg    text := (SELECT c.relname FROM pg_catalog.pg_class c WHERE c.oid = tbl) || '_guard_update';
    cols   text[] := ARRAY(SELECT a.attname::text FROM pg_catalog.pg_attribute a
                           WHERE a.attrelid = tbl AND a.attnum > 0 AND NOT a.attisdropped
                           ORDER BY a.attnum);
    state  "char" := (SELECT t.tgenabled FROM pg_catalog.pg_trigger t WHERE t.tgrelid = tbl AND t.tgname = trg);
    was    text := current_setting('geoint.update_guard_installing', true);
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
    cond := cond || format(' OR %I.update_guard_columns_changed(%L::regclass, %L::text[])', nsp, tbl::text, cols);
    PERFORM set_config('geoint.update_guard_installing', 'on', true);
    EXECUTE format('CREATE OR REPLACE TRIGGER %I BEFORE UPDATE ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION %I.guard_update(%s)',
        trg, tbl, cond, nsp,
        (SELECT string_agg(quote_literal(c), ', ' ORDER BY n) FROM unnest(allowed) WITH ORDINALITY AS u(c, n)));
    IF state = 'A' THEN
        EXECUTE format('ALTER TABLE %s ENABLE ALWAYS TRIGGER %I', tbl, trg);
    ELSIF state = 'R' THEN
        EXECUTE format('ALTER TABLE %s ENABLE REPLICA TRIGGER %I', tbl, trg);
    ELSIF state = 'D' THEN
        EXECUTE format('ALTER TABLE %s DISABLE TRIGGER %I', tbl, trg);
    END IF;
    PERFORM set_config('geoint.update_guard_installing', coalesce(was, ''), true);
END
$$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION install_update_guard(regclass, text[]) FROM PUBLIC;

-- The event trigger: one for the database, created once (concurrent
-- migrations of several schemas wait for each other on the advisory lock).
-- +goose StatementBegin
DO $$
DECLARE
    path text := pg_catalog.current_setting('search_path'::text);
BEGIN
    -- Operators and functions of this block from pg_catalog only.
    PERFORM pg_catalog.set_config('search_path'::text, 'pg_catalog, pg_temp'::text, true);
    PERFORM pg_advisory_xact_lock(hashtext('geoint_update_guard_reinstall'::text)::bigint);
    CREATE SCHEMA IF NOT EXISTS geoint_guard;
    IF (SELECT n.nspowner FROM pg_namespace n WHERE n.nspname = 'geoint_guard')
       <> (SELECT r.oid FROM pg_roles r WHERE r.rolname = current_user) THEN
        RAISE EXCEPTION 'schema geoint_guard exists and belongs to %, not to the migrating owner %: drop it and migrate again',
            (SELECT n.nspowner::regrole FROM pg_namespace n WHERE n.nspname = 'geoint_guard'), current_user;
    END IF;
    REVOKE ALL ON SCHEMA geoint_guard FROM PUBLIC, geoint_worker, geoint_api;
    -- For every ALTER TABLE of relationship, person_record or
    -- company_version in a schema that has install_update_guard: re-install
    -- the guard with its current allow-list, or refuse the ALTER if it
    -- dropped the guard. Any other table: nothing. install_update_guard is
    -- called only when owned by the table's owner and by this event
    -- trigger's owner; one owned by anyone else is refused, never run.
    CREATE OR REPLACE FUNCTION geoint_guard.reinstall_update_guards() RETURNS event_trigger
        LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $f$
    DECLARE
        r    record;
        args text[];
        fn   regprocedure;
        fown oid;
    BEGIN
        IF current_setting('geoint.update_guard_installing', true) = 'on' THEN
            RETURN;
        END IF;
        FOR r IN SELECT DISTINCT c.oid, c.relname, c.relowner, n.nspname
                 FROM pg_catalog.pg_event_trigger_ddl_commands() d
                 JOIN pg_catalog.pg_class c ON c.oid = d.objid
                 JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
                 WHERE d.classid = 'pg_catalog.pg_class'::regclass
                   AND c.relname IN ('relationship', 'person_record', 'company_version')
        LOOP
            fn := pg_catalog.to_regprocedure(pg_catalog.format('%I.install_update_guard(regclass, text[])', r.nspname));
            CONTINUE WHEN fn IS NULL; -- a schema without this file's guards
            fown := (SELECT p.proowner FROM pg_catalog.pg_proc p WHERE p.oid = fn);
            -- IS DISTINCT FROM, never <>: a function dropped between the lookup above and
            -- this read gives a NULL owner, and a NULL comparison must refuse, not pass.
            IF fown IS NULL OR fown IS DISTINCT FROM r.relowner
               OR fown IS DISTINCT FROM (SELECT e.evtowner FROM pg_catalog.pg_event_trigger e
                                         WHERE e.evtname = 'geoint_update_guard_reinstall') THEN
                RAISE EXCEPTION 'ALTER TABLE %.% refused: % belongs to %, not to the owner of the table (%) and of the update guards: '
                    'it was not created by migration 0023 and is not run; drop it, then repeat the ALTER',
                    r.nspname, r.relname, fn, fown::regrole, r.relowner::regrole;
            END IF;
            SELECT array_remove(string_to_array(encode(t.tgargs, 'escape'), '\000'), '')
              INTO args
              FROM pg_catalog.pg_trigger t
              WHERE t.tgrelid = r.oid AND t.tgname = r.relname || '_guard_update';
            IF NOT FOUND THEN
                RAISE EXCEPTION 'ALTER TABLE %.% would leave the table without its update guard (%_guard_update): '
                    'never DROP … CASCADE a column the guard compares; re-install the guard instead',
                    r.nspname, r.relname, r.relname;
            END IF;
            EXECUTE pg_catalog.format('SELECT %I.install_update_guard($1::pg_catalog.regclass, $2::pg_catalog.text[])', r.nspname)
                USING r.oid::regclass, args;
        END LOOP;
    END
    $f$;
    IF NOT EXISTS (SELECT 1 FROM pg_event_trigger WHERE evtname = 'geoint_update_guard_reinstall') THEN
        CREATE EVENT TRIGGER geoint_update_guard_reinstall ON ddl_command_end
            WHEN TAG IN ('ALTER TABLE') EXECUTE FUNCTION geoint_guard.reinstall_update_guards();
    END IF;
    PERFORM set_config('search_path'::text, path, true);
END
$$;
-- +goose StatementEnd

-- The allow-lists of migration 0001, unchanged.
-- Exact argument types: an install_update_guard(text, …) planted beforehand
-- would otherwise win over this file's for a string literal.
SELECT install_update_guard('relationship'::pg_catalog.regclass, ARRAY['src_entity_id', 'dst_entity_id',
    'last_seen_collection_id', 'closed_collection_id', 'valid_to_after', 'valid_to_before']::pg_catalog.text[]);
SELECT install_update_guard('person_record'::pg_catalog.regclass,
    ARRAY['entity_id', 'identity_confidence', 'locked_by_decision_id']::pg_catalog.text[]);
SELECT install_update_guard('company_version'::pg_catalog.regclass,
    ARRAY['last_seen_collection_id', 'closed_collection_id']::pg_catalog.text[]);

-- +goose Down
-- install_update_guard goes first: from then on the event trigger skips
-- this schema, and a function of that name planted later by another role
-- makes the owner's ALTER TABLE fail instead of running. The event trigger
-- serves every schema of the database: it is dropped with its schema only
-- when no schema has install_update_guard any more. The pinned search_path
-- of the trigger functions stays: going back to the caller's search_path
-- would reopen the hole it closes (it changes no rule).
DROP FUNCTION install_update_guard(pg_catalog.regclass, pg_catalog.text[]);
CREATE OR REPLACE TRIGGER relationship_guard_update BEFORE UPDATE ON relationship FOR EACH ROW EXECUTE FUNCTION guard_update(
    'src_entity_id', 'dst_entity_id', 'last_seen_collection_id', 'closed_collection_id', 'valid_to_after', 'valid_to_before');
CREATE OR REPLACE TRIGGER person_record_guard_update BEFORE UPDATE ON person_record FOR EACH ROW EXECUTE FUNCTION guard_update(
    'entity_id', 'identity_confidence', 'locked_by_decision_id');
CREATE OR REPLACE TRIGGER company_version_guard_update BEFORE UPDATE ON company_version FOR EACH ROW EXECUTE FUNCTION guard_update(
    'last_seen_collection_id', 'closed_collection_id');
DROP FUNCTION update_guard_columns_changed(pg_catalog.regclass, pg_catalog.text[]);
-- +goose StatementBegin
DO $$
DECLARE
    path text := pg_catalog.current_setting('search_path'::text);
BEGIN
    PERFORM pg_catalog.set_config('search_path'::text, 'pg_catalog, pg_temp'::text, true);
    PERFORM pg_advisory_xact_lock(hashtext('geoint_update_guard_reinstall'::text)::bigint);
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_proc p
                   WHERE p.proname = 'install_update_guard'
                     AND p.proargtypes = '2205 1009'::pg_catalog.oidvector) THEN  -- (regclass, text[])
        DROP EVENT TRIGGER IF EXISTS geoint_update_guard_reinstall;
        DROP FUNCTION IF EXISTS geoint_guard.reinstall_update_guards();
        DROP SCHEMA IF EXISTS geoint_guard;
    END IF;
    PERFORM set_config('search_path'::text, path, true);
END
$$;
-- +goose StatementEnd
