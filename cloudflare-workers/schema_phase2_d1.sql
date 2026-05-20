-- Phase 2 schema for D1 (no RAISE clauses — D1 rejects them outside triggers).
-- Strips the no-op pragma_table_info guard from schema_phase2.sql; only the
-- table creates + the ALTER TABLE calls remain. ALTER is one-shot; re-running
-- this file fails (idempotent re-run isn't supported by D1).

CREATE TABLE IF NOT EXISTS golden_versions (
  hash             TEXT PRIMARY KEY,
  canonical_url    TEXT NOT NULL,
  size_bytes       INTEGER,
  cells_available  TEXT NOT NULL DEFAULT '[]',
  ami_version      TEXT,
  created_at       INTEGER NOT NULL,
  retired_at       INTEGER
);

CREATE INDEX IF NOT EXISTS idx_golden_versions_active
  ON golden_versions(created_at) WHERE retired_at IS NULL;

CREATE TABLE IF NOT EXISTS checkpoints_index (
  id               TEXT PRIMARY KEY,
  sandbox_id       TEXT NOT NULL,
  org_id           TEXT NOT NULL,
  owner_cell_id    TEXT NOT NULL,
  s3_url           TEXT NOT NULL,
  size_bytes       INTEGER,
  golden_hash      TEXT NOT NULL,
  workspace_size   INTEGER,
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER,
  replicated_to    TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX IF NOT EXISTS idx_checkpoints_org      ON checkpoints_index(org_id);
CREATE INDEX IF NOT EXISTS idx_checkpoints_sandbox  ON checkpoints_index(sandbox_id);
CREATE INDEX IF NOT EXISTS idx_checkpoints_owner    ON checkpoints_index(owner_cell_id);
CREATE INDEX IF NOT EXISTS idx_checkpoints_expires  ON checkpoints_index(expires_at)
  WHERE expires_at IS NOT NULL;

-- templates: extend with canonical Tigris URLs. ALTERs run once; re-runs fail.
ALTER TABLE templates ADD COLUMN canonical_rootfs_url TEXT;
ALTER TABLE templates ADD COLUMN canonical_workspace_url TEXT;
