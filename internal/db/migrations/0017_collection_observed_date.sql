-- +goose Up

-- Safe on a populated database: collection holds one row per run (tens to
-- hundreds of rows), so the backfill and the NOT NULL/CHECK validation scan
-- only that table, in milliseconds. ALTER TABLE takes an ACCESS EXCLUSIVE
-- lock on collection for that time; every loader reads collection, so run
-- with the loaders stopped (as every migration here).

-- observed_date: a date by which the source's state was actually observed.
-- For an edition source it is the edition's publication date (Receita: the
-- newest Last-Modified among the edition's data parts), otherwise the day the
-- collection started (UTC). Closing a claim a full collection did not see
-- again uses observed_date + 1 as the exclusive upper bound
-- (internal/claims.CloseUnseen); staleness of edition sources counts from it
-- (internal/freshness). Rows from before this column: reference_date, the
-- only date they have (for them it equals the old closing bound).
ALTER TABLE collection ADD COLUMN observed_date date;
UPDATE collection SET observed_date = reference_date;
ALTER TABLE collection ALTER COLUMN observed_date SET NOT NULL;
ALTER TABLE collection ADD CONSTRAINT collection_observed_not_before_reference
    CHECK (observed_date >= reference_date);

-- source.editions: the source is published as dated editions (written by
-- internal/sources.Sync). Its staleness counts from the observed date of
-- its newest done full collection; other sources count from the reference
-- date. A constant default: a catalogue change, no rewrite.
ALTER TABLE source ADD COLUMN editions boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE source DROP COLUMN editions;
ALTER TABLE collection DROP CONSTRAINT collection_observed_not_before_reference;
ALTER TABLE collection DROP COLUMN observed_date;
