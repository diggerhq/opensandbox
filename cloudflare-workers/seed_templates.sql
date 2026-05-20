-- Seed built-in public templates in D1. Mirrors the legacy PG seeds in
-- internal/db/migrations/004_seed_templates.up.sql + 008_default_template.up.sql.
-- Public templates (org_id IS NULL) are visible to every org via the
-- /internal/templates/by-name public-fallback path.
--
-- Apply with:
--   cd cloudflare-workers/events-ingest && \
--     npx wrangler d1 execute opencomputer-dev --remote \
--       --file ../seed_templates.sql

INSERT INTO templates
  (id, org_id, name, tag, template_type, image_ref, is_public, status, cells_available, created_at)
VALUES
  ('00000000-0000-0000-0000-000000000001', NULL, 'default', 'latest', 'dockerfile', 'opensandbox/default:latest',         1, 'ready', '[]', unixepoch()),
  ('00000000-0000-0000-0000-000000000002', NULL, 'base',    'latest', 'dockerfile', 'docker.io/library/ubuntu:22.04',     1, 'ready', '[]', unixepoch()),
  ('00000000-0000-0000-0000-000000000003', NULL, 'ubuntu',  'latest', 'dockerfile', 'docker.io/library/ubuntu:22.04',     1, 'ready', '[]', unixepoch()),
  ('00000000-0000-0000-0000-000000000004', NULL, 'python',  'latest', 'dockerfile', 'docker.io/library/python:3.12-slim', 1, 'ready', '[]', unixepoch()),
  ('00000000-0000-0000-0000-000000000005', NULL, 'node',    'latest', 'dockerfile', 'docker.io/library/node:20-slim',     1, 'ready', '[]', unixepoch())
ON CONFLICT(id) DO NOTHING;
