-- Stripe billing fields on orgs
ALTER TABLE orgs ADD COLUMN stripe_customer_id TEXT UNIQUE;
ALTER TABLE orgs ADD COLUMN stripe_subscription_id TEXT;
ALTER TABLE orgs ADD COLUMN monthly_spend_cap_cents INT;
ALTER TABLE orgs ADD COLUMN last_usage_reported_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Mapping of org subscription items (one per metered price/tier)
CREATE TABLE IF NOT EXISTS org_subscription_items (
    org_id                      UUID NOT NULL REFERENCES orgs(id),
    memory_mb                   INT NOT NULL,
    stripe_subscription_item_id TEXT NOT NULL,
    PRIMARY KEY (org_id, memory_mb)
);
