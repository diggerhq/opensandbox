DROP INDEX IF EXISTS idx_scale_events_org_disk;
ALTER TABLE sandbox_scale_events DROP COLUMN IF EXISTS disk_mb;
