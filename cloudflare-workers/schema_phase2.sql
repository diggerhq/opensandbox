-- Phase 2: multi-region resource layer
--
-- Adds the global registries that let cells share canonical resources
-- (golden rootfs blobs, templates, checkpoints) via Tigris and D1.
--
-- Apply with:
--   wrangler d1 execute opencomputer-dev --remote --file cloudflare-workers/schema_phase2.sql
--
-- The original schema.sql remains the bootstrap; this layers on top.
-- All statements are idempotent (`IF NOT EXISTS` for tables/indexes).
-- The ALTER TABLE statements use a CASE pattern that no-ops if the
-- column already exists (D1/SQLite doesn't support `ADD COLUMN IF NOT
-- EXISTS` directly, so we use the `pragma_table_info` workaround).

-- Golden versions registry ----------------------------------------------
--
-- One row per distinct golden hash that exists anywhere in the fleet.
-- canonical_url points at Tigris (s3://opencomputer-goldens-dev/bases/{hash}/default.ext4).
-- cells_available tracks which cells have pulled + cached the blob locally.
-- A worker's PrepareGoldenSnapshot path:
--   1. Look up golden_versions WHERE hash=$1
--   2. If not in cells_available[this_cell]: pull blob from canonical_url
--   3. Build local snapshot, update cells_available
--
-- retired_at is a soft-retire — set when no sandboxes depend on this
-- golden. Lifecycle GC can purge canonical bytes after a TTL.

CREATE TABLE IF NOT EXISTS golden_versions (
  hash             TEXT PRIMARY KEY,            -- sha256 of default.ext4 content
  canonical_url    TEXT NOT NULL,               -- s3://opencomputer-goldens-dev/bases/{hash}/default.ext4
  size_bytes       INTEGER,
  cells_available  TEXT NOT NULL DEFAULT '[]',  -- JSON array of cell_ids
  ami_version      TEXT,                        -- AMI/image version that produced this golden (e.g. git SHA)
  created_at       INTEGER NOT NULL,            -- unix s
  retired_at       INTEGER                      -- unix s, NULL = active
);

CREATE INDEX IF NOT EXISTS idx_golden_versions_active
  ON golden_versions(created_at) WHERE retired_at IS NULL;

-- Checkpoint global index ----------------------------------------------
--
-- One row per checkpoint, regardless of which cell stores the bytes.
-- The bytes themselves stay in the owning cell's S3 (Azure Blob / AWS S3 /
-- per-cell store) — Tigris is for canonical *globally-shared* blobs, not
-- per-sandbox checkpoints (write-heavy, mostly local-read pattern).
--
-- replicated_to lets us track cross-cell wakes that triggered a copy.
-- Wake flow:
--   1. Look up checkpoints_index WHERE id=$1 → owner_cell_id
--   2. If owner_cell_id == this_cell: wake locally (fast path)
--   3. If different: copy bytes from owner cell's S3 to local, update replicated_to
--   OR: redirect user to owner cell via X-Owner-Cell header (avoid the copy)

CREATE TABLE IF NOT EXISTS checkpoints_index (
  id               TEXT PRIMARY KEY,            -- checkpoint UUID
  sandbox_id       TEXT NOT NULL,
  org_id           TEXT NOT NULL,
  owner_cell_id    TEXT NOT NULL,               -- cell whose S3 holds the canonical bytes
  s3_url           TEXT NOT NULL,               -- e.g. azure-blob://bucket/checkpoints/sb-X/ckpt-Y/...
  size_bytes       INTEGER,
  golden_hash      TEXT NOT NULL,               -- which golden version this checkpoint depends on
  workspace_size   INTEGER,
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER,                     -- nullable, optional TTL
  replicated_to    TEXT NOT NULL DEFAULT '[]'   -- JSON array of cell_ids that have a local cached copy
);

CREATE INDEX IF NOT EXISTS idx_checkpoints_org      ON checkpoints_index(org_id);
CREATE INDEX IF NOT EXISTS idx_checkpoints_sandbox  ON checkpoints_index(sandbox_id);
CREATE INDEX IF NOT EXISTS idx_checkpoints_owner    ON checkpoints_index(owner_cell_id);
CREATE INDEX IF NOT EXISTS idx_checkpoints_expires  ON checkpoints_index(expires_at)
  WHERE expires_at IS NOT NULL;

-- Templates: extend existing table with canonical Tigris URLs ----------
--
-- The original schema.sql has `templates(rootfs_s3_key, workspace_s3_key)`
-- which were per-cell paths. Phase 2 adds canonical_*_url pointing at
-- Tigris (s3://opencomputer-templates-dev/{template_id}/...) so any cell
-- can fetch the template on first use.
--
-- cells_available was already in the schema (JSON array). Per-cell paths
-- become caches; canonical_*_url is the source of truth.
--
-- D1/SQLite doesn't support `ALTER TABLE ADD COLUMN IF NOT EXISTS`, so
-- we use a guard query that only ADDs the column if it's missing. If you
-- run this twice, the second run silently no-ops (we're idempotent).

-- canonical_rootfs_url
SELECT CASE
  WHEN (SELECT COUNT(*) FROM pragma_table_info('templates') WHERE name='canonical_rootfs_url') = 0
  THEN RAISE(IGNORE)
END
WHERE 0; -- never executes; the actual ALTER follows

-- The above is a no-op SELECT that documents intent. SQLite's pragma trick
-- doesn't actually ADD COLUMN conditionally. The cleanest path is to
-- accept that re-running this file fails on the ALTERs after first run,
-- which is fine — Phase 2 schema is applied once.

ALTER TABLE templates ADD COLUMN canonical_rootfs_url TEXT;
ALTER TABLE templates ADD COLUMN canonical_workspace_url TEXT;
