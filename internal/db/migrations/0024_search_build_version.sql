-- +goose Up

-- What the live search tables were built with, so that a rebuild may reuse
-- what they already hold (internal/search, task 5 of
-- docs/performance/2026-load-optimization-design.md):
--
--   build_version       a fingerprint of everything name_norm and tokens are
--                       computed with: the definitions of search_norm,
--                       search_words and search_tokens as the catalogue holds
--                       them, the unaccent dictionary, the server version and
--                       the text of the build statements. A rebuild reuses
--                       name_norm and tokens of the live tables, and may skip
--                       when nothing searchable changed, only when the stored
--                       value equals the one computed now; a migration that
--                       changes any of those functions changes it by itself.
--   frequent_threshold  the frequent-token threshold the live
--                       search_frequent_token was computed with; a rebuild
--                       with another threshold is never skipped.
--
-- Both are NULL until the first rebuild after this migration, which is
-- therefore a full one. Safe on the national base: adding nullable columns
-- without a default only changes the catalogue (no rewrite, no scan) and
-- locks the one-row search_state for milliseconds; `make migrate` stops the
-- api and the worker first. The table-level grants of 0021 (worker SELECT,
-- UPDATE; api SELECT) cover the new columns.
ALTER TABLE search_state
    ADD COLUMN build_version text,
    ADD COLUMN frequent_threshold bigint;

-- +goose Down
ALTER TABLE search_state DROP COLUMN frequent_threshold, DROP COLUMN build_version;
