CREATE SCHEMA IF NOT EXISTS budget;

-- Ledger of every recorded usage event. Append-only; rows are not
-- deleted by the runtime. A retention sweep can prune by recorded_at
-- if the table grows unbounded.
CREATE TABLE budget.usage_records (
    id                BIGSERIAL   PRIMARY KEY,
    provider          TEXT        NOT NULL,
    model             TEXT        NOT NULL DEFAULT '',
    prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
    completion_tokens BIGINT      NOT NULL DEFAULT 0,
    total_tokens      BIGINT      NOT NULL DEFAULT 0,
    task_id           TEXT        NOT NULL DEFAULT '',
    task_complexity   TEXT        NOT NULL DEFAULT '',
    success           BOOLEAN     NOT NULL DEFAULT TRUE,
    recorded_at       TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_usage_provider_time
    ON budget.usage_records (provider, recorded_at DESC);

-- Per-provider rolling counters. RecordUsage upserts here on each call
-- so consumers can read the live remaining budget without aggregating
-- the ledger.
CREATE TABLE budget.provider_status (
    provider         TEXT        PRIMARY KEY,
    daily_used       BIGINT      NOT NULL DEFAULT 0,
    daily_limit      BIGINT      NOT NULL DEFAULT 0,
    weekly_used      BIGINT      NOT NULL DEFAULT 0,
    weekly_limit     BIGINT      NOT NULL DEFAULT 0,
    daily_reset_at   TIMESTAMPTZ NOT NULL,
    weekly_reset_at  TIMESTAMPTZ NOT NULL,
    is_available     BOOLEAN     NOT NULL DEFAULT TRUE,
    error_count      INTEGER     NOT NULL DEFAULT 0,
    last_updated     TIMESTAMPTZ NOT NULL
);
