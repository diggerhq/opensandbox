-- Publishable checkpoints (design 009). When is_public=true, any org that
-- knows the checkpoint ID may fork it via createFromCheckpointCore; every
-- other checkpoint op (patch, delete) remains owner-scoped.
ALTER TABLE sandbox_checkpoints
    ADD COLUMN is_public BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX idx_checkpoints_public
    ON sandbox_checkpoints(is_public) WHERE is_public = true;
