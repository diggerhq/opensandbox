-- D1 schema for OpenSandbox global layer.
-- Apply with: wrangler d1 execute opencomputer-dev --file cloudflare-workers/schema.sql

-- Identity ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS orgs (
  id                     TEXT PRIMARY KEY,
  name                   TEXT NOT NULL,
  slug                   TEXT NOT NULL UNIQUE,
  plan                   TEXT NOT NULL,         -- "free" | "pro"
  home_cell              TEXT NOT NULL,
  stripe_customer_id     TEXT,
  stripe_subscription_id TEXT,
  workos_org_id          TEXT UNIQUE,
  is_personal            INTEGER NOT NULL DEFAULT 0,
  owner_user_id          TEXT,
  created_at             INTEGER NOT NULL,
  updated_at             INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
  id              TEXT PRIMARY KEY,
  email           TEXT NOT NULL UNIQUE,
  workos_user_id  TEXT UNIQUE,
  name            TEXT,
  created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS org_memberships (
  org_id     TEXT NOT NULL,
  user_id    TEXT NOT NULL,
  role       TEXT NOT NULL,       -- "owner" | "admin" | "member"
  created_at INTEGER NOT NULL,
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_memberships_user ON org_memberships(user_id);

CREATE TABLE IF NOT EXISTS api_keys (
  id          TEXT PRIMARY KEY,
  org_id      TEXT NOT NULL,
  created_by  TEXT,
  key_hash    TEXT NOT NULL UNIQUE,
  key_prefix  TEXT NOT NULL,
  name        TEXT NOT NULL,
  scopes      TEXT NOT NULL DEFAULT 'sandbox:*',
  last_used   INTEGER,
  expires_at  INTEGER,
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);
CREATE INDEX IF NOT EXISTS idx_api_keys_org  ON api_keys(org_id);

-- Global catalog ---------------------------------------------------------

CREATE TABLE IF NOT EXISTS templates (
  id               TEXT PRIMARY KEY,
  org_id           TEXT,                          -- NULL = public template
  name             TEXT NOT NULL,
  tag              TEXT NOT NULL DEFAULT 'latest',
  template_type    TEXT NOT NULL DEFAULT 'dockerfile',
  image_ref        TEXT,
  rootfs_s3_key    TEXT,
  workspace_s3_key TEXT,
  dockerfile       TEXT,
  is_public        INTEGER NOT NULL DEFAULT 0,
  status           TEXT NOT NULL DEFAULT 'ready',
  cells_available  TEXT NOT NULL DEFAULT '[]',    -- JSON array
  created_at       INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_templates_unique ON templates(org_id, name, tag);
CREATE INDEX IF NOT EXISTS idx_templates_public ON templates(is_public) WHERE is_public = 1;

-- Global secret stores (CPs fetch via HMAC at sandbox-create time;
-- regional CP never persists a copy).
CREATE TABLE IF NOT EXISTS secret_stores (
  id               TEXT PRIMARY KEY,
  org_id           TEXT NOT NULL,
  name             TEXT NOT NULL,
  egress_allowlist TEXT NOT NULL DEFAULT '[]',    -- JSON array
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_secret_stores_unique ON secret_stores(org_id, name);
CREATE INDEX IF NOT EXISTS idx_secret_stores_org ON secret_stores(org_id);

CREATE TABLE IF NOT EXISTS secret_store_entries (
  id              TEXT PRIMARY KEY,
  store_id        TEXT NOT NULL,
  name            TEXT NOT NULL,
  encrypted_value BLOB NOT NULL,                  -- AES-GCM, key in CF secret
  allowed_hosts   TEXT NOT NULL DEFAULT '[]',     -- JSON array
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_secret_entries_unique ON secret_store_entries(store_id, name);
CREATE INDEX IF NOT EXISTS idx_secret_entries_store ON secret_store_entries(store_id);

-- Global sandbox index (cross-region routing and listing) -----------------

CREATE TABLE IF NOT EXISTS sandboxes_index (
  id            TEXT PRIMARY KEY,                 -- sandbox_id
  org_id        TEXT NOT NULL,
  user_id       TEXT,
  cell_id       TEXT NOT NULL,
  worker_id     TEXT,
  status        TEXT NOT NULL,                    -- running | hibernated | stopped | error
  template_id   TEXT,
  created_at    INTEGER NOT NULL,
  last_event_at INTEGER,
  stopped_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_sandboxes_org_status ON sandboxes_index(org_id, status);
CREATE INDEX IF NOT EXISTS idx_sandboxes_cell       ON sandboxes_index(cell_id, status);
CREATE INDEX IF NOT EXISTS idx_sandboxes_active     ON sandboxes_index(org_id) WHERE status = 'running';

-- Events (audit + query) -------------------------------------------------

CREATE TABLE IF NOT EXISTS events (
  id         TEXT PRIMARY KEY,                    -- event UUID from worker
  cell_id    TEXT NOT NULL,
  type       TEXT NOT NULL,
  org_id     TEXT,
  sandbox_id TEXT,
  user_id    TEXT,
  worker_id  TEXT,
  ts         INTEGER NOT NULL,                    -- unix ms
  payload    TEXT NOT NULL                        -- JSON
);
CREATE INDEX IF NOT EXISTS idx_events_org_ts     ON events(org_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_events_sandbox_ts ON events(sandbox_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_events_cell_ts    ON events(cell_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_events_type_ts    ON events(type, ts DESC);

-- Billing (pro tier) ------------------------------------------------------

CREATE TABLE IF NOT EXISTS usage_snapshots (
  org_id            TEXT NOT NULL,
  snapshot_ts       INTEGER NOT NULL,             -- hourly bucket (unix s)
  cpu_seconds       INTEGER NOT NULL,
  wall_seconds      INTEGER NOT NULL,
  memory_gb_seconds REAL NOT NULL,
  sandbox_count     INTEGER NOT NULL,
  cost_cents        INTEGER NOT NULL,
  reported_to_stripe INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (org_id, snapshot_ts)
);
CREATE INDEX IF NOT EXISTS idx_usage_unreported ON usage_snapshots(reported_to_stripe, org_id) WHERE reported_to_stripe = 0;

CREATE TABLE IF NOT EXISTS org_subscription_items (
  org_id          TEXT NOT NULL,
  tier            TEXT NOT NULL,                  -- e.g. "memory" | "cpu"
  stripe_item_id  TEXT NOT NULL,
  price_id        TEXT NOT NULL,
  PRIMARY KEY (org_id, tier)
);
