-- +goose Up

-- Every request for a collection, made by the scheduler (and, later, by
-- anyone else who wants it on record). It is the durable memory behind "do
-- not ask again within 24 h": a request may sit in the queue for a long time
-- before a collection row exists, and the queue itself is not a record.
--
-- published_at is set once the request reached the queue. A row without it
-- (the publish failed, or the process died between insert and publish) only
-- blocks a new request for a short while, so a failed publish is retried
-- soon instead of after a day.
CREATE TABLE collection_request (
    id           bigserial PRIMARY KEY,
    source_id    smallint    NOT NULL REFERENCES source (id),
    requested_at timestamptz NOT NULL DEFAULT now(),
    requested_by text        NOT NULL CHECK (requested_by <> ''),
    full_scope   boolean     NOT NULL,
    published_at timestamptz
);
CREATE INDEX collection_request_source_requested ON collection_request (source_id, requested_at DESC);

-- +goose Down
DROP TABLE collection_request;
