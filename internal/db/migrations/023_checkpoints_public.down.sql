DROP INDEX IF EXISTS idx_checkpoints_public;
ALTER TABLE sandbox_checkpoints DROP COLUMN IF EXISTS is_public;
