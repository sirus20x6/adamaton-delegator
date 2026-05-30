-- Composite index to speed up eviction scans that filter by terminal
-- status and order by age. The pattern is:
--   SELECT ... FROM delegator.tasks
--   WHERE status NOT IN ('pending', 'running')
--   ORDER BY created_at [ASC|DESC]
-- A single (status, created_at) index satisfies both the equality/range
-- predicate on status and the sort on created_at without an extra sort step.
CREATE INDEX IF NOT EXISTS tasks_status_created_idx
    ON delegator.tasks (status, created_at DESC);
