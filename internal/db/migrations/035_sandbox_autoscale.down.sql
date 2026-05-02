DROP INDEX IF EXISTS idx_sandbox_sessions_autoscale_active;

ALTER TABLE sandbox_sessions
  DROP COLUMN IF EXISTS autoscale_last_event_at,
  DROP COLUMN IF EXISTS autoscale_max_mb,
  DROP COLUMN IF EXISTS autoscale_min_mb,
  DROP COLUMN IF EXISTS autoscale_enabled;
