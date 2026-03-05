-- Patch scripts attached to checkpoints for rolling updates
CREATE TABLE checkpoint_patches (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    checkpoint_id   UUID NOT NULL REFERENCES sandbox_checkpoints(id) ON DELETE CASCADE,
    sequence        INT NOT NULL,
    script          TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    strategy        TEXT NOT NULL DEFAULT 'hot',  -- hot | on_wake
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(checkpoint_id, sequence)
);
CREATE INDEX idx_checkpoint_patches_checkpoint ON checkpoint_patches(checkpoint_id, sequence);

-- Track which checkpoint a sandbox was forked from + patch level
ALTER TABLE sandbox_sessions ADD COLUMN IF NOT EXISTS based_on_checkpoint_id UUID REFERENCES sandbox_checkpoints(id);
ALTER TABLE sandbox_sessions ADD COLUMN IF NOT EXISTS last_patch_sequence INT NOT NULL DEFAULT 0;
