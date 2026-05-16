CREATE SCHEMA IF NOT EXISTS delegator;

-- Task records produced by the MCP writer and consumed by the apiserver
-- reader. Cross-process visibility was sqlite's job; postgres MVCC
-- removes the need for the per-request "open + close" dance the old
-- reader did to avoid blocking the writer.
CREATE TABLE delegator.tasks (
    id            TEXT        PRIMARY KEY,
    agent         TEXT        NOT NULL,
    provider      TEXT        NOT NULL DEFAULT '',
    difficulty    TEXT        NOT NULL DEFAULT '',
    priority      TEXT        NOT NULL DEFAULT '',
    prompt        TEXT        NOT NULL,
    working_dir   TEXT        NOT NULL DEFAULT '',
    model         TEXT        NOT NULL DEFAULT '',
    status        TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    started_at    TIMESTAMPTZ,
    completed_at  TIMESTAMPTZ,
    exit_code     INTEGER     NOT NULL DEFAULT 0,
    output        TEXT        NOT NULL DEFAULT '',
    stderr        TEXT        NOT NULL DEFAULT '',
    error         TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX tasks_created_idx ON delegator.tasks (created_at DESC);
CREATE INDEX tasks_status_idx  ON delegator.tasks (status);
