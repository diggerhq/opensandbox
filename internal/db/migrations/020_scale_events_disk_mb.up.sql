-- Track per-sandbox disk size on scale events for metered disk-overage billing.
-- Disk doesn't change mid-life, but bracketing it alongside memory tiers lets the
-- existing GetOrgUsage aggregation produce GB-seconds per (memory_mb, disk_mb).
ALTER TABLE sandbox_scale_events ADD COLUMN disk_mb INTEGER NOT NULL DEFAULT 20480;
CREATE INDEX IF NOT EXISTS idx_scale_events_org_disk ON sandbox_scale_events(org_id, disk_mb);
