-- Sandbox tags — per-sandbox customer-defined groupings (team, env,
-- customer, …) used to attribute usage without touching the Stripe
-- pricing path. Row-per-tag so grouping SQL is a single join and
-- per-sandbox tag-count limits are trivial.
--
-- org_id is part of the primary key, not just a filter column. Sandbox
-- IDs are short `sb-xxxxxxxx` strings generated independently per
-- create path and the schema does not enforce cross-org uniqueness — a
-- `(sandbox_id, key)` PK would let a collision alias tag state across
-- tenants. Every read, join, and write scopes on (org_id, sandbox_id).
CREATE TABLE IF NOT EXISTS sandbox_tags (
    org_id      UUID        NOT NULL,
    sandbox_id  TEXT        NOT NULL,
    key         TEXT        NOT NULL,
    value       TEXT        NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, sandbox_id, key)
);

CREATE INDEX IF NOT EXISTS idx_sandbox_tags_org_key_value
    ON sandbox_tags (org_id, key, value);
