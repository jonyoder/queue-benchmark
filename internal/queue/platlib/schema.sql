-- schema.sql — minimal Postgres schema for the rsqueue adapter.
-- Applied by Harness.New (see store.go: applySchema).
--
-- This schema is intentionally minimal: no queue_group (the benchmark
-- uses groupId = 0 only), no chunk-related tables (chunked work is
-- outside the benchmark scope), and no queue_group support. See the
-- example store at rstudio/platform-lib for the full shape.

CREATE TABLE IF NOT EXISTS queue (
    id         BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    name       TEXT        NOT NULL,
    priority   BIGINT      NOT NULL,
    permit     BIGINT      NOT NULL DEFAULT 0,
    item       BYTEA       NOT NULL,
    group_id   BIGINT,
    type       BIGINT      NOT NULL,
    address    TEXT        UNIQUE,
    carrier    BYTEA
);

CREATE INDEX IF NOT EXISTS idx_queue_name_pri_permit_type
    ON queue (name, priority, permit, type);

-- queue_permits is effectively a monotonic ID generator for claims.
-- The agent deletes the permit row when work completes (via QueueDelete),
-- along with the queue row that references it.
CREATE TABLE IF NOT EXISTS queue_permits (
    id         BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Persisted failure info for addressed work. Cleared by QueueAddressedComplete
-- on subsequent re-runs or on explicit success.
CREATE TABLE IF NOT EXISTS queue_failure (
    address TEXT NOT NULL,
    error   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_queue_failure_address ON queue_failure (address);
