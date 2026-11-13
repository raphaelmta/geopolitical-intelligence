-- +goose Up

-- Background jobs (internal/jobs): long graph queries the API enqueues and the
-- browser polls. Operational data, not a claim: no provenance, and rows are
-- plain mutable state (status, progress, result), so no delete ban either.
-- updated_at is the heartbeat of a running job: a worker that died leaves it
-- behind, which is how another worker knows to take the job over. run_id
-- names the run that owns a running job (set when a worker claims it): every
-- write of a run is conditioned on it, so a run that was taken over cannot
-- overwrite the new owner's outcome.
CREATE TABLE job (
    id          uuid PRIMARY KEY,
    type        text NOT NULL,
    params      jsonb NOT NULL,
    status      text NOT NULL CHECK (status IN ('queued', 'running', 'done', 'failed')),
    progress    double precision NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 1),
    result      jsonb,
    error       text NOT NULL DEFAULT '',
    run_id      uuid,
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    CHECK ((status IN ('done', 'failed')) = (finished_at IS NOT NULL))
);

CREATE INDEX job_created_by_created_at ON job (created_by, created_at DESC);

-- +goose Down
DROP TABLE job;
