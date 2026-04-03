-- Add migration state tracking to sandbox_sessions
ALTER TABLE sandbox_sessions ADD COLUMN IF NOT EXISTS migrating_to_worker TEXT NOT NULL DEFAULT '';
