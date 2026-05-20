-- DELETE-capture outbox for the PG → D1 cutover sync (Option C).
--
-- An AFTER DELETE trigger on each of the 8 global tables enqueues a
-- (table, op, row_id) row into global_sync_outbox IN THE SAME TRANSACTION as
-- the delete. The sync daemon (backfill --mode=daemon) drains the outbox and
-- removes the corresponding D1 rows, then prunes drained rows so the table
-- stays small.
--
-- Why only DELETE: INSERT/UPDATE are already caught reliably by the updated_at
-- watermark (every global table got an updated_at trigger via migrations 040 +
-- 041), so the daemon upserts those via a watermarked delta. The one thing a
-- watermark can't see is a DELETE — the row is gone, with no updated_at to
-- detect. This outbox fills exactly that gap, at the DB level so no write path
-- (request handlers, usage reporter, Stripe webhooks, raw SQL) can bypass it.
--
-- org_memberships is derived from users (no PG table), so the users trigger
-- drives it. sandboxes_index / checkpoints_index are NOT here — they are owned
-- by the worker event stream via events-ingest.

CREATE TABLE IF NOT EXISTS global_sync_outbox (
  seq         BIGSERIAL PRIMARY KEY,
  table_name  TEXT NOT NULL,
  op          TEXT NOT NULL,                 -- INSERT | UPDATE | DELETE
  row_id      TEXT NOT NULL,                 -- id, or "org_id/memory_mb" for org_subscription_items
  enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Trigger function: enqueue the changed row's key. Uses to_jsonb so it compiles
-- regardless of the table's columns; branches for the one composite-PK table.
CREATE OR REPLACE FUNCTION enqueue_global_sync()
RETURNS TRIGGER AS $$
DECLARE
  rec jsonb;
  key text;
BEGIN
  IF TG_OP = 'DELETE' THEN
    rec := to_jsonb(OLD);
  ELSE
    rec := to_jsonb(NEW);
  END IF;

  IF TG_TABLE_NAME = 'org_subscription_items' THEN
    key := (rec->>'org_id') || '/' || (rec->>'memory_mb');
  ELSE
    key := rec->>'id';
  END IF;

  INSERT INTO global_sync_outbox (table_name, op, row_id)
    VALUES (TG_TABLE_NAME, TG_OP, key);
  RETURN NULL; -- AFTER trigger; return value ignored
END;
$$ LANGUAGE plpgsql;

-- Attach to each global table (defensive: only if the table exists).
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
      EXECUTE format(
        'CREATE TRIGGER %I_global_sync AFTER DELETE ON %I '
        || 'FOR EACH ROW EXECUTE FUNCTION enqueue_global_sync()', t, t);
    END IF;
  END LOOP;
END $$;
