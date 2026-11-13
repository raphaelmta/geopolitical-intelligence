-- +goose Up

-- Search by CPF (internal/search, Find): a query carrying a CPF lists the
-- persons having a record with its 6 middle digits. The lookup starts from
-- the fragment (or, for a fragment above the cap, checks a name-search
-- candidate's records), so it needs an index that leads with cpf_fragment;
-- the existing (name_norm, cpf_fragment) one cannot serve it. entity_id rides
-- along so both lookups come from the index alone. Records without a
-- fragment ('') are never searched and are left out.
--
-- Plain CREATE INDEX (not CONCURRENTLY, which goose's transaction would not
-- allow): it holds a SHARE lock on person_record while it builds, so any
-- write to person_record waits until it ends — the worker's loaders and
-- identity resolution and the API's identity decisions (merge, split,
-- proposals). `make migrate` and `make up` stop both api and worker first,
-- so nothing waits in practice. On the national base (~29 M records) it reads
-- the table once and sorts it: minutes, not hours (estimate; not measured).
--
-- IF NOT EXISTS: an operator may have built it by hand beforehand. A failed
-- CREATE INDEX CONCURRENTLY leaves an INVALID index with this name, which IF
-- NOT EXISTS would silently keep: the check below refuses it (DROP INDEX
-- person_record_cpf_fragment_entity, then migrate again).
--
-- Numbering: never below the highest applied migration (0020 stays unused).
CREATE INDEX IF NOT EXISTS person_record_cpf_fragment_entity
    ON person_record (cpf_fragment, entity_id) WHERE cpf_fragment <> '';

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
               WHERE c.relname = 'person_record_cpf_fragment_entity'
                 AND c.relnamespace = current_schema()::regnamespace
                 AND NOT (i.indisvalid AND i.indisready)) THEN
        RAISE EXCEPTION 'index person_record_cpf_fragment_entity exists but is invalid (a failed CREATE INDEX CONCURRENTLY?): DROP INDEX person_record_cpf_fragment_entity and migrate again';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS person_record_cpf_fragment_entity;
