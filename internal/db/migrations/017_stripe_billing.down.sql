DROP TABLE IF EXISTS org_subscription_items;
ALTER TABLE orgs DROP COLUMN IF EXISTS last_usage_reported_at;
ALTER TABLE orgs DROP COLUMN IF EXISTS monthly_spend_cap_cents;
ALTER TABLE orgs DROP COLUMN IF EXISTS stripe_subscription_id;
ALTER TABLE orgs DROP COLUMN IF EXISTS stripe_customer_id;
