-- Reverse 042: drop the per-table triggers, the function, and the outbox table.
DO $$
DECLARE
  t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'orgs','users','api_keys','templates','secret_stores',
    'secret_store_entries','agent_subscriptions','org_subscription_items'
  ] LOOP
    IF EXISTS (SELECT 1 FROM information_schema.tables
               WHERE table_schema = 'public' AND table_name = t) THEN
      EXECUTE format('DROP TRIGGER IF EXISTS %I_global_sync ON %I', t, t);
    END IF;
  END LOOP;
END $$;

DROP FUNCTION IF EXISTS enqueue_global_sync();
DROP TABLE IF EXISTS global_sync_outbox;
