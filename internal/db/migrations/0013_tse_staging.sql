-- +goose Up

-- Scratch tables of the TSE (candidacies and campaign-finance receipts) bulk
-- load. Like the Receita ones (0003, 0004) they hold no claims: each Load
-- truncates them, fills them with COPY and set-based SQL, writes the claims
-- and truncates them again (also when it fails). UNLOGGED, no triggers or
-- foreign keys. They never hold a full CPF: only the 6-digit fragment the
-- parser derives (docs/sources/tse.md, "Privacidade").

-- One row per CSV entry read in this Load; record_ref is name || ':' || line.
CREATE UNLOGGED TABLE tse_stg_file (
    file_no smallint PRIMARY KEY,
    name    text NOT NULL
);

-- Parsed consulta_cand rows: one per candidacy and round (NR_TURNO).
CREATE UNLOGGED TABLE tse_stg_cand (
    year            smallint NOT NULL,
    sq              text     NOT NULL,
    turno           smallint NOT NULL,
    election_date   date,
    office_code     text     NOT NULL,
    office          text     NOT NULL,
    uf              text     NOT NULL,
    city            text     NOT NULL,
    city_name       text     NOT NULL,
    name            text     NOT NULL,
    name_norm       text     NOT NULL,
    ballot_name     text     NOT NULL,
    cpf_fragment    text     NOT NULL,
    party           text     NOT NULL,
    party_name      text     NOT NULL,
    party_name_norm text     NOT NULL,
    result          text     NOT NULL,
    file_no         smallint NOT NULL,
    line            bigint   NOT NULL
);

-- Parsed receitas rows that are loaded (other origins are only counted).
-- donor_kind: person, company, party or candidate. donor_key identifies the
-- donor within its kind (person: name_norm || '|' || fragment; company: CNPJ
-- base; party: sigla; candidate: SQ_CANDIDATO_DOADOR). amount is the decimal
-- text with a dot.
CREATE UNLOGGED TABLE tse_stg_receipt (
    year                  smallint NOT NULL,
    receipt_id            text     NOT NULL,
    sq                    text     NOT NULL,
    origin_code           text     NOT NULL,
    origin                text     NOT NULL,
    donor_kind            text     NOT NULL,
    donor_key             text     NOT NULL,
    donor_name            text     NOT NULL,
    donor_name_norm       text     NOT NULL,
    donor_fragment        text     NOT NULL,
    donor_base            text     NOT NULL,
    donor_party           text     NOT NULL,
    donor_party_name      text     NOT NULL,
    donor_party_name_norm text     NOT NULL,
    donor_sq              text     NOT NULL,
    amount                text     NOT NULL,
    receipt_date          date,
    file_no               smallint NOT NULL,
    line                  bigint   NOT NULL
);

-- One row per candidacy (year, sq): the record key and the claim data.
CREATE UNLOGGED TABLE tse_stg_candidate (
    year         smallint NOT NULL,
    sq           text     NOT NULL,
    context_key  text     NOT NULL,
    name         text     NOT NULL,
    name_norm    text     NOT NULL,
    cpf_fragment text     NOT NULL,
    ballot_name  text     NOT NULL,
    office_code  text     NOT NULL,
    office       text     NOT NULL,
    uf           text     NOT NULL,
    city         text     NOT NULL,
    city_name    text     NOT NULL,
    party        text     NOT NULL,
    result       text     NOT NULL,
    valid_from   date,
    file_no      smallint NOT NULL,
    line         bigint   NOT NULL
);

-- One row per person record key (candidates and individual donors).
CREATE UNLOGGED TABLE tse_stg_person (
    context_key  text     NOT NULL,
    name         text     NOT NULL,
    name_norm    text     NOT NULL,
    cpf_fragment text     NOT NULL,
    file_no      smallint NOT NULL,
    line         bigint   NOT NULL
);

-- Person records new in this Load, with the id of the entity created for each.
CREATE UNLOGGED TABLE tse_stg_person_new (
    entity_id    bigint   NOT NULL,
    context_key  text     NOT NULL,
    name         text     NOT NULL,
    name_norm    text     NOT NULL,
    cpf_fragment text     NOT NULL,
    file_no      smallint NOT NULL,
    line         bigint   NOT NULL
);

-- One row per receipt (SQ_RECEITA) and donor: the items of a receipt summed.
CREATE UNLOGGED TABLE tse_stg_donation (
    year            smallint NOT NULL,
    receipt_id      text     NOT NULL,
    sq              text     NOT NULL,
    donor_kind      text     NOT NULL,
    donor_key       text     NOT NULL,
    donor_name_norm text     NOT NULL,
    donor_fragment  text     NOT NULL,
    donor_base      text     NOT NULL,
    donor_party     text     NOT NULL,
    donor_sq        text     NOT NULL,
    amount          numeric  NOT NULL,
    receipt_date    date,
    origin          text     NOT NULL,
    origin_code     text     NOT NULL,
    items           int      NOT NULL,
    file_no         smallint NOT NULL,
    line            bigint   NOT NULL
);

-- Every claim of the Load with the ids identity resolution never moves
-- (person record ids, party and company entities), sorted by bucket
-- (originally hashtext of the candidate's context_key; since the load
-- optimisation, task 2, a range of context keys: see connector.RangeChunks;
-- either way all claims of a candidacy share a bucket). Each bucket is written in its own short transaction
-- under the identity lock.
CREATE UNLOGGED TABLE tse_stg_claim (
    bucket        int      NOT NULL,
    rel_type      text     NOT NULL,
    src_entity_id bigint,
    src_record_id bigint,
    dst_entity_id bigint,
    dst_record_id bigint,
    role          text     NOT NULL,
    valid_from    date,
    attrs         jsonb    NOT NULL,
    file_no       smallint NOT NULL,
    line          bigint   NOT NULL
);
CREATE INDEX tse_stg_claim_bucket ON tse_stg_claim USING brin (bucket);

-- The relationships of one bucket, one per natural key, with the records'
-- current entities.
CREATE UNLOGGED TABLE tse_stg_rel (
    rel_type      text     NOT NULL,
    src_entity_id bigint   NOT NULL,
    dst_entity_id bigint   NOT NULL,
    src_record_id bigint,
    dst_record_id bigint,
    role          text     NOT NULL,
    valid_from    date,
    attrs         jsonb    NOT NULL,
    file_no       smallint NOT NULL,
    line          bigint   NOT NULL
);

-- +goose Down
DROP TABLE tse_stg_rel, tse_stg_claim, tse_stg_donation, tse_stg_person_new, tse_stg_person,
    tse_stg_candidate, tse_stg_receipt, tse_stg_cand, tse_stg_file;
