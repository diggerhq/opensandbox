// CreditAccount — Durable Object per org tracking free-tier balance and halt
// status. One DO instance per org_id (deterministically routed via idFromName).
//
// Endpoints (all internal HTTP, called via DO stub fetch):
//   POST /check       → { allowed, balance_cents } — gate sandbox starts/wakes
//   POST /debit       → idempotent (event_id), decrements, dispatches halt at 0
//   POST /credit      → increments, dispatches resume if was halted_credits
//   POST /mark-pro    → flips plan to "pro", clears halt
//   POST /mark-free   → flips plan back to "free" (subscription cancelled)
//   GET  /snapshot    → full state, used by parity-check cron and tests
//
// State seeds lazily on first /check from D1 orgs.plan; the $5 free-tier
// seed is hardcoded in this file (single source of truth — the runbook says
// 500 cents and D1 doesn't carry a per-org seed column).
//
// Halt/resume dispatch: on balance <= 0, query D1 sandboxes_index for cells
// where the org has running/migrating sandboxes, HMAC-POST /admin/halt-org to
// each cell's CP base_url. On credit refill or mark-pro, mirror with /resume-org.
// 3x exponential retry; final-failure logged for the CP halt_reconciler to mop up.

interface DOState {
  org_id: string;
  plan: "free" | "pro";
  balance_cents: number;            // -1 sentinel for plan="pro" (untracked)
  lifetime_spent_cents: number;
  status: "active" | "halted_credits";
  halted_at?: number;
  last_debit_at?: number;
  processed_event_ids: string[];    // bounded LRU, last 1024 — idempotency for /debit
}

interface CellRow {
  cell_id: string;
  base_url: string;
}

interface Env {
  OPENCOMPUTER_DB: D1Database;
  CF_ADMIN_SECRET: string;
}

const FREE_TIER_SEED_CENTS = 500;          // $5 — runbook decision
const PROCESSED_LRU_MAX = 1024;
const HALT_RETRY_BACKOFFS_MS = [200, 1000, 5000];

export class CreditAccount {
  state: DurableObjectState;
  env: Env;

  constructor(state: DurableObjectState, env: Env) {
    this.state = state;
    this.env = env;
  }

  async fetch(req: Request): Promise<Response> {
    const url = new URL(req.url);
    const path = url.pathname;

    try {
      if (path === "/snapshot") return await this.snapshot();

      // org_id comes from the URL — caller is responsible for routing to the
      // right DO (idFromName(orgID)). Body carries event_id / amount / etc.
      const orgID = url.searchParams.get("org_id");
      if (!orgID) return json({ error: "missing org_id" }, 400);

      if (path === "/check"    && req.method === "POST") return await this.check(orgID);
      if (path === "/debit"    && req.method === "POST") return await this.debit(orgID, req);
      if (path === "/credit"   && req.method === "POST") return await this.credit(orgID, req);
      if (path === "/mark-pro" && req.method === "POST") return await this.markPro(orgID);
      if (path === "/mark-free"&& req.method === "POST") return await this.markFree(orgID);

      return json({ error: "not found" }, 404);
    } catch (err) {
      console.error("credit_account: handler error", err);
      return json({ error: (err as Error).message }, 500);
    }
  }

  // ── handlers ───────────────────────────────────────────────────────────

  private async check(orgID: string): Promise<Response> {
    const s = await this.loadOrInit(orgID);
    const allowed = s.plan === "pro" || s.balance_cents > 0;
    return json({ allowed, balance_cents: s.balance_cents, plan: s.plan, status: s.status });
  }

  private async debit(orgID: string, req: Request): Promise<Response> {
    const body = await req.json<{ event_id?: string; amount_cents?: number }>().catch(() => ({} as { event_id?: string; amount_cents?: number }));
    const eventID = body.event_id ?? "";
    const amount = Math.max(0, Math.floor(body.amount_cents ?? 0));
    if (!eventID) return json({ error: "missing event_id" }, 400);
    if (amount === 0) return json({ ok: true, deduped: false, balance_cents: -1 }, 200);

    const s = await this.loadOrInit(orgID);

    // Pro orgs: no-op. Per-event-id is still recorded to keep the LRU honest
    // in case the org flips back to free later.
    if (s.plan === "pro") {
      this.recordEvent(s, eventID);
      await this.persist(s);
      return json({ ok: true, deduped: false, balance_cents: -1, plan: "pro" });
    }

    if (s.processed_event_ids.includes(eventID)) {
      return json({ ok: true, deduped: true, balance_cents: s.balance_cents });
    }

    const prev = s.balance_cents;
    s.balance_cents = Math.max(0, prev - amount);
    s.lifetime_spent_cents += (prev - s.balance_cents);
    s.last_debit_at = Math.floor(Date.now() / 1000);
    this.recordEvent(s, eventID);

    let dispatchedHalt = false;
    if (s.balance_cents === 0 && s.status === "active") {
      s.status = "halted_credits";
      s.halted_at = Math.floor(Date.now() / 1000);
      await this.persist(s);
      await this.markD1Halted(orgID, true, s.halted_at);
      await this.mirrorBalanceToD1(orgID, 0);
      // Fire dispatch out-of-band — failure here is recoverable via the CP's
      // halt_reconciler safety net, so don't block the debit response on it.
      this.state.waitUntil(this.dispatchHalt(orgID));
      dispatchedHalt = true;
    } else {
      await this.persist(s);
      // Best-effort: mirror to D1 every N debits to keep dashboard reads
      // close to real-time without paying the write on every tick. waitUntil
      // keeps it async so it doesn't block the /debit ack to events-ingest.
      this.state.waitUntil(this.mirrorBalanceToD1(orgID, s.balance_cents));
    }

    return json({
      ok: true,
      deduped: false,
      balance_cents: s.balance_cents,
      halt_dispatched: dispatchedHalt,
    });
  }

  private async credit(orgID: string, req: Request): Promise<Response> {
    const body = await req.json<{ amount_cents?: number }>().catch(() => ({} as { amount_cents?: number }));
    const amount = Math.max(0, Math.floor(body.amount_cents ?? 0));
    if (amount === 0) return json({ error: "amount_cents must be > 0" }, 400);

    const s = await this.loadOrInit(orgID);

    if (s.plan === "pro") {
      // Crediting a pro org is meaningless (balance untracked) but not an error.
      return json({ ok: true, balance_cents: -1, plan: "pro" });
    }

    const wasHalted = s.status === "halted_credits";
    s.balance_cents = (s.balance_cents < 0 ? 0 : s.balance_cents) + amount;
    if (wasHalted && s.balance_cents > 0) {
      s.status = "active";
      s.halted_at = undefined;
    }
    await this.persist(s);

    let dispatchedResume = false;
    if (wasHalted && s.balance_cents > 0) {
      await this.markD1Halted(orgID, false, 0);
      this.state.waitUntil(this.dispatchResume(orgID));
      dispatchedResume = true;
    }
    this.state.waitUntil(this.mirrorBalanceToD1(orgID, s.balance_cents));
    return json({ ok: true, balance_cents: s.balance_cents, resume_dispatched: dispatchedResume });
  }

  private async markPro(orgID: string): Promise<Response> {
    const s = await this.loadOrInit(orgID);
    const wasHalted = s.status === "halted_credits";
    s.plan = "pro";
    s.balance_cents = -1;
    s.status = "active";
    s.halted_at = undefined;
    await this.persist(s);

    // Mirror to D1 so any cron / future-cell that looks at orgs.plan sees the truth.
    await this.env.OPENCOMPUTER_DB.prepare(
      `UPDATE orgs SET plan = 'pro', is_halted = 0, halted_at = NULL, updated_at = ?1 WHERE id = ?2`,
    )
      .bind(Math.floor(Date.now() / 1000), orgID)
      .run();

    // If the org was previously halted on free-tier, wake their sandboxes.
    let dispatchedResume = false;
    if (wasHalted) {
      this.state.waitUntil(this.dispatchResume(orgID));
      dispatchedResume = true;
    }
    return json({ ok: true, plan: "pro", resume_dispatched: dispatchedResume });
  }

  private async markFree(orgID: string): Promise<Response> {
    // Subscription cancellation. Don't immediately halt — flip to free with
    // whatever balance they have. If balance is 0, the next debit (or the
    // halt_reconciler's next pass) will trigger halt. Avoids halting mid-
    // billing-cycle just because Stripe sent a "subscription.deleted" event.
    const s = await this.loadOrInit(orgID);
    s.plan = "free";
    // Keep balance_cents as-is, but if it's the -1 sentinel from pro, reset.
    if (s.balance_cents < 0) s.balance_cents = 0;
    await this.persist(s);
    await this.env.OPENCOMPUTER_DB.prepare(
      `UPDATE orgs SET plan = 'free', updated_at = ?1 WHERE id = ?2`,
    )
      .bind(Math.floor(Date.now() / 1000), orgID)
      .run();
    return json({ ok: true, plan: "free", balance_cents: s.balance_cents });
  }

  private async snapshot(): Promise<Response> {
    const data = await this.state.storage.list();
    return json(Object.fromEntries(data));
  }

  // ── state helpers ──────────────────────────────────────────────────────

  // loadOrInit reads the DO's persisted state if present, otherwise seeds
  // it from D1 orgs.plan (free → 500¢ seed; pro → -1 sentinel). The seed
  // is durable after the first /check or /debit because we persist right away.
  private async loadOrInit(orgID: string): Promise<DOState> {
    const existing = await this.state.storage.get<DOState>("state");
    if (existing) return existing;

    const row = await this.env.OPENCOMPUTER_DB.prepare(
      `SELECT plan FROM orgs WHERE id = ?1`,
    )
      .bind(orgID)
      .first<{ plan: string }>();
    const plan: "free" | "pro" = row?.plan === "pro" ? "pro" : "free";
    const seed: DOState = {
      org_id: orgID,
      plan,
      balance_cents: plan === "pro" ? -1 : FREE_TIER_SEED_CENTS,
      lifetime_spent_cents: 0,
      status: "active",
      processed_event_ids: [],
    };
    await this.persist(seed);
    return seed;
  }

  private async persist(s: DOState): Promise<void> {
    await this.state.storage.put("state", s);
  }

  private recordEvent(s: DOState, eventID: string): void {
    s.processed_event_ids.push(eventID);
    if (s.processed_event_ids.length > PROCESSED_LRU_MAX) {
      s.processed_event_ids = s.processed_event_ids.slice(-PROCESSED_LRU_MAX);
    }
  }

  private async markD1Halted(orgID: string, halted: boolean, haltedAt: number): Promise<void> {
    try {
      await this.env.OPENCOMPUTER_DB.prepare(
        `UPDATE orgs SET is_halted = ?1, halted_at = ?2, updated_at = ?3 WHERE id = ?4`,
      )
        .bind(halted ? 1 : 0, halted ? haltedAt : null, Math.floor(Date.now() / 1000), orgID)
        .run();
    } catch (err) {
      // D1 mirror is best-effort — the DO state is authoritative. If this
      // fails, halt-list endpoint just won't include the org until the next
      // /debit retries the write, but the actual halt webhook still goes
      // out via dispatchHalt below.
      console.error("credit_account: failed to mirror halt state to D1", err);
    }
  }

  // mirrorBalanceToD1 writes the current free-tier balance to orgs.free_credits_remaining_cents
  // so dashboard reads (/api/dashboard/billing) see the live value without round-tripping
  // through the DO on every page load. DO state remains authoritative — this column is the
  // projection consumed by the cached read path.
  private async mirrorBalanceToD1(orgID: string, balanceCents: number): Promise<void> {
    try {
      await this.env.OPENCOMPUTER_DB.prepare(
        `UPDATE orgs SET free_credits_remaining_cents = ?1, updated_at = ?2 WHERE id = ?3`,
      )
        .bind(balanceCents, Math.floor(Date.now() / 1000), orgID)
        .run();
    } catch (err) {
      console.error("credit_account: failed to mirror balance to D1", err);
    }
  }

  // ── dispatch ───────────────────────────────────────────────────────────

  // Resolve the cells where the org has running/migrating sandboxes, then
  // HMAC-POST /admin/halt-org to each cell's CP. Retries up to 3x with
  // exponential backoff per cell; the CP halt_reconciler is the safety net
  // for terminal failures.
  private async dispatchHalt(orgID: string): Promise<void> {
    const cells = await this.cellsForOrg(orgID);
    if (cells.length === 0) return;
    const body = JSON.stringify({ org_id: orgID, reason: "credits_exhausted" });
    await Promise.all(cells.map((c) => this.postWithRetry(c, "/admin/halt-org", body)));
  }

  private async dispatchResume(orgID: string): Promise<void> {
    const cells = await this.cellsForOrg(orgID);
    if (cells.length === 0) return;
    const body = JSON.stringify({ org_id: orgID, skip_resume: false });
    await Promise.all(cells.map((c) => this.postWithRetry(c, "/admin/resume-org", body)));
  }

  // Distinct cells where this org has any non-stopped sandbox. We dispatch
  // halt to all of them so cross-cell-distributed orgs get consistent
  // treatment in one shot.
  private async cellsForOrg(orgID: string): Promise<CellRow[]> {
    const { results } = await this.env.OPENCOMPUTER_DB.prepare(
      `SELECT DISTINCT c.cell_id, c.base_url
         FROM sandboxes_index s
         JOIN cells c ON s.cell_id = c.cell_id
        WHERE s.org_id = ?1 AND s.status IN ('running', 'hibernated', 'migrating')`,
    )
      .bind(orgID)
      .all<CellRow>();
    return results ?? [];
  }

  private async postWithRetry(cell: CellRow, path: string, body: string): Promise<void> {
    const ts = Math.floor(Date.now() / 1000).toString();
    const sig = await hmacHex(this.env.CF_ADMIN_SECRET, `${ts}.${body}`);
    const url = cell.base_url.replace(/\/$/, "") + path;

    for (let attempt = 0; attempt <= HALT_RETRY_BACKOFFS_MS.length; attempt++) {
      try {
        const resp = await fetch(url, {
          method: "POST",
          headers: {
            "content-type": "application/json",
            "X-Timestamp": ts,
            "X-Signature": sig,
          },
          body,
        });
        if (resp.status >= 200 && resp.status < 300) return;
        console.error(`credit_account: dispatch ${path} → ${cell.cell_id} status=${resp.status} attempt=${attempt}`);
      } catch (err) {
        console.error(`credit_account: dispatch ${path} → ${cell.cell_id} error=${(err as Error).message} attempt=${attempt}`);
      }
      if (attempt < HALT_RETRY_BACKOFFS_MS.length) {
        await sleep(HALT_RETRY_BACKOFFS_MS[attempt]);
      }
    }
    console.error(`credit_account: dispatch ${path} → ${cell.cell_id} permanently failed — relying on CP halt_reconciler safety net`);
  }
}

// ── module-local helpers (not exported) ──────────────────────────────────

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

async function hmacHex(secret: string, data: string): Promise<string> {
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(data));
  return [...new Uint8Array(sig)].map((b) => b.toString(16).padStart(2, "0")).join("");
}
