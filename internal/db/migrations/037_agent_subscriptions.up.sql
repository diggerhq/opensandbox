-- Per-agent paid feature subscriptions.
--
-- Each row represents one Stripe subscription gating one feature on one
-- agent (e.g. Telegram channel). Modeled as a separate table — not
-- columns on `orgs` — because:
--   - agent IDs are owned by sessions-api (text, not a UUID FK).
--   - more features (Discord, Slack, etc.) will land here without
--     adding columns.
--   - terminal subscriptions (canceled, past_due) need to be retained
--     for billing history without polluting active-state queries.
--
-- The (org_id, agent_id, feature) tuple is unique among non-terminal
-- rows — partial unique index below — so a single agent can be
-- "subscribed to telegram" only once at a time, but historic rows
-- coexist for audit.
CREATE TABLE agent_subscriptions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    agent_id TEXT NOT NULL,
    feature TEXT NOT NULL,                       -- 'telegram', future: 'discord', 'slack', ...
    stripe_customer_id TEXT NOT NULL,
    stripe_subscription_id TEXT NOT NULL,
    stripe_price_id TEXT NOT NULL,
    status TEXT NOT NULL,                        -- 'active' | 'trialing' | 'past_due' | 'canceled' | 'incomplete' | 'incomplete_expired' | 'unpaid'
    current_period_end TIMESTAMPTZ,
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
    canceled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One active row per (agent, feature). Terminal rows can stack so
-- billing history is preserved across resubscriptions.
CREATE UNIQUE INDEX agent_subscriptions_active_per_feature
    ON agent_subscriptions (agent_id, feature)
    WHERE status NOT IN ('canceled', 'incomplete_expired', 'unpaid');

CREATE INDEX agent_subscriptions_org ON agent_subscriptions (org_id);
CREATE INDEX agent_subscriptions_stripe_sub ON agent_subscriptions (stripe_subscription_id);
