DROP TABLE IF EXISTS monthly_spend;
DROP TABLE IF EXISTS credit_transactions;

ALTER TABLE orgs DROP COLUMN IF EXISTS monthly_spend_cap_cents;
ALTER TABLE orgs DROP COLUMN IF EXISTS auto_topup_amount_cents;
ALTER TABLE orgs DROP COLUMN IF EXISTS auto_topup_threshold_cents;
ALTER TABLE orgs DROP COLUMN IF EXISTS auto_topup_enabled;
ALTER TABLE orgs DROP COLUMN IF EXISTS unbilled_usage_cents;
ALTER TABLE orgs DROP COLUMN IF EXISTS last_billed_at;
ALTER TABLE orgs DROP COLUMN IF EXISTS stripe_customer_id;
