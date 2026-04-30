// CreditAccount — Durable Object per org tracking free-tier balance.
// Imported by both api-edge and events-ingest Workers.
//
// Endpoints (all internal HTTP):
//   POST /check       → { allowed, balance_cents }
//   POST /debit       → idempotent (event_id), decrements balance, dispatches halt at zero
//   POST /credit      → increments balance, may trigger resume
//   POST /mark-pro    → flips to pro plan, optionally dispatches resume
//   GET  /snapshot    → full state, used by parity-check cron
//
// State seeds lazily on first /check from D1 orgs.plan + free_credits_remaining_cents
// (so we don't hard-code $5 in two places).

interface State {
  org_id: string;
  plan: "free" | "pro";
  balance_cents: number; // -1 when plan == "pro" (no longer tracked)
  lifetime_spent_cents: number;
  status: "active" | "halted_credits" | "suspended";
  halted_at?: number;
  last_debit_at?: number;
  processed_event_ids: string[]; // bounded LRU, last 1024
}

interface Env {
  OPENCOMPUTER_DB: D1Database;
  CF_ADMIN_SECRET: string;
}

export class CreditAccount {
  state: DurableObjectState;
  env: Env;

  constructor(state: DurableObjectState, env: Env) {
    this.state = state;
    this.env = env;
  }

  async fetch(req: Request): Promise<Response> {
    const url = new URL(req.url);

    // TODO Stage 2:
    //   /check    → load-or-init from D1 if storage empty, return {allowed, balance_cents}
    //   /debit    → check processed_event_ids LRU, decrement, dispatch halt at <=0
    //   /credit   → increment, dispatch resume if status was halted_credits and balance > 0
    //   /mark-pro → set plan="pro", balance_cents=-1, optionally dispatchResume
    //   /snapshot → return full State

    if (url.pathname === "/snapshot") {
      const data = await this.state.storage.list();
      return new Response(JSON.stringify(Object.fromEntries(data)), {
        headers: { "content-type": "application/json" },
      });
    }

    return new Response("not implemented", { status: 501 });
  }

  // dispatchHalt queries D1 sandboxes_index for cells where the org has
  // any running/migrating sandboxes, then HMAC-POSTs /admin/halt-org to
  // each cell's regional CP. Retries 3x exponential. Logs and falls back
  // to the CP halt_reconciler safety net on persistent failure.
  // private async dispatchHalt(orgId: string): Promise<void> { ... }

  // dispatchResume mirrors dispatchHalt for /admin/resume-org.
  // private async dispatchResume(orgId: string): Promise<void> { ... }
}
