-- Rename sandbox_checkpoints to sandbox_hibernations (these are hibernate records, not checkpoints)
ALTER TABLE sandbox_checkpoints RENAME TO sandbox_hibernations;
ALTER TABLE sandbox_hibernations RENAME COLUMN checkpoint_key TO hibernation_key;
ALTER INDEX idx_checkpoints_sandbox RENAME TO idx_hibernations_sandbox;
ALTER INDEX idx_checkpoints_org RENAME TO idx_hibernations_org;
ALTER INDEX idx_checkpoints_active RENAME TO idx_hibernations_active;
