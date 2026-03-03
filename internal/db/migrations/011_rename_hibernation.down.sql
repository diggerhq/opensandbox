-- Reverse: rename sandbox_hibernations back to sandbox_checkpoints
ALTER TABLE sandbox_hibernations RENAME TO sandbox_checkpoints;
ALTER TABLE sandbox_checkpoints RENAME COLUMN hibernation_key TO checkpoint_key;
ALTER INDEX idx_hibernations_sandbox RENAME TO idx_checkpoints_sandbox;
ALTER INDEX idx_hibernations_org RENAME TO idx_checkpoints_org;
ALTER INDEX idx_hibernations_active RENAME TO idx_checkpoints_active;
