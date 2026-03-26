-- Billing fields on orgs
ALTER TABLE orgs ADD COLUMN stripe_customer_id TEXT UNIQUE;
ALTER TABLE orgs ADD COLUMN last_billed_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE orgs ADD COLUMN unbilled_usage_cents DOUBLE PRECISION NOT NULL DEFAULT 0;

-- Auto top-up settings
ALTER TABLE orgs ADD COLUMN auto_topup_enabled BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE orgs ADD COLUMN auto_topup_threshold_cents INT NOT NULL DEFAULT 500;
ALTER TABLE orgs ADD COLUMN auto_topup_amount_cents INT NOT NULL DEFAULT 5000;
ALTER TABLE orgs ADD COLUMN monthly_spend_cap_cents INT;

-- Credit transaction ledger for audit trail
CREATE TABLE IF NOT EXISTS credit_transactions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES orgs(id),
    amount_cents INT NOT NULL,
    balance_after_cents INT NOT NULL,
    type        TEXT NOT NULL,
    description TEXT,
    stripe_payment_intent_id TEXT,
    stripe_checkout_session_id TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_credit_txn_org ON credit_transactions(org_id, created_at);

-- Track monthly spend for cap enforcement
CREATE TABLE IF NOT EXISTS monthly_spend (
    org_id              UUID NOT NULL REFERENCES orgs(id),
    month               DATE NOT NULL,
    total_charged_cents INT NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, month)
);
