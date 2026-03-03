-- Named checkpoints for manual save/restore within a sandbox
CREATE TABLE sandbox_checkpoints (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sandbox_id        TEXT NOT NULL,
    org_id            UUID NOT NULL REFERENCES orgs(id),
    name              TEXT NOT NULL,
    rootfs_s3_key     TEXT,
    workspace_s3_key  TEXT,
    sandbox_config    JSONB NOT NULL DEFAULT '{}',
    status            TEXT NOT NULL DEFAULT 'processing',  -- processing | ready
    size_bytes        BIGINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_checkpoints_sandbox ON sandbox_checkpoints(sandbox_id);
CREATE UNIQUE INDEX idx_checkpoints_name ON sandbox_checkpoints(sandbox_id, name);
