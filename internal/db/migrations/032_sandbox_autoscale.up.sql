-- Per-sandbox autoscaler config. NULL columns mean autoscale is disabled
-- (default for all existing sandboxes). When enabled, CP autoscaler resizes
-- the sandbox between min and max along the configured tier list (1, 4, 8,
-- 16 GB today) based on observed memory pressure.
--
-- Asymmetric thresholds: scale up fast on a brief 1m spike (>75%); scale
-- down only after sustained low utilization across 1m AND 5m AND 15m moving
-- averages (<25% on each). Cooldowns prevent flapping.

ALTER TABLE sandbox_sessions
  ADD COLUMN autoscale_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN autoscale_min_mb INT,
  ADD COLUMN autoscale_max_mb INT,
  ADD COLUMN autoscale_last_event_at TIMESTAMPTZ;

-- Partial index lets the autoscaler scan the active set cheaply.
CREATE INDEX idx_sandbox_sessions_autoscale_active
  ON sandbox_sessions (sandbox_id)
  WHERE autoscale_enabled = TRUE AND status = 'running';
