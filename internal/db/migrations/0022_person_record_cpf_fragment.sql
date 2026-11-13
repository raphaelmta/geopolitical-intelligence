-- +goose Up

-- Search by CPF (internal/search, Find): a query carrying a CPF lists the
-- persons having a record with its 6 middle digits. The lookup starts from
-- the fragment, so it needs an index that leads with cpf_fragment; the
-- existing (name_norm, cpf_fragment) one cannot serve it. entity_id rides
-- along so the candidate entities come from the index alone. Records without
-- a fragment ('') are never searched and are left out.
--
-- Plain CREATE INDEX (not CONCURRENTLY): person_record is written only by the
-- worker, which `make migrate` and `make up` stop before migrating. On the
-- national base (~29 M records) it reads the table once and sorts it: minutes,
-- not hours. IF NOT EXISTS: an operator may have built it by hand beforehand.
--
-- Numbering: never below the highest applied migration (0020 stays unused).
CREATE INDEX IF NOT EXISTS person_record_cpf_fragment_entity
    ON person_record (cpf_fragment, entity_id) WHERE cpf_fragment <> '';

-- +goose Down
DROP INDEX IF EXISTS person_record_cpf_fragment_entity;
