-- Add updated_at columns + auto-update triggers to tables that don't have
-- them, so the upcoming prod → D1 backfill script can use a uniform
-- watermark (updated_at) across every table during the cutover delta sync.
--
-- Without this, no delta-sync strategy catches row UPDATEs between the
-- initial snapshot and the cutover window — only INSERTs (via created_at)
-- — and a customer who renames their workspace, rotates a key, or has a
-- sandbox status change in that gap would silently desync into D1.
--
-- Why the trigger and not just DEFAULT now(): in Postgres, DEFAULT only
-- fires on INSERT, never on UPDATE. We need updated_at to track "last
-- modified", which requires a BEFORE UPDATE trigger. The trigger function
-- is shared across all tables — define once, reuse seven times.
--
-- Why nullable then backfilled then NOT NULL: ALTER TABLE ADD COLUMN with
-- a volatile DEFAULT (now()) forces PG to rewrite the table (slow on big
-- tables, locks). Splitting into "add nullable" → "backfill" → "set NOT
-- NULL + DEFAULT" keeps the schema change instant and the backfill
-- well-bounded.
--
-- Why each table is wrapped in IF EXISTS: prod and dev fleets have drifted
-- over time — some envs predate certain migrations (e.g. dev was missing
-- org_memberships at the time 040 was written). Wrapping each block in a
-- DO $$ ... IF EXISTS THEN ... END $$ makes the migration safely
-- idempotent across whatever schema actually exists, instead of failing
-- hard on the first missing table.
--
-- Tables targeted (no updated_at today):
--   users, api_keys, org_memberships, templates,
--   sandbox_sessions, sandbox_checkpoints, org_subscription_items
--
-- Tables skipped (already have a usable watermark):
--   orgs.updated_at, secret_stores.updated_at, secret_store_entries.updated_at,
--   agent_subscriptions.updated_at, image_cache.last_used_at,
--   usage_snapshots.snapshot_ts.

-- Shared trigger function — sets NEW.updated_at = now() on every UPDATE.
-- CREATE OR REPLACE so re-running the migration is a no-op for the function.
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── users ─────────────────────────────────────────────────────────────
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'users') THEN
    ALTER TABLE users ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
    UPDATE users SET updated_at = created_at WHERE updated_at IS NULL;
    ALTER TABLE users ALTER COLUMN updated_at SET NOT NULL;
    ALTER TABLE users ALTER COLUMN updated_at SET DEFAULT now();
    DROP TRIGGER IF EXISTS users_set_updated_at ON users;
    CREATE TRIGGER users_set_updated_at
      BEFORE UPDATE ON users
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;

-- ── api_keys ──────────────────────────────────────────────────────────
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'api_keys') THEN
    ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
    UPDATE api_keys SET updated_at = created_at WHERE updated_at IS NULL;
    ALTER TABLE api_keys ALTER COLUMN updated_at SET NOT NULL;
    ALTER TABLE api_keys ALTER COLUMN updated_at SET DEFAULT now();
    DROP TRIGGER IF EXISTS api_keys_set_updated_at ON api_keys;
    CREATE TRIGGER api_keys_set_updated_at
      BEFORE UPDATE ON api_keys
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;

-- ── org_memberships ───────────────────────────────────────────────────
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'org_memberships') THEN
    ALTER TABLE org_memberships ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
    UPDATE org_memberships SET updated_at = created_at WHERE updated_at IS NULL;
    ALTER TABLE org_memberships ALTER COLUMN updated_at SET NOT NULL;
    ALTER TABLE org_memberships ALTER COLUMN updated_at SET DEFAULT now();
    DROP TRIGGER IF EXISTS org_memberships_set_updated_at ON org_memberships;
    CREATE TRIGGER org_memberships_set_updated_at
      BEFORE UPDATE ON org_memberships
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;

-- ── templates ─────────────────────────────────────────────────────────
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'templates') THEN
    ALTER TABLE templates ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
    UPDATE templates SET updated_at = created_at WHERE updated_at IS NULL;
    ALTER TABLE templates ALTER COLUMN updated_at SET NOT NULL;
    ALTER TABLE templates ALTER COLUMN updated_at SET DEFAULT now();
    DROP TRIGGER IF EXISTS templates_set_updated_at ON templates;
    CREATE TRIGGER templates_set_updated_at
      BEFORE UPDATE ON templates
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;

-- ── sandbox_sessions ──────────────────────────────────────────────────
-- Sandbox sessions have started_at + stopped_at but no single "modified"
-- timestamp; for the backfill watermark, the most recent of the two is
-- the right historical anchor (a stopped sandbox last "changed" when it
-- stopped; a running one when it started).
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'sandbox_sessions') THEN
    ALTER TABLE sandbox_sessions ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
    UPDATE sandbox_sessions
      SET updated_at = COALESCE(stopped_at, started_at)
      WHERE updated_at IS NULL;
    ALTER TABLE sandbox_sessions ALTER COLUMN updated_at SET NOT NULL;
    ALTER TABLE sandbox_sessions ALTER COLUMN updated_at SET DEFAULT now();
    DROP TRIGGER IF EXISTS sandbox_sessions_set_updated_at ON sandbox_sessions;
    CREATE TRIGGER sandbox_sessions_set_updated_at
      BEFORE UPDATE ON sandbox_sessions
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;

-- ── sandbox_checkpoints ───────────────────────────────────────────────
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'sandbox_checkpoints') THEN
    ALTER TABLE sandbox_checkpoints ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
    UPDATE sandbox_checkpoints SET updated_at = created_at WHERE updated_at IS NULL;
    ALTER TABLE sandbox_checkpoints ALTER COLUMN updated_at SET NOT NULL;
    ALTER TABLE sandbox_checkpoints ALTER COLUMN updated_at SET DEFAULT now();
    DROP TRIGGER IF EXISTS sandbox_checkpoints_set_updated_at ON sandbox_checkpoints;
    CREATE TRIGGER sandbox_checkpoints_set_updated_at
      BEFORE UPDATE ON sandbox_checkpoints
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;

-- ── org_subscription_items ────────────────────────────────────────────
-- No created_at on this table — backfill existing rows to now() so they
-- get a sensible historical watermark. Subsequent UPDATEs flow through
-- the trigger.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'org_subscription_items') THEN
    ALTER TABLE org_subscription_items ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ;
    UPDATE org_subscription_items SET updated_at = now() WHERE updated_at IS NULL;
    ALTER TABLE org_subscription_items ALTER COLUMN updated_at SET NOT NULL;
    ALTER TABLE org_subscription_items ALTER COLUMN updated_at SET DEFAULT now();
    DROP TRIGGER IF EXISTS org_subscription_items_set_updated_at ON org_subscription_items;
    CREATE TRIGGER org_subscription_items_set_updated_at
      BEFORE UPDATE ON org_subscription_items
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;
