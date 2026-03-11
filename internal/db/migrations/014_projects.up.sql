-- Projects: higher-level entity that groups sandboxes and holds secrets + config
CREATE TABLE projects (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    template        TEXT NOT NULL DEFAULT 'default',
    cpu_count       INT NOT NULL DEFAULT 1,
    memory_mb       INT NOT NULL DEFAULT 1024,
    timeout_sec     INT NOT NULL DEFAULT 300,
    egress_allowlist TEXT[] NOT NULL DEFAULT '{}',  -- e.g. {"api.anthropic.com","*.amazonaws.com"}, empty = allow all
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(org_id, name)
);
CREATE INDEX idx_projects_org ON projects(org_id);

-- Secrets belonging to a project, encrypted at rest (AES-256-GCM)
CREATE TABLE project_secrets (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,       -- env var name (e.g. ANTHROPIC_API_KEY)
    encrypted_value BYTEA NOT NULL,      -- AES-256-GCM ciphertext (nonce || ciphertext)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(project_id, name)
);
CREATE INDEX idx_project_secrets_project ON project_secrets(project_id);

-- Link sandboxes to projects
ALTER TABLE sandbox_sessions ADD COLUMN IF NOT EXISTS project_id UUID REFERENCES projects(id);
CREATE INDEX IF NOT EXISTS idx_sandbox_sessions_project ON sandbox_sessions(project_id);
