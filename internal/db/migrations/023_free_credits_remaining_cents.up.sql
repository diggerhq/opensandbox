-- Free-tier trial credits. Decremented by the usage reporter while plan='free'.
-- At <=0, the org's running sandboxes are force-hibernated and new create/wake
-- is rejected until the org upgrades to pro. Ignored while plan='pro'.
ALTER TABLE orgs ADD COLUMN free_credits_remaining_cents BIGINT NOT NULL DEFAULT 500;

-- Grandfather existing free orgs to the new $5 trial starting balance.
UPDATE orgs SET free_credits_remaining_cents = 500 WHERE plan = 'free';

-- Reset the usage-reporting watermark for free orgs so the first deduction
-- window only covers post-migration usage, not months of historical scale
-- events that would cause a massive one-shot over-deduction.
UPDATE orgs SET last_usage_reported_at = now() WHERE plan = 'free';
