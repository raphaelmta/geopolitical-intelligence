-- +goose Up

-- Name search (internal/search), as measured in
-- docs/spikes/2026-11-receita-path-queries.md, block SEARCH_STRATEGY.
-- The search tables are derived data: Rebuild recreates them from entity and
-- relationship after every collection and swaps them in by renaming. The
-- empty tables created here make queries answer (with nothing) before the
-- first rebuild.

-- unaccent lives in public, whatever the search_path, so that the functions
-- below can name it and dropping another schema never drops it.
CREATE EXTENSION IF NOT EXISTS unaccent SCHEMA public;

-- Index side and query side use this same expression (SEARCH_STRATEGY 2).
-- Deliberately not identity.NormalizeName: that keeps only [A-Z0-9 ] and
-- turns punctuation into spaces, while search_norm keeps punctuation, so an
-- exact-name match compares names as written (accents and case folded).
-- +goose StatementBegin
CREATE FUNCTION search_norm(t text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE
RETURN nullif(btrim(regexp_replace(upper(public.unaccent(
         'public.unaccent'::regdictionary, coalesce(t, ''))),
         '\s+', ' ', 'g')), '');
-- +goose StatementEnd

-- The word rule, defined once and used by both the index (search_tokens) and
-- the query (internal/search reads search_words): the words of a name in
-- order, each with its position and whether it is searched. Words are runs of
-- [a-z0-9] (lower() under the "C" collation, so the result never depends on
-- the caller's collation). A word is NOT searched when
--   - it is one of 15 stopwords (SEARCH_STRATEGY 3);
--   - (a) it contains 5 or more consecutive digits;
--   - (n) it contains a digit and the whole name has 9 or more ASCII digits in
--     total, wherever they are and however they are separated or glued to
--     letters.
-- Guarantee: a name with 9 or more digits in total (a CPF has 11, a CNPJ 14,
-- in any spelling) is not searchable by any of its words that contain
-- digits, so no query, with right or wrong digits, can match it through
-- them; it is found only by its digit-free words. Cost: digits in such names
-- (and long numbers anywhere) are not searchable. Short numbers in other
-- names still are (POSTO 15, C6 BANK, RUA 7 123).
-- +goose StatementBegin
CREATE FUNCTION search_words(name_norm text)
RETURNS TABLE (pos bigint, word text, searched boolean)
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
  SELECT u.i, u.w,
    u.w NOT IN ('de', 'da', 'do', 'das', 'dos', 'e', 'a', 'o', 'em', 'ltda', 'me', 'epp', 'eireli', 's', 'sa')
    AND u.w !~ '[0-9]{5,}'
    AND NOT (d.n >= 9 AND u.w ~ '[0-9]')
  FROM (SELECT length(regexp_replace(name_norm COLLATE "C", '[^0-9]+', '', 'g')) AS n) AS d,
    unnest(array_remove(regexp_split_to_array(lower(name_norm COLLATE "C"), '[^a-z0-9]+'), ''))
      WITH ORDINALITY AS u(w, i)
$$;
-- +goose StatementEnd

-- Words of the index: the searched words of search_words.
-- +goose StatementBegin
CREATE FUNCTION search_tokens(name_norm text) RETURNS tsvector
LANGUAGE sql IMMUTABLE PARALLEL SAFE
RETURN array_to_tsvector(ARRAY(SELECT DISTINCT word FROM search_words(name_norm) WHERE searched));
-- +goose StatementEnd

-- One row. generation grows by one at every swap; services reload their
-- cache of frequent tokens when they see it change. requested_at is set
-- (durably, and always growing) when new data asks for a rebuild;
-- rebuilt_from is the requested_at a successful rebuild started from, and
-- finished_collections the number of finished (done or failed) collections
-- when it started reading data. The tables are dirty when requested_at >
-- rebuilt_from, when more collections have finished since (recovers a lost
-- mark, whatever the clock does), or when they were never built
-- (generation 0) while entities exist; the worker rebuilds dirty tables at
-- start.
CREATE TABLE search_state (
    id           boolean     PRIMARY KEY DEFAULT true CHECK (id),
    generation   bigint      NOT NULL DEFAULT 0,
    rebuilt_at   timestamptz,
    requested_at timestamptz,
    rebuilt_from timestamptz,
    finished_collections bigint
);
INSERT INTO search_state DEFAULT VALUES;

-- Same shape, names and indexes as the tables Rebuild swaps in.
CREATE TABLE search_company (
    search_rank bigint NOT NULL,
    entity_id   bigint,
    name_norm   text COLLATE "C",
    grau        integer,
    tokens      tsvector,
    CONSTRAINT search_company_pkey PRIMARY KEY (search_rank)
);
CREATE INDEX search_company_name ON search_company (name_norm);
CREATE INDEX search_company_tokens ON search_company USING gin (tokens);

CREATE TABLE search_person (
    search_rank bigint NOT NULL,
    entity_id   bigint,
    kind        "char",
    name_norm   text COLLATE "C",
    grau        integer,
    tokens      tsvector,
    CONSTRAINT search_person_pkey PRIMARY KEY (search_rank)
);
CREATE INDEX search_person_name ON search_person (name_norm);
CREATE INDEX search_person_tokens ON search_person USING gin (tokens);

CREATE TABLE search_other (
    search_rank bigint NOT NULL,
    entity_id   bigint,
    kind        "char",
    name_norm   text COLLATE "C",
    grau        integer,
    tokens      tsvector,
    CONSTRAINT search_other_pkey PRIMARY KEY (search_rank)
);
CREATE INDEX search_other_name ON search_other (name_norm);
CREATE INDEX search_other_tokens ON search_other USING gin (tokens);

CREATE TABLE search_frequent_token (
    token text NOT NULL,
    ndoc  bigint,
    CONSTRAINT search_frequent_token_pkey PRIMARY KEY (token)
);

-- +goose Down
DROP TABLE IF EXISTS search_company, search_person, search_other, search_frequent_token, search_state,
    search_ranked, search_company_new, search_person_new, search_other_new, search_frequent_token_new;
DROP FUNCTION IF EXISTS search_tokens(text);
DROP FUNCTION IF EXISTS search_words(text);
DROP FUNCTION IF EXISTS search_norm(text);
