-- Seed data for the dev cell PG (opensandbox-prod RG, westus2) so the PG → D1
-- backfill test exercises every table and every code path.
--
-- Dev's schema already matches prod (migration 40); this only adds DATA. It is
-- idempotent (ON CONFLICT DO NOTHING) and all rows use recognizable seed UUIDs
-- (aaaaaaaa-… orgs, bbbbbbbb-… users, etc.) + 'seed-' names, so re-running is a
-- no-op and teardown is trivial (see bottom of file).
--
-- Coverage notes (why the CASE/MOD variation exists):
--   * orgs            — free/pro, legacy/unified billing, with/without custom_domain + stripe ids
--   * users           — owner/admin/member roles (drives org_memberships derivation)
--   * api_keys        — single vs multi scope, with/without last_used + expires_at
--   * templates       — org-owned vs public (NULL org_id), ready vs building
--   * agent_subs      — active vs canceled (tests canceled_at→cancelled_at mapping)
--   * org_sub_items   — varied memory_mb (tests memory_mb→tier mapping)
--   * sandbox_sessions— running/hibernated (indexed) + stopped/error (skipped); some NULL golden_version
--   * sandbox_checkpts— ready vs processing (skipped), some NULL rootfs (skipped),
--                       some on a sandbox with NULL golden_version (skipped) — exercises all 3 skip branches

-- orgs ----------------------------------------------------------------------
INSERT INTO orgs (id, name, slug, plan, billing_mode, custom_domain,
                  stripe_customer_id, stripe_subscription_id, is_personal,
                  credit_balance_cents, free_credits_remaining_cents)
SELECT ('aaaaaaaa-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       'Seed Org '||i,
       'seed-org-'||lpad(i::text,2,'0'),
       CASE WHEN i%2=0 THEN 'pro' ELSE 'free' END,
       CASE WHEN i%2=0 THEN 'unified' ELSE 'legacy' END,
       CASE WHEN i%3=0 THEN 'seed'||i||'.example.com' ELSE NULL END,
       CASE WHEN i%2=0 THEN 'cus_seed'||i ELSE NULL END,
       CASE WHEN i%2=0 THEN 'sub_seed'||i ELSE NULL END,
       (i%4=0),
       1000*i,
       500
FROM generate_series(1,10) i
ON CONFLICT (id) DO NOTHING;

-- users (one per seed org; varied roles) ------------------------------------
INSERT INTO users (id, org_id, email, name, role, workos_user_id)
SELECT ('bbbbbbbb-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       ('aaaaaaaa-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       'seed-user-'||lpad(i::text,2,'0')||'@example.com',
       'Seed User '||i,
       CASE WHEN i%3=0 THEN 'owner' WHEN i%3=1 THEN 'admin' ELSE 'member' END,
       CASE WHEN i%2=0 THEN 'wos_user_seed'||i ELSE NULL END
FROM generate_series(1,10) i
ON CONFLICT (id) DO NOTHING;

-- api_keys ------------------------------------------------------------------
INSERT INTO api_keys (id, org_id, created_by, key_hash, key_prefix, name,
                      scopes, last_used, expires_at)
SELECT ('cccccccc-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       ('aaaaaaaa-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       ('bbbbbbbb-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       'seedhash_'||i,
       'sk_seed'||i,
       'Seed Key '||i,
       CASE WHEN i%2=0 THEN ARRAY['sandbox:*','templates:read'] ELSE ARRAY['sandbox:*'] END,
       CASE WHEN i%2=0 THEN now()-(i||' days')::interval ELSE NULL END,
       CASE WHEN i%3=0 THEN now()+(30||' days')::interval ELSE NULL END
FROM generate_series(1,10) i
ON CONFLICT (id) DO NOTHING;

-- templates (org-owned + public) --------------------------------------------
INSERT INTO templates (id, org_id, name, tag, template_type, image_ref,
                       is_public, status, rootfs_s3_key, workspace_s3_key)
SELECT ('dddddddd-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       CASE WHEN i%4=0 THEN NULL ELSE ('aaaaaaaa-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid END,
       'seed-template-'||i,
       'latest',
       CASE WHEN i%2=0 THEN 'dockerfile' ELSE 'image' END,
       'registry/seed:'||i,
       (i%4=0),
       CASE WHEN i%5=0 THEN 'building' ELSE 'ready' END,
       'templates/seed/'||i||'/rootfs.ext4',
       'templates/seed/'||i||'/workspace.ext4'
FROM generate_series(1,10) i
ON CONFLICT (id) DO NOTHING;

-- secret_stores -------------------------------------------------------------
INSERT INTO secret_stores (id, org_id, name, egress_allowlist)
SELECT ('eeeeeeee-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       ('aaaaaaaa-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       'seed-store-'||i,
       CASE WHEN i%2=0 THEN ARRAY['api.openai.com','*.github.com'] ELSE ARRAY[]::text[] END
FROM generate_series(1,10) i
ON CONFLICT (id) DO NOTHING;

-- secret_store_entries are NOT seeded here. The backfill decrypts each entry
-- with the cell keyring and re-encrypts for the edge, so they must be REAL
-- keyring-format ciphertext (dummy bytes can't be decrypted). They're seeded
-- with known plaintexts by `cmd/seed-dev-secrets` (run on the CP, which has the
-- key), so the edge-decrypt verification can assert the round-trip.

-- agent_subscriptions (active + canceled) -----------------------------------
INSERT INTO agent_subscriptions (id, org_id, agent_id, feature, stripe_customer_id,
                                 stripe_subscription_id, stripe_price_id, status,
                                 current_period_end, cancel_at_period_end, canceled_at)
SELECT ('1a1a1a1a-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       ('aaaaaaaa-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       'agent-'||i,
       CASE WHEN i%2=0 THEN 'telegram' ELSE 'premium-tools' END,
       'cus_seed'||i,
       'sub_seed'||i,
       'price_seed'||i,
       CASE WHEN i%3=0 THEN 'canceled' ELSE 'active' END,
       now()+(30||' days')::interval,
       (i%3=0),
       CASE WHEN i%3=0 THEN now()-(i||' days')::interval ELSE NULL END
FROM generate_series(1,10) i
ON CONFLICT (id) DO NOTHING;

-- org_subscription_items (varied memory tiers) ------------------------------
INSERT INTO org_subscription_items (org_id, memory_mb, stripe_subscription_item_id)
SELECT ('aaaaaaaa-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       512*i,
       'si_seed'||i
FROM generate_series(1,10) i
ON CONFLICT (org_id, memory_mb) DO NOTHING;

-- sandbox_sessions (all statuses; some NULL golden_version) ------------------
INSERT INTO sandbox_sessions (id, sandbox_id, org_id, user_id, template, region,
                              worker_id, status, based_on_template_id, golden_version,
                              started_at, stopped_at)
SELECT ('2a2a2a2a-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       'seed-sb-'||lpad(i::text,2,'0'),
       ('aaaaaaaa-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       CASE WHEN i%2=0 THEN ('bbbbbbbb-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid ELSE NULL END,
       'seed-template-'||i,
       'westus2',
       'seed-worker-'||(i%3),
       CASE WHEN i%4=0 THEN 'stopped' WHEN i%4=1 THEN 'running'
            WHEN i%4=2 THEN 'hibernated' ELSE 'error' END,
       CASE WHEN i%4=0 THEN NULL ELSE ('dddddddd-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid END,
       CASE WHEN i%5=0 THEN NULL ELSE 'golden-'||(i%3) END,
       now()-(i||' hours')::interval,
       CASE WHEN i%4 IN (0,3) THEN now()-((i-1)||' hours')::interval ELSE NULL END
FROM generate_series(1,10) i
ON CONFLICT (id) DO NOTHING;

-- sandbox_checkpoints (ready/processing; NULL rootfs; NULL-golden sandboxes) -
INSERT INTO sandbox_checkpoints (id, sandbox_id, org_id, name, rootfs_s3_key,
                                 workspace_s3_key, status, size_bytes)
SELECT ('3a3a3a3a-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       'seed-sb-'||lpad(i::text,2,'0'),
       ('aaaaaaaa-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,
       'seed-ckpt-'||i,
       CASE WHEN i%6=0 THEN NULL
            ELSE 'checkpoints/seed-sb-'||lpad(i::text,2,'0')||'/ckpt'||i||'/rootfs.tar.zst' END,
       'checkpoints/seed-sb-'||lpad(i::text,2,'0')||'/ckpt'||i||'/workspace.tar.zst',
       CASE WHEN i%3=0 THEN 'processing' ELSE 'ready' END,
       1024*1024*i
FROM generate_series(1,10) i
ON CONFLICT (id) DO NOTHING;

-- Teardown (run to remove all seed data):
--   DELETE FROM sandbox_checkpoints WHERE id::text LIKE '3a3a3a3a-%';
--   DELETE FROM sandbox_sessions    WHERE id::text LIKE '2a2a2a2a-%';
--   DELETE FROM org_subscription_items WHERE stripe_subscription_item_id LIKE 'si_seed%';
--   DELETE FROM agent_subscriptions WHERE id::text LIKE '1a1a1a1a-%';
--   DELETE FROM secret_store_entries WHERE id::text LIKE 'ffffffff-%';
--   DELETE FROM secret_stores       WHERE id::text LIKE 'eeeeeeee-%';
--   DELETE FROM templates           WHERE id::text LIKE 'dddddddd-%';
--   DELETE FROM api_keys            WHERE id::text LIKE 'cccccccc-%';
--   DELETE FROM users               WHERE id::text LIKE 'bbbbbbbb-%';
--   DELETE FROM orgs                WHERE id::text LIKE 'aaaaaaaa-%';
