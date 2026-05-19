-- Down migration for 041_strip_global_concerns.
--
-- This is a one-way cutover in practice: D1 is the source of truth post-041,
-- so even if we recreate the table SHAPES here, there's no path to recreate
-- the DATA (it never existed in PG once D1 took over). We recreate schema for
-- the rare case of forensic comparison against a pre-cutover dump.
--
-- FK constraints on cell-local tables that referenced these globals are
-- NOT restored here — the columns survived as plain UUIDs and re-adding the
-- FKs would 23503-fail on every row.

BEGIN;

CREATE TABLE IF NOT EXISTS orgs (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                     TEXT NOT NULL,
    slug                     TEXT NOT NULL UNIQUE,
    plan                     TEXT NOT NULL DEFAULT 'free',
    max_concurrent_sandboxes INT NOT NULL DEFAULT 5,
    max_sandbox_timeout_sec  INT NOT NULL DEFAULT 3600,
    workos_org_id            TEXT UNIQUE,
    is_personal              BOOLEAN NOT NULL DEFAULT false,
    owner_user_id            UUID,
    credit_balance_cents     INT NOT NULL DEFAULT 3000,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL,
    email           TEXT NOT NULL UNIQUE,
    name            TEXT,
    role            TEXT NOT NULL DEFAULT 'member',
    workos_user_id  TEXT UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS api_keys (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL,
    created_by  UUID,
    key_hash    TEXT NOT NULL UNIQUE,
    key_prefix  TEXT NOT NULL,
    name        TEXT NOT NULL,
    scopes      TEXT[] NOT NULL DEFAULT '{sandbox:*}',
    last_used   TIMESTAMPTZ,
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);

CREATE TABLE IF NOT EXISTS user_sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL,
    access_token  TEXT NOT NULL UNIQUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '7 days'
);
CREATE INDEX IF NOT EXISTS idx_user_sessions_token ON user_sessions(access_token);
CREATE INDEX IF NOT EXISTS idx_user_sessions_user  ON user_sessions(user_id);

CREATE TABLE IF NOT EXISTS templates (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id                UUID,
    name                  TEXT NOT NULL,
    tag                   TEXT NOT NULL DEFAULT 'latest',
    template_type         TEXT NOT NULL DEFAULT 'dockerfile',
    image_ref             TEXT,
    rootfs_s3_key         TEXT,
    workspace_s3_key      TEXT,
    dockerfile            TEXT,
    is_public             BOOLEAN NOT NULL DEFAULT false,
    status                TEXT NOT NULL DEFAULT 'ready',
    created_by_sandbox_id TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(org_id, name, tag)
);

CREATE TABLE IF NOT EXISTS secret_stores (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           UUID NOT NULL,
    name             TEXT NOT NULL,
    egress_allowlist TEXT[] NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(org_id, name)
);
CREATE INDEX IF NOT EXISTS idx_secret_stores_org ON secret_stores(org_id);

CREATE TABLE IF NOT EXISTS secret_store_entries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id        UUID NOT NULL,
    name            TEXT NOT NULL,
    encrypted_value BYTEA NOT NULL,
    allowed_hosts   TEXT[] NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(store_id, name)
);
CREATE INDEX IF NOT EXISTS idx_secret_store_entries_store ON secret_store_entries(store_id);

CREATE TABLE IF NOT EXISTS usage_snapshots (
    org_id              UUID NOT NULL,
    snapshot_ts         TIMESTAMPTZ NOT NULL,
    cpu_seconds         INT NOT NULL,
    wall_seconds        INT NOT NULL,
    memory_gb_seconds   DOUBLE PRECISION NOT NULL,
    sandbox_count       INT NOT NULL,
    cost_cents          INT NOT NULL,
    reported_to_stripe  BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (org_id, snapshot_ts)
);

CREATE TABLE IF NOT EXISTS org_subscription_items (
    org_id          UUID NOT NULL,
    tier            TEXT NOT NULL,
    stripe_item_id  TEXT NOT NULL,
    price_id        TEXT NOT NULL,
    PRIMARY KEY (org_id, tier)
);

CREATE TABLE IF NOT EXISTS agent_subscriptions (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id                 UUID NOT NULL,
    agent_id               TEXT NOT NULL,
    feature                TEXT NOT NULL,
    stripe_customer_id     TEXT NOT NULL,
    stripe_subscription_id TEXT NOT NULL,
    stripe_price_id        TEXT NOT NULL,
    status                 TEXT NOT NULL,
    current_period_end     TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
