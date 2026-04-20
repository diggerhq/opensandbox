-- Surface patch errors to users. Cleared on next successful patch application.
ALTER TABLE sandbox_sessions ADD COLUMN IF NOT EXISTS patch_error TEXT;
