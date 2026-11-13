-- +goose Up

-- Least-privilege database roles, so that the application cannot switch off
-- the integrity rules (the no-delete/no-truncate triggers and the update
-- guards): only the owner of a table may ALTER it (DISABLE TRIGGER), and the
-- owner is used by nothing but `geoint migrate`.
--
--   owner          the role that runs the migrations (POSTGRES_USER, created
--                  by the postgres image; superuser). Owns every claim,
--                  history and catalogue table. Used ONLY by `geoint migrate`.
--   geoint_worker  the worker (loaders, identity resolution, search rebuild,
--                  scheduler, graph jobs). No superuser/CREATEDB/CREATEROLE.
--                  Owns only operational tables it must TRUNCATE or replace:
--                  the loader staging tables and the search tables (the
--                  rebuild creates, renames and drops them).
--   geoint_api     the API and `geoint collect`. Reads; appends the audit
--                  trail; writes only what analyst decisions, job and
--                  collection requests write.
--
-- Roles are cluster-wide: created once (any schema's migration may be the
-- first); every grant below is relative to the schema being migrated
-- (current_schema()), so the test helper's schema-per-test works too. The
-- roles start NOLOGIN; `geoint migrate` gives them LOGIN and their passwords
-- (DB_WORKER_PASSWORD, DB_API_PASSWORD) at every run (internal/db/roles.go).
--
-- Privilege matrix (S select, I insert, U update, M maintain = ANALYZE/VACUUM;
-- D delete; U(cols) = only those columns; own = owner; - = nothing). Neither
-- role has TRUNCATE, TRIGGER or REFERENCES on any table it does not own, nor
-- DELETE on any table but job.
-- internal/db/roles_test.go holds the same matrix and fails when a table is
-- missing from it or a privilege differs.
--
--   table                   geoint_worker   geoint_api                         why (api)
--   source                  S               S
--   collection              S I U           S
--   collection_file         S I             S
--   collection_request      S I U(published_at)  S I U(published_at)          freshness.Request
--   source_check            S I U           S
--   entity                  S I U M         S I                                identity.newPersonEntity
--   person_record           S I U M         S U(entity_id, identity_confidence, locked_by_decision_id)  identity decisions
--   relationship            S I U M         S U(src_entity_id, dst_entity_id)  identity.repoint
--   company_version         S I U M         S
--   identity_decision       S I             S I                                identity decisions
--   identity_proposal       S I U           S U(status, decided_by, decided_at, decision_id)  Decide
--   identity_ambiguous_key  S               S I                                Split
--   identity_resolve_done   S I             S
--   job                     S U D           S I U(status, error, finished_at, updated_at)  jobs.Enqueue
--                           (D: jobs.Purge deletes finished jobs after their retention; job is
--                           operational, not a claim, and has no integrity trigger)
--   audit_log               -               I                                  audit.Log
--   search_state            S U             S
--   search_company, search_person, search_other, search_frequent_token
--                           own             S (also on the tables each rebuild creates)
--   receita_stg_*, tse_stg_* (loader staging)
--                           own             -
--   goose_db_version        -               -
--
-- Sequences: USAGE for a role that may INSERT into the table owning them.
-- Schema: USAGE for both; CREATE for the worker (the search rebuild creates
-- tables). Default privileges: a table a later migration creates gets
-- SELECT for both roles and INSERT/UPDATE for the worker (never DELETE or
-- TRUNCATE) until its line is added here and in roles_test.go, which fails
-- until it is.

-- +goose StatementBegin
DO $$
DECLARE
    s text := quote_ident(current_schema());
    ns oid := (SELECT oid FROM pg_namespace WHERE nspname = current_schema());
    r record;
BEGIN
    BEGIN
        CREATE ROLE geoint_worker NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
    END;
    BEGIN
        CREATE ROLE geoint_api NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
    END;

    EXECUTE format('GRANT USAGE ON SCHEMA %s TO geoint_worker, geoint_api', s);
    EXECUTE format('GRANT CREATE ON SCHEMA %s TO geoint_worker', s);

    FOR r IN SELECT * FROM (VALUES
        ('source',                 'SELECT',                                   'SELECT'),
        ('collection',             'SELECT, INSERT, UPDATE',                   'SELECT'),
        ('collection_file',        'SELECT, INSERT',                           'SELECT'),
        ('collection_request',     'SELECT, INSERT, UPDATE (published_at)',    'SELECT, INSERT, UPDATE (published_at)'),
        ('source_check',           'SELECT, INSERT, UPDATE',                   'SELECT'),
        ('entity',                 'SELECT, INSERT, UPDATE, MAINTAIN',         'SELECT, INSERT'),
        ('person_record',          'SELECT, INSERT, UPDATE, MAINTAIN',
                                   'SELECT, UPDATE (entity_id, identity_confidence, locked_by_decision_id)'),
        ('relationship',           'SELECT, INSERT, UPDATE, MAINTAIN',         'SELECT, UPDATE (src_entity_id, dst_entity_id)'),
        ('company_version',        'SELECT, INSERT, UPDATE, MAINTAIN',         'SELECT'),
        ('identity_decision',      'SELECT, INSERT',                           'SELECT, INSERT'),
        ('identity_proposal',      'SELECT, INSERT, UPDATE',
                                   'SELECT, UPDATE (status, decided_by, decided_at, decision_id)'),
        ('identity_ambiguous_key', 'SELECT',                                   'SELECT, INSERT'),
        ('identity_resolve_done',  'SELECT, INSERT',                           'SELECT'),
        ('job',                    'SELECT, UPDATE, DELETE',                         'SELECT, INSERT, UPDATE (status, error, finished_at, updated_at)'),
        ('audit_log',              '',                                         'INSERT'),
        ('search_state',           'SELECT, UPDATE',                           'SELECT')
    ) AS m(tbl, worker, api) LOOP
        IF r.worker <> '' THEN
            EXECUTE format('GRANT %s ON TABLE %I TO geoint_worker', r.worker, r.tbl);
        END IF;
        IF r.api <> '' THEN
            EXECUTE format('GRANT %s ON TABLE %I TO geoint_api', r.api, r.tbl);
        END IF;
    END LOOP;

    -- Operational tables the worker replaces or truncates: it owns them
    -- (with their indexes and sequences). The search tables are readable by
    -- the API; staging is not. Leftovers of an interrupted rebuild
    -- (search_ranked, search_*_new) are given to the worker too, so its next
    -- rebuild can drop them.
    FOR r IN SELECT c.relname FROM pg_class c
        WHERE c.relnamespace = ns AND c.relkind IN ('r', 'p')
          AND (c.relname LIKE 'receita\_stg\_%' OR c.relname LIKE 'tse\_stg\_%' OR c.relname LIKE 'search\_%')
          AND c.relname <> 'search_state'
    LOOP
        EXECUTE format('ALTER TABLE %I OWNER TO geoint_worker', r.relname);
        IF r.relname LIKE 'search\_%' THEN
            EXECUTE format('GRANT SELECT ON TABLE %I TO geoint_api', r.relname);
        END IF;
    END LOOP;

    -- Sequences of tables a role may insert into (bigserial ids).
    FOR r IN SELECT seq.relname AS seq, tbl.oid AS tbl FROM pg_class seq
        JOIN pg_depend d ON d.objid = seq.oid AND d.classid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')
        JOIN pg_class tbl ON tbl.oid = d.refobjid
        WHERE seq.relkind = 'S' AND seq.relnamespace = ns
    LOOP
        IF has_table_privilege('geoint_worker', r.tbl, 'INSERT') THEN
            EXECUTE format('GRANT USAGE ON SEQUENCE %I TO geoint_worker', r.seq);
        END IF;
        IF has_table_privilege('geoint_api', r.tbl, 'INSERT') THEN
            EXECUTE format('GRANT USAGE ON SEQUENCE %I TO geoint_api', r.seq);
        END IF;
    END LOOP;

    -- Tables of later migrations (created by the owner, the current role):
    -- never silently inaccessible, never deletable by the application.
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT, INSERT, UPDATE ON TABLES TO geoint_worker', s);
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT ON TABLES TO geoint_api', s);
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT USAGE ON SEQUENCES TO geoint_worker', s);
    -- Tables each search rebuild creates (as the worker) stay readable by the API.
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE geoint_worker IN SCHEMA %s GRANT SELECT ON TABLES TO geoint_api', s);
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Intentionally empty: revoking would leave a running worker and API without
-- access; roles are cluster-wide and may serve other schemas.
