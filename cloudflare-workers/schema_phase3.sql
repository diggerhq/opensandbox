-- Phase 3: free-tier halt tracking
--
-- The CreditAccount DO is the source of truth for an org's free-tier balance
-- and halt status (lives in DO storage, lazy-loaded on first /check). The
-- mirror column below exists for one reason: the api-edge /internal/halt-list
-- endpoint needs to enumerate halted orgs without round-tripping every org's
-- DO. The DO writes back to this column on dispatchHalt success and clears it
-- on dispatchResume. D1 is the projection; DO is authoritative.
--
-- Apply with:
--   wrangler d1 execute opencomputer-dev --remote --file cloudflare-workers/schema_phase3.sql

ALTER TABLE orgs ADD COLUMN is_halted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE orgs ADD COLUMN halted_at INTEGER;

CREATE INDEX IF NOT EXISTS idx_orgs_halted ON orgs(is_halted) WHERE is_halted = 1;
