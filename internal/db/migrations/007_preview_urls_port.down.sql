DROP INDEX IF EXISTS idx_preview_urls_hostname;
ALTER TABLE sandbox_preview_urls DROP CONSTRAINT IF EXISTS uq_preview_url_sandbox_port;
ALTER TABLE sandbox_preview_urls DROP COLUMN IF EXISTS port;
