-- Add status column to templates (processing while snapshot is being prepared, ready when usable)
ALTER TABLE templates ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'ready';
