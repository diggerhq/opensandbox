-- Per-org billing pipeline selector. The unified pipeline (allocator
-- + outbox + sender) ships outbox rows to Stripe for orgs in
-- 'unified' mode; UsageReporter ships for orgs in 'legacy' mode.
-- Both paths use the same dollar amounts via current tiered rates —
-- the column controls which goroutine emits the meter events, not
-- what the customer pays.
--
-- Policy: new orgs default to 'unified' (covered by the column
-- DEFAULT). Existing orgs are pinned to 'legacy' below to preserve
-- their current billing path until an explicit decision flips them.
ALTER TABLE orgs ADD COLUMN billing_mode TEXT NOT NULL DEFAULT 'unified'
    CHECK (billing_mode IN ('legacy', 'unified'));

UPDATE orgs SET billing_mode = 'legacy';
