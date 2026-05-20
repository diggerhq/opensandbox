-- Reverse 040_add_updated_at: drop triggers, columns, and the shared function.
-- Wrapped per-table in IF EXISTS so partial-applied envs reverse cleanly.

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'users') THEN
    DROP TRIGGER IF EXISTS users_set_updated_at ON users;
    ALTER TABLE users DROP COLUMN IF EXISTS updated_at;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'api_keys') THEN
    DROP TRIGGER IF EXISTS api_keys_set_updated_at ON api_keys;
    ALTER TABLE api_keys DROP COLUMN IF EXISTS updated_at;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'org_memberships') THEN
    DROP TRIGGER IF EXISTS org_memberships_set_updated_at ON org_memberships;
    ALTER TABLE org_memberships DROP COLUMN IF EXISTS updated_at;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'templates') THEN
    DROP TRIGGER IF EXISTS templates_set_updated_at ON templates;
    ALTER TABLE templates DROP COLUMN IF EXISTS updated_at;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'sandbox_sessions') THEN
    DROP TRIGGER IF EXISTS sandbox_sessions_set_updated_at ON sandbox_sessions;
    ALTER TABLE sandbox_sessions DROP COLUMN IF EXISTS updated_at;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'sandbox_checkpoints') THEN
    DROP TRIGGER IF EXISTS sandbox_checkpoints_set_updated_at ON sandbox_checkpoints;
    ALTER TABLE sandbox_checkpoints DROP COLUMN IF EXISTS updated_at;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'org_subscription_items') THEN
    DROP TRIGGER IF EXISTS org_subscription_items_set_updated_at ON org_subscription_items;
    ALTER TABLE org_subscription_items DROP COLUMN IF EXISTS updated_at;
  END IF;
END $$;

DROP FUNCTION IF EXISTS set_updated_at();
