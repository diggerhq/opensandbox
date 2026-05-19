-- Strip global concerns from per-cell PG. Identity (orgs/users/api_keys),
-- templates, and secret stores now live in D1 (the edge layer). The CP is a
-- stateless executor: it receives a capability token with org_id + plan, and
-- fetches templates + secret-store entries via HMAC'd /internal/* endpoints
-- on the edge at sandbox-create time.
--
-- This migration drops:
--   - Identity:  orgs, users, api_keys, user_sessions
--   - Templates: templates
--   - Secrets:   secret_stores, secret_store_entries
--   - Stripe:    org_subscription_items, usage_snapshots (D1 owns these now)
--   - Agents:    agent_subscriptions (depends on orgs)
--
-- Tables that survive (cell-local concerns):
--   - sandbox_sessions, sandbox_checkpoints, image_cache, preview_urls,
--     workers, snapshots, image_patches, migration_state, …
--
-- FK constraints from cell-local tables that pointed at the global tables
-- are auto-dropped by `DROP TABLE ... CASCADE` — the org_id/user_id columns
-- themselves are preserved as free-text UUID tags (no longer enforced).

BEGIN;

-- Wipe local copies of global tables. CASCADE drops dependent FK constraints
-- on cell-local tables without dropping their columns.
DROP TABLE IF EXISTS user_sessions          CASCADE;
DROP TABLE IF EXISTS api_keys               CASCADE;
DROP TABLE IF EXISTS agent_subscriptions    CASCADE;
DROP TABLE IF EXISTS secret_store_entries   CASCADE;
DROP TABLE IF EXISTS secret_stores          CASCADE;
DROP TABLE IF EXISTS templates              CASCADE;
DROP TABLE IF EXISTS org_subscription_items CASCADE;
DROP TABLE IF EXISTS usage_snapshots        CASCADE;
DROP TABLE IF EXISTS users                  CASCADE;
DROP TABLE IF EXISTS orgs                   CASCADE;

COMMIT;
