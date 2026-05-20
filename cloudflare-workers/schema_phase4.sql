-- Phase 4: dashboard-on-edge data model.
--
-- The CF edge becomes the source of truth for identity, billing, and the
-- sandbox routing index (per the new architecture diagram). This migration
-- adds the missing columns and tables to D1 so api-edge dashboard handlers
-- can read/write directly instead of proxying to cell PG.
--
-- Apply with:
--   wrangler d1 execute opencomputer-dev --remote --file cloudflare-workers/schema_phase4.sql
--
-- SQLite/D1 has no `ALTER TABLE ADD COLUMN IF NOT EXISTS`, so these ALTERs
-- fail on re-run. Phase 4 is one-shot. If you need to re-apply, drop the
-- affected columns first or skip the ALTERs.

-- ── orgs: full feature parity with cell PG ───────────────────────────────
--
-- These mirror columns the cell PG `orgs` table has had since migrations
-- 005 / 016 / 029. Carrying them in D1 means dashboard can manage them
-- without touching any cell. The CF custom-domain handlers live on the
-- edge (it calls the CF API directly anyway), so this is the natural
-- place for the data.

ALTER TABLE orgs ADD COLUMN custom_domain TEXT;
ALTER TABLE orgs ADD COLUMN cf_hostname_id TEXT;
ALTER TABLE orgs ADD COLUMN domain_verification_status TEXT NOT NULL DEFAULT 'none';
ALTER TABLE orgs ADD COLUMN domain_ssl_status TEXT NOT NULL DEFAULT 'none';
ALTER TABLE orgs ADD COLUMN verification_txt_name TEXT;
ALTER TABLE orgs ADD COLUMN verification_txt_value TEXT;
ALTER TABLE orgs ADD COLUMN ssl_txt_name TEXT;
ALTER TABLE orgs ADD COLUMN ssl_txt_value TEXT;

-- Billing balances mirrored from CreditAccount DO. The DO is the live
-- authority; these columns let dashboard render balances without a DO
-- round-trip per page load. Updated on /debit, /credit, /mark-pro.
ALTER TABLE orgs ADD COLUMN free_credits_remaining_cents INTEGER NOT NULL DEFAULT 500;
ALTER TABLE orgs ADD COLUMN credit_balance_cents INTEGER NOT NULL DEFAULT 0;

-- Org quota controls (matches PG migration 029).
ALTER TABLE orgs ADD COLUMN max_concurrent_sandboxes INTEGER NOT NULL DEFAULT 5;
ALTER TABLE orgs ADD COLUMN max_sandbox_timeout_sec INTEGER NOT NULL DEFAULT 3600;
ALTER TABLE orgs ADD COLUMN max_disk_mb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE orgs ADD COLUMN max_memory_gb INTEGER NOT NULL DEFAULT 0;

-- Billing mode (matches PG migration 031): "legacy" = UsageReporter → Stripe;
-- "unified" = phase-3 billable_events sender. New orgs default to unified
-- in the dashboard create flow.
ALTER TABLE orgs ADD COLUMN billing_mode TEXT NOT NULL DEFAULT 'unified';

-- Last Stripe usage report timestamp (matches PG migration 016 area).
ALTER TABLE orgs ADD COLUMN last_usage_reported_at INTEGER NOT NULL DEFAULT 0;

-- ── invitations ─────────────────────────────────────────────────────────
--
-- Pending invitations to an org. WorkOS handles the email send and acceptance
-- handshake; we mirror state here so dashboard can list outstanding invites
-- and revoke them. Acceptance flow (WorkOS callback) updates D1 directly.

CREATE TABLE IF NOT EXISTS invitations (
  id           TEXT PRIMARY KEY,                    -- D1-local UUID
  org_id       TEXT NOT NULL,
  email        TEXT NOT NULL,
  role         TEXT NOT NULL DEFAULT 'member',      -- "owner" | "admin" | "member"
  invited_by   TEXT,                                 -- user_id of the inviter
  workos_invitation_id TEXT,                         -- WorkOS Invitation.id (null until WorkOS call succeeds)
  status       TEXT NOT NULL DEFAULT 'pending',     -- pending | accepted | revoked | expired
  token        TEXT UNIQUE,                          -- short opaque accept token (not currently used; WorkOS owns the flow)
  expires_at   INTEGER,                              -- unix s; null = no expiry
  created_at   INTEGER NOT NULL,
  accepted_at  INTEGER,
  revoked_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_invitations_org_status ON invitations(org_id, status);
CREATE INDEX IF NOT EXISTS idx_invitations_email ON invitations(email);

-- ── agent_subscriptions ─────────────────────────────────────────────────
--
-- Per-(org, agent, feature) subscription rows. Pro-tier orgs subscribe to
-- specific agent features (e.g., Telegram bot premium tools) and we ship
-- those events to Stripe via meters. Dashboard reads this to render
-- subscription state; mutations dispatch to Stripe + record here.

CREATE TABLE IF NOT EXISTS agent_subscriptions (
  id             TEXT PRIMARY KEY,                  -- D1-local UUID
  org_id         TEXT NOT NULL,
  agent_id       TEXT NOT NULL,
  feature        TEXT NOT NULL,                     -- "telegram", "premium-tools", etc.
  status         TEXT NOT NULL DEFAULT 'active',    -- active | cancelled
  stripe_item_id TEXT,                               -- Stripe Subscription Item ID
  created_at     INTEGER NOT NULL,
  cancelled_at   INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_subs_unique ON agent_subscriptions(org_id, agent_id, feature);
CREATE INDEX IF NOT EXISTS idx_agent_subs_org ON agent_subscriptions(org_id);
