# CF Dev Cutover Runbook

Migration of the dev cluster (OPENSANDBOX-PROD, westus2) from the single
control-plane architecture to the distributed Cloudflare-fronted architecture.
**Dev is the dress rehearsal for prod.** Every step we run here, we replay
verbatim on prod once dev is stable.

The full architecture is in `markdown-preview.pdf` (the planning doc, 2026-04-28).
This runbook captures the dev-specific decisions, the week-by-week sequence,
and the migration risks we expect to bite.

## Settled decisions

| Item | Value |
| --- | --- |
| Cell ID | `azure-westus2-cell-a` |
| Regional CP hostname | `dev.opensandbox.ai` (unchanged, DNS-only proxy off) |
| Edge Worker hostname | `app.dev.opensandbox.ai` (after DNS) — interim: `opensandbox-edge-dev.brian-124.workers.dev` |
| Events ingest hostname | `events.dev.opensandbox.ai` (after DNS) — interim: `opensandbox-events-ingest-dev.brian-124.workers.dev` |
| CF Account ID | `1241f114453e32d292197e3fb36210b2` |
| CF Zone | `opensandbox.ai` (legacy; not prod-related, fair game for dev work) |
| CF Nameservers | `fred.ns.cloudflare.com`, `lovisa.ns.cloudflare.com` |
| Workers Plan | Paid (DOs require it) |
| WorkOS dev app | Reused from existing OPENSANDBOX-PROD |
| Stripe | Test mode keys (created when needed for Stage 3) |
| Free-tier seed | $5 / 500 cents on org create (matches existing PG default) |
| Upgrade promo credit | $30 applied as Stripe promotional credit (matches existing) |
| Auto-resume on upgrade | Yes (behavior change from "manual wake") |
| Secret store re-encrypt | Decrypt-and-re-encrypt during D1 backfill (option (a) — clean replay for prod) |
| Migration 019 on dev | Yes, with PG basebackup snapshot taken first |

**Prod analogue (later):** zone `opencomputer.dev`, edge `app.opencomputer.dev`,
events `events.opencomputer.workers.dev`, separate CF account.

## Operating principle

CF-parallel mode at all times during dev validation: PG remains authoritative
for credits and usage; D1/DO observe and validate. The CP's existing
`usage_reporter.go::deductFreeOrg()` keeps running. The DO's halt dispatch is
idempotent against the existing hibernate path (hibernating an already-
hibernated sandbox is a no-op). We only flip CF-authoritative on dev when
parity has been clean for 24h+.

## Sequence

### Week 1 — Infrastructure + Go scaffolding (no traffic)

- [ ] CF infra provisioned:
  - [ ] D1 database `opencomputer-dev` (`wrangler d1 create opencomputer-dev`)
  - [ ] D1 schema applied (`wrangler d1 execute opencomputer-dev --file cloudflare-workers/schema.sql`)
  - [ ] KV namespace `SESSIONS_KV` (`wrangler kv:namespace create sessions_kv`)
  - [ ] R2 bucket `events-archive-dev` (`wrangler r2 bucket create events-archive-dev`)
  - [ ] DNS: GoDaddy NS flipped to `fred.ns.cloudflare.com` / `lovisa.ns.cloudflare.com`
  - [ ] Existing GoDaddy `dev` A record (20.64.208.62) recreated in CF as `@` (dev.opensandbox.ai), DNS only
- [ ] Wrangler IDs filled into the two `wrangler.toml` files (replace `TODO_RUN_WRANGLER_*`)
- [ ] CF secrets set on api-edge: `SESSION_JWT_SECRET`, `CF_ADMIN_SECRET`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_API_KEY`, `WORKOS_API_KEY`, `WORKOS_CLIENT_ID`, `EVENT_SECRET`
- [ ] CF secrets set on events-ingest: `EVENT_SECRET`, `CF_ADMIN_SECRET`
- [ ] Go scaffolding implementations filled in (the stubs in this branch):
  - [ ] `internal/worker/redis_event_publisher.go` — replaces NATS publisher
  - [ ] `internal/controlplane/event_forwarder.go` + `cf_event_client.go`
  - [ ] `internal/controlplane/admin_handlers.go` (HMAC verify, halt/resume delegation)
  - [ ] `internal/controlplane/halt_reconciler.go`
  - [ ] `internal/auth/jwt_verifier.go`
  - [ ] `/internal/sandboxes/create` route on CP (cap-token verify)
- [ ] Workers deployed (stubs return 501/202 — no traffic):
  - [ ] `wrangler deploy` from `cloudflare-workers/api-edge`
  - [ ] `wrangler deploy` from `cloudflare-workers/events-ingest`
- [ ] Verify Workers reachable via `*.brian-124.workers.dev`

### Week 2 — Cloudflare Workers behavior

- [ ] events-ingest pipeline complete: HMAC verify, R2 archive, D1 inserts, KV dedup
- [ ] CreditAccount DO endpoints: `/check`, `/debit`, `/credit`, `/mark-pro`, `/snapshot`
- [ ] api-edge: WorkOS login + JWT mint; `POST /api/sandboxes` create proxy with cap-token; metadata GET with `cell_endpoint`; cross-cell list; 307 wildcard for dumb clients
- [ ] Local smoke test (`make run-full-server` pointed at deployed dev Workers)

### Week 3 — Backfill + cutover to CF-parallel mode

- [ ] `scripts/migrate_to_d1.py` written and dry-run against dev PG (counts must match)
- [ ] Backfill executes: `orgs`, `users`, `org_memberships`, `api_keys`, `templates`, `secret_stores`, `secret_store_entries` (decrypt-rekey), `sandboxes_index` (from active `sandbox_sessions`)
- [ ] CreditAccount DOs lazy-init verified (first `/check` reads from D1 `orgs.free_credits_remaining_cents`)
- [ ] Dev PG basebackup snapshot taken (point-of-no-return marker for week 5)
- [ ] Dev CP restarted with new env vars set:
  - `OPENSANDBOX_CELL_ID=azure-westus2-cell-a`
  - `OPENSANDBOX_CF_EVENT_ENDPOINT=https://opensandbox-events-ingest-dev.brian-124.workers.dev/ingest`
  - `OPENSANDBOX_CF_EVENT_SECRET=<EVENT_SECRET>`
  - `OPENSANDBOX_CF_ADMIN_SECRET=<CF_ADMIN_SECRET>`
  - `OPENSANDBOX_SESSION_JWT_SECRET=<SESSION_JWT_SECRET>`
  - `OPENSANDBOX_HALT_LIST_URL=https://opensandbox-edge-dev.brian-124.workers.dev/internal/halt-list`
- [ ] Dev workers restarted with same `CELL_ID`
- [ ] Events flowing: worker SQLite → Redis Stream → CP forwarder → CF events-ingest → D1 + R2
- [ ] Parity check cron live: every 15 min compare DO `balance_cents` vs PG `free_credits_remaining_cents` for free orgs, alert on drift > 1 tick

### Week 4 — Soak + acceptance suite

Run in order, single dev cell:

- [ ] Login via `app.dev.opensandbox.ai/auth/login` mints session JWT
- [ ] `oc sandbox create --endpoint app.dev.opensandbox.ai` creates a sandbox via the new path (cell selected, sandbox boots on dev cell)
- [ ] `oc sandbox exec` goes direct to regional CP, JWT verified locally, zero CF hops
- [ ] Existing dev API keys still create sandboxes via the legacy direct path
- [ ] Free-tier canary: drain $5, verify DO `/debit` runs to 0, halt webhook fires, sandbox hibernates within 90s
- [ ] Upgrade canary via Stripe test webhook → DO `/mark-pro` → resume webhook → sandbox wakes
- [ ] Pro-tier usage flows: events → D1 `usage_snapshots` → cron → Stripe meter events
- [ ] Halt reconciler safety net: kill the resume webhook mid-upgrade, verify reconciler's 60s pull-list catches the missed resume
- [ ] CP restart mid-batch: `XAUTOCLAIM` recovers messages within 60s, no duplicates at CF (KV dedup verified)
- [ ] Cross-cell listing works (single cell exercises the path)

### Week 5 — Run migration 019 on dev, smoke shrunk dev

- [ ] Second PG basebackup taken (separate from week 3's)
- [ ] Run `internal/db/migrations/019_strip_global_concerns.up.sql` on dev PG
- [ ] Verify dev CP comes up with shrunk schema (5 tables: `sandbox_sessions`, `sandbox_checkpoints`, `checkpoint_patches`, `sandbox_preview_urls`, `image_cache`)
- [ ] Re-run the entire week-4 acceptance suite — everything must still work
- [ ] Document any code paths that broke (these are the prod migration gotchas)
- [ ] Update this runbook with prod-replay deltas

## Migration risks (the ones expected to bite)

These are the failures I expect to surface only when running against real dev data.
**The whole point of doing this on dev is to find them before prod.**

1. **`sandbox_scale_events` is read by `usage_reporter.go`** to compute pro-tier usage.
   The plan drops it. Either move that logic to read events stream, or pro billing
   silently breaks post-migration. Hard-stop check before week 5.

2. **`workers` table is dropped** — confirm nothing in `internal/db/store.go` still
   reads it. Code should already go through Redis registry but the migration will catch it.

3. **`secret_store_entries` re-encryption** is one-shot and not easily reversible. Mismatched
   keys mean every sandbox using secrets fails to start. Test with a canary template that
   pulls a secret before believing the backfill.

4. **API key path during cutover**: dev SDK clients with cached API keys keep hitting the
   old direct-to-CP create endpoint. That path must keep working until we've manually
   migrated all dev clients. Plan for both paths to coexist for the entire dev soak.

5. **Free-tier balance drift during week 3 parallel mode**: PG decrements every 5 min via
   `usage_reporter.go`; DO decrements every 60s via events. They will not stay in lockstep —
   spec the "acceptable drift" up front: DO can be ahead by up to 5 min, never behind.

6. **`org_id` on the cap token** — capability tokens carry `org_id` from the api-edge's view.
   Regional CP trusts it. If WorkOS dev app's org IDs differ from dev PG `orgs.id`, you'll get
   cross-org auth failures. Confirm the WorkOS-org-id-to-D1-org-id mapping during backfill.

7. **`/api/sandboxes/{id}/preview-urls`** still hits CF Custom Hostnames API directly from CP.
   Dev CP needs to keep its CF API token. Do not accidentally remove it during the
   "remove WorkOS/Stripe creds from CP" cleanup.

## Rollback

- **Pre-week-5**: revert env vars on dev CP, redeploy, drop CF Workers — no data loss,
  events stop flowing, system returns to single-CP behavior.
- **Post-week-5 (after migration 019)**: restore from PG basebackup taken at the start
  of week 5. CF Workers stay deployed but go silent. Dev cluster is back to its
  pre-migration state.
- **Mid-soak parity drift**: pause the DO debit path (set `event.plan = "pro"` in events
  to bypass `/debit`), continue observing PG, fix the bug, resume parallel.

## What ships in this branch

This branch (`feat/cf-dev-cutover`) lands the **scaffolding**: configs, stub Go files
with type signatures, wrangler skeletons, D1 schema, this runbook. None of it changes
behavior — every new env var defaults to empty, which preserves current code paths.

Implementation lands in subsequent commits on the same branch, week by week.
