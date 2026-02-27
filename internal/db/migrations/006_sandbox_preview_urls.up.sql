CREATE TABLE sandbox_preview_urls (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sandbox_id      TEXT NOT NULL,
    org_id          UUID NOT NULL REFERENCES orgs(id),
    hostname        TEXT NOT NULL,
    cf_hostname_id  TEXT,
    ssl_status      TEXT DEFAULT 'initializing',
    auth_config     JSONB DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_preview_urls_sandbox ON sandbox_preview_urls(sandbox_id);
