-- Image cache: maps content-hashed image manifests to checkpoints.
-- Named entries are "snapshots" (persistent, user-facing); unnamed are auto-cached.
CREATE TABLE image_cache (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL REFERENCES orgs(id),
    content_hash    TEXT NOT NULL,
    checkpoint_id   UUID REFERENCES sandbox_checkpoints(id) ON DELETE SET NULL,
    name            TEXT,  -- NULL for auto-cached; unique per org when set
    manifest        JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'building',  -- building | ready | failed
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only one entry per (org, content_hash)
CREATE UNIQUE INDEX idx_image_cache_org_hash ON image_cache(org_id, content_hash);

-- Named snapshots are unique per org
CREATE UNIQUE INDEX idx_image_cache_org_name ON image_cache(org_id, name) WHERE name IS NOT NULL;

-- Fast lookup by checkpoint
CREATE INDEX idx_image_cache_checkpoint ON image_cache(checkpoint_id);
