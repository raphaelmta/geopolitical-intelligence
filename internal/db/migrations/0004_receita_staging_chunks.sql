-- +goose Up

-- The Receita relationship phase runs in short per-bucket transactions, each
-- holding the identity lock only while it reads the records' current entities
-- and writes its relationships. receita_stg_partner holds every Socios row
-- with all ids that identity resolution never moves (companies, foreign
-- entities, person record ids) resolved once, outside the lock, sorted by
-- bucket; BRIN on that order makes each bucket a short range scan.
-- bucket is a range of cnpj_basico (originally hashtext(cnpj_basico) & 1023;
-- since the load optimisation, task 2, one of 1,024 ranges of about equal
-- numbers of Socios rows, recomputed from staging at every load: see
-- connector.RangeChunks). Either way every claim of one company falls in the
-- bucket of that company, so deduplicating per bucket equals deduplicating
-- over the whole load.
CREATE UNLOGGED TABLE receita_stg_partner (
    bucket            int      NOT NULL,
    company_id        bigint   NOT NULL,
    partner_entity_id bigint, -- company or foreign partner
    partner_record_id bigint, -- person partner
    rep_record_id     bigint,
    qual              text     NOT NULL,
    rep_qual          text     NOT NULL,
    valid_from        date,
    file_no           smallint NOT NULL,
    line              bigint   NOT NULL
);
CREATE INDEX receita_stg_partner_bucket ON receita_stg_partner USING brin (bucket);

-- Replaced by receita_stg_partner (record ids) plus a per-bucket read of
-- person_record.entity_id under the identity lock.
DROP TABLE receita_stg_person_map;

-- +goose Down
DROP TABLE receita_stg_partner;
CREATE UNLOGGED TABLE receita_stg_person_map (
    context_key  text   NOT NULL,
    name_norm    text   NOT NULL,
    cpf_fragment text   NOT NULL,
    record_id    bigint NOT NULL,
    entity_id    bigint NOT NULL
);
