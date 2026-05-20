-- Add set_updated_at triggers to the source tables that already had an
-- updated_at column before migration 040 and so were skipped by it:
--   orgs, secret_stores, secret_store_entries, agent_subscriptions
--
-- Why this is needed: the PG → D1 cutover delta sync uses updated_at as its
-- watermark across every source table. Migration 040 added a BEFORE UPDATE
-- trigger to the tables that lacked the column, but skipped these four because
-- they already had it. Their updated_at is therefore only bumped by app code
-- that explicitly sets it — and at least one production path does NOT:
--   internal/db/usage.go — `UPDATE orgs SET last_usage_reported_at = $2 ...`
-- (the usage reporter, every billing tick). That column IS mirrored into D1,
-- so without a trigger it goes stale and delta sync silently misses the update.
-- A trigger makes the watermark reliable on every write path, present and
-- future. Setting now() in the trigger is harmless alongside app code that
-- also sets it.
--
-- Wrapped per-table in IF EXISTS so it's safe across schema-drifted fleets.

-- set_updated_at() is created by 040; CREATE OR REPLACE here too so this
-- migration is self-contained and idempotent.
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'orgs') THEN
    DROP TRIGGER IF EXISTS orgs_set_updated_at ON orgs;
    CREATE TRIGGER orgs_set_updated_at
      BEFORE UPDATE ON orgs
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'secret_stores') THEN
    DROP TRIGGER IF EXISTS secret_stores_set_updated_at ON secret_stores;
    CREATE TRIGGER secret_stores_set_updated_at
      BEFORE UPDATE ON secret_stores
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'secret_store_entries') THEN
    DROP TRIGGER IF EXISTS secret_store_entries_set_updated_at ON secret_store_entries;
    CREATE TRIGGER secret_store_entries_set_updated_at
      BEFORE UPDATE ON secret_store_entries
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'agent_subscriptions') THEN
    DROP TRIGGER IF EXISTS agent_subscriptions_set_updated_at ON agent_subscriptions;
    CREATE TRIGGER agent_subscriptions_set_updated_at
      BEFORE UPDATE ON agent_subscriptions
      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
  END IF;
END $$;
