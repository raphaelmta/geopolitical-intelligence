-- +goose Up

-- The last successful check of a source that found nothing to collect: a
-- full request whose newest published edition is already loaded creates no
-- collection, so without this row a failed attempt before it would keep the
-- source "failed" until a new edition appears. Operational state, one row per
-- source, upserted by the runner (not a claim, not audited history).
CREATE TABLE source_check (
    source_id       smallint    PRIMARY KEY REFERENCES source (id),
    last_checked_at timestamptz NOT NULL,
    last_outcome    text        NOT NULL CHECK (last_outcome IN ('nothing_new'))
);

-- +goose Down
DROP TABLE source_check;
