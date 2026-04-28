-- Per-org ceiling on concurrent in-memory sandbox capacity, in whole GB.
-- Used both as the reservationLimitGb in the capacity calendar and as the
-- runtime admission cap when creating or waking sandboxes. Same shape as
-- max_disk_mb (migration 019). Default 2 covers free tier; pro orgs are
-- backfilled to 16. Per-row admin overrides remain available afterwards.
ALTER TABLE orgs ADD COLUMN max_memory_gb INTEGER NOT NULL DEFAULT 2
    CHECK (max_memory_gb >= 0);
UPDATE orgs SET max_memory_gb = 16 WHERE plan = 'pro';
