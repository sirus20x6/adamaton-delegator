-- Standalone index on recorded_at for time-range queries and retention
-- sweeps that prune old rows from the append-only usage_records ledger.
-- The existing idx_usage_provider_time covers (provider, recorded_at DESC)
-- for per-provider window queries; this index covers global time-only
-- access patterns (e.g. DELETE WHERE recorded_at < $cutoff).
CREATE INDEX IF NOT EXISTS idx_usage_recorded_at
    ON budget.usage_records (recorded_at DESC);
