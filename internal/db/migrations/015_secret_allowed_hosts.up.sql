-- Replace projects with secret stores: simpler entity that just holds secrets + egress config.
-- Projects bundled too much (template, cpu, mem, timeout) — those belong on sandbox creation.

-- Create secret stores
CREATE TABLE IF NOT EXISTS secret_stores (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    egress_allowlist TEXT[] NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(org_id, name)
);
CREATE INDEX IF NOT EXISTS idx_secret_stores_org ON secret_stores(org_id);

-- Create secret store entries with per-secret host restrictions
CREATE TABLE IF NOT EXISTS secret_store_entries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    store_id        UUID NOT NULL REFERENCES secret_stores(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    encrypted_value BYTEA NOT NULL,
    allowed_hosts   TEXT[] NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(store_id, name)
);
CREATE INDEX IF NOT EXISTS idx_secret_store_entries_store ON secret_store_entries(store_id);

-- Drop old project tables (secrets first due to FK)
DROP TABLE IF EXISTS project_secrets;
DROP TABLE IF EXISTS projects;

-- Update sandbox_sessions: replace project_id with secret_store_id
ALTER TABLE sandbox_sessions DROP COLUMN IF EXISTS project_id;
ALTER TABLE sandbox_sessions ADD COLUMN IF NOT EXISTS secret_store_id UUID REFERENCES secret_stores(id);
