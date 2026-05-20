-- Reverse 041: drop the set_updated_at triggers added to the four tables.
-- Leaves set_updated_at() in place (owned by 040).

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'orgs') THEN
    DROP TRIGGER IF EXISTS orgs_set_updated_at ON orgs;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'secret_stores') THEN
    DROP TRIGGER IF EXISTS secret_stores_set_updated_at ON secret_stores;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'secret_store_entries') THEN
    DROP TRIGGER IF EXISTS secret_store_entries_set_updated_at ON secret_store_entries;
  END IF;
END $$;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'agent_subscriptions') THEN
    DROP TRIGGER IF EXISTS agent_subscriptions_set_updated_at ON agent_subscriptions;
  END IF;
END $$;
