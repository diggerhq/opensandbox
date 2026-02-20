-- Partial unique index for public templates (NULL org_id).
-- The existing UNIQUE(org_id, name, tag) doesn't enforce uniqueness when org_id IS NULL
-- because NULL != NULL in SQL.
CREATE UNIQUE INDEX IF NOT EXISTS idx_templates_public_name_tag
    ON templates (name, tag) WHERE org_id IS NULL;

-- Seed built-in public templates
INSERT INTO templates (org_id, name, tag, image_ref, is_public) VALUES
    (NULL, 'base',   'latest', 'docker.io/library/ubuntu:22.04',     true),
    (NULL, 'python', 'latest', 'docker.io/library/python:3.12-slim', true),
    (NULL, 'node',   'latest', 'docker.io/library/node:20-slim',     true)
ON CONFLICT DO NOTHING;
