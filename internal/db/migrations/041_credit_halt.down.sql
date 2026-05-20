DROP INDEX IF EXISTS idx_sandbox_sessions_halt_reason;
ALTER TABLE sandbox_sessions DROP COLUMN IF EXISTS halt_reason;

DROP INDEX IF EXISTS idx_orgs_halted;
ALTER TABLE orgs DROP COLUMN IF EXISTS halted_at;
ALTER TABLE orgs DROP COLUMN IF EXISTS is_halted;
