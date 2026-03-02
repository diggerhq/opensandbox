-- Extend templates table for sandbox snapshot templates.
-- Adds template_type ('dockerfile' vs 'sandbox'), S3 keys for snapshot templates,
-- and tracks which sandbox was the source.
ALTER TABLE templates ADD COLUMN IF NOT EXISTS template_type TEXT NOT NULL DEFAULT 'dockerfile';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS rootfs_s3_key TEXT;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS workspace_s3_key TEXT;
ALTER TABLE templates ADD COLUMN IF NOT EXISTS created_by_sandbox_id TEXT;

-- Make image_ref optional (snapshot templates don't have a Docker image ref).
ALTER TABLE templates ALTER COLUMN image_ref DROP NOT NULL;
ALTER TABLE templates ALTER COLUMN image_ref SET DEFAULT '';

-- Track which template a sandbox session was based on (set when creating from a snapshot template).
ALTER TABLE sandbox_sessions ADD COLUMN IF NOT EXISTS based_on_template_id UUID REFERENCES templates(id);
