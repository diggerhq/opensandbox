-- Per-org ceiling on concurrent in-memory sandbox capacity, in whole GB.
-- v1 use: returned as reservationLimitGb on the capacity calendar so
-- customers see how much they can reserve per interval. Runtime admission
-- against this column is deferred to a follow-up phase — see
-- ws-pricing/work/001 "Runtime admission". Same shape as max_disk_mb
-- (migration 019). Default 8 covers free tier; pro orgs are backfilled to
-- 128. Per-row admin overrides remain available afterwards.
ALTER TABLE orgs ADD COLUMN max_memory_gb INTEGER NOT NULL DEFAULT 8
    CHECK (max_memory_gb >= 0);
UPDATE orgs SET max_memory_gb = 128 WHERE plan = 'pro';
