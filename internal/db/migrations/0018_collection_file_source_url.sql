-- +goose Up

-- Safe on a populated database: adding a nullable column is a catalogue
-- change; the CHECK is validated over collection_file, one row per raw file
-- (thousands at most), in milliseconds, under an ACCESS EXCLUSIVE lock on
-- that table. Run with the loaders stopped.

-- source_url: the URL each raw file was downloaded from (the Receita WebDAV
-- file, the TSE/CVM zip, the Câmara/Senado API request), so provenance can
-- link to the source. NULL only for rows recorded before this column; the
-- connectors always record it (internal/collection.RecordSourceFile). Stored
-- without credentials: never user information (a share token sent as Basic
-- auth included), never a fragment.
ALTER TABLE collection_file ADD COLUMN source_url text;
ALTER TABLE collection_file ADD CONSTRAINT collection_file_source_url_safe CHECK (
    source_url IS NULL OR (source_url ~ '^https?://[^/@?#]+([/?]|$)' AND position('#' in source_url) = 0)
);

-- +goose Down
ALTER TABLE collection_file DROP CONSTRAINT collection_file_source_url_safe;
ALTER TABLE collection_file DROP COLUMN source_url;
