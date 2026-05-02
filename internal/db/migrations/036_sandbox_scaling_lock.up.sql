-- Sandbox-level "scaling lock". When true, blocks BOTH manual scaling
-- (POST /scale) AND automatic scaling (per-sandbox autoscaler). Useful when
-- the user wants predictable, fixed resources for a sandbox — e.g. a
-- benchmark, a long-running pinned workload, or a billing-sensitive setup.
--
-- Locking auto-disables autoscale (the lock handler clears autoscale_enabled
-- alongside setting scaling_locked=TRUE), but the partial-index filter here
-- is defense in depth in case the two columns ever drift.

ALTER TABLE sandbox_sessions
  ADD COLUMN scaling_locked BOOLEAN NOT NULL DEFAULT FALSE;
