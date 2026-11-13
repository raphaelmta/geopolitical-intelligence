-- +goose Up

-- Scratch tables of the Receita (CNPJ) bulk load. They hold no claims: each
-- Load truncates them, fills them with COPY and set-based SQL, and writes the
-- claims from them. UNLOGGED (not crash-safe, no WAL) and without indexes,
-- triggers or foreign keys, so the national load stays fast; a crash only
-- means the next Load starts over.

-- One row per CSV file read in this Load; record_ref is name || ':' || line.
CREATE UNLOGGED TABLE receita_stg_file (
    file_no smallint PRIMARY KEY,
    name    text NOT NULL
);

-- Parsed Empresas rows (duplicates of cnpj_basico included).
CREATE UNLOGGED TABLE receita_stg_empresa (
    cnpj_basico  text     NOT NULL,
    name         text     NOT NULL,
    name_norm    text     NOT NULL,
    legal_nature text     NOT NULL,
    size         text     NOT NULL,
    capital      text,
    file_no      smallint NOT NULL,
    line         bigint   NOT NULL
);

-- Parsed Estabelecimentos rows of head offices (matriz) only.
CREATE UNLOGGED TABLE receita_stg_estab (
    cnpj_basico text     NOT NULL,
    status      text     NOT NULL,
    uf          text     NOT NULL,
    city        text     NOT NULL,
    cnae        text     NOT NULL,
    file_no     smallint NOT NULL,
    line        bigint   NOT NULL
);

-- Parsed Socios rows. partner_type: 1 company, 2 natural person, 3 foreign.
-- partner_base is set for companies, cpf_fragment for persons, foreign_key
-- for foreign partners. has_rep: a legal representative is named.
CREATE UNLOGGED TABLE receita_stg_socio (
    cnpj_basico   text     NOT NULL,
    partner_type  text     NOT NULL,
    name          text     NOT NULL,
    name_norm     text     NOT NULL,
    partner_base  text,
    cpf_fragment  text     NOT NULL,
    foreign_key   text,
    qual          text     NOT NULL,
    valid_from    date,
    has_rep       boolean  NOT NULL,
    rep_name      text     NOT NULL,
    rep_name_norm text     NOT NULL,
    rep_fragment  text     NOT NULL,
    rep_qual      text     NOT NULL,
    file_no       smallint NOT NULL,
    line          bigint   NOT NULL
);

-- Company bases selected by a slice (Scope with CNPJs and Depth).
CREATE UNLOGGED TABLE receita_stg_scope (
    cnpj_basico text PRIMARY KEY
);

-- Every company the Load touches, deduplicated, with its registry data.
-- has_version is false for companies known only from Socios (orphans).
CREATE UNLOGGED TABLE receita_stg_company_src (
    cnpj_basico  text     NOT NULL,
    name         text     NOT NULL,
    name_norm    text     NOT NULL,
    has_version  boolean  NOT NULL,
    legal_nature text     NOT NULL,
    size         text     NOT NULL,
    uf           text     NOT NULL,
    city         text     NOT NULL,
    cnae         text     NOT NULL,
    status       text     NOT NULL,
    capital      numeric(18, 2),
    file_no      smallint,
    line         bigint
);

-- receita_stg_company_src with the id of each company entity.
CREATE UNLOGGED TABLE receita_stg_company (
    cnpj_basico  text     NOT NULL,
    entity_id    bigint   NOT NULL,
    has_version  boolean  NOT NULL,
    name         text     NOT NULL,
    legal_nature text     NOT NULL,
    size         text     NOT NULL,
    uf           text     NOT NULL,
    city         text     NOT NULL,
    cnae         text     NOT NULL,
    status       text     NOT NULL,
    capital      numeric(18, 2),
    file_no      smallint,
    line         bigint
);

-- One row per person record key (partners and representatives).
CREATE UNLOGGED TABLE receita_stg_person (
    context_key  text     NOT NULL,
    name         text     NOT NULL,
    name_norm    text     NOT NULL,
    cpf_fragment text     NOT NULL,
    file_no      smallint NOT NULL,
    line         bigint   NOT NULL
);

-- Person records new in this Load, with the id of the entity created for each.
CREATE UNLOGGED TABLE receita_stg_person_new (
    entity_id    bigint   NOT NULL,
    context_key  text     NOT NULL,
    name         text     NOT NULL,
    name_norm    text     NOT NULL,
    cpf_fragment text     NOT NULL,
    file_no      smallint NOT NULL,
    line         bigint   NOT NULL
);

-- Person record keys with the record id and the record's current entity.
CREATE UNLOGGED TABLE receita_stg_person_map (
    context_key  text   NOT NULL,
    name_norm    text   NOT NULL,
    cpf_fragment text   NOT NULL,
    record_id    bigint NOT NULL,
    entity_id    bigint NOT NULL
);

-- Relationships asserted by this Load, one per natural key.
CREATE UNLOGGED TABLE receita_stg_rel (
    rel_type      text     NOT NULL,
    src_entity_id bigint   NOT NULL,
    dst_entity_id bigint   NOT NULL,
    src_record_id bigint,
    dst_record_id bigint,
    role          text     NOT NULL,
    valid_from    date,
    file_no       smallint NOT NULL,
    line          bigint   NOT NULL
);

-- +goose Down
DROP TABLE receita_stg_rel, receita_stg_person_map, receita_stg_person_new, receita_stg_person,
    receita_stg_company, receita_stg_company_src, receita_stg_scope, receita_stg_socio,
    receita_stg_estab, receita_stg_empresa, receita_stg_file;
