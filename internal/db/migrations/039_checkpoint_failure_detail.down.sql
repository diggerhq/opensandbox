ALTER TABLE sandbox_checkpoints
  DROP COLUMN IF EXISTS error_msg,
  DROP COLUMN IF EXISTS failed_at;
