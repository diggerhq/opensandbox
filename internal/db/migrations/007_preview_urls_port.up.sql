ALTER TABLE sandbox_preview_urls ADD COLUMN port INTEGER NOT NULL DEFAULT 80;
ALTER TABLE sandbox_preview_urls ADD CONSTRAINT uq_preview_url_sandbox_port UNIQUE (sandbox_id, port);
CREATE UNIQUE INDEX IF NOT EXISTS idx_preview_urls_hostname ON sandbox_preview_urls(hostname);
