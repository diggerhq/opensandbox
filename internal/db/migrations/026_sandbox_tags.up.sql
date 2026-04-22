-- Sandbox tags — per-sandbox customer-defined groupings (team, env,
-- customer, …) used to attribute usage without touching the Stripe
-- pricing path. Row-per-tag so grouping SQL is a single join and
-- per-sandbox tag-count limits are trivial. org_id is denormalized
-- so GET /tags can filter on (org_id, key) without a join.
CREATE TABLE IF NOT EXISTS sandbox_tags (
    org_id      UUID        NOT NULL,
    sandbox_id  TEXT        NOT NULL,
    key         TEXT        NOT NULL,
    value       TEXT        NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sandbox_id, key)
);

CREATE INDEX IF NOT EXISTS idx_sandbox_tags_org_key_value
    ON sandbox_tags (org_id, key, value);
