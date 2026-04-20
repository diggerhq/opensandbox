-- Grandfather existing paying orgs at whatever Stripe Price IDs they currently
-- have. price_locked=TRUE means cmd/migrate-prices (and any future price-swap
-- tool) will skip this org by default. New signups default to FALSE so the
-- next price migration naturally picks them up.
ALTER TABLE orgs ADD COLUMN price_locked BOOLEAN NOT NULL DEFAULT FALSE;

-- One-shot lock of every currently-paying org at this point in time.
UPDATE orgs SET price_locked = TRUE WHERE stripe_subscription_id IS NOT NULL;
