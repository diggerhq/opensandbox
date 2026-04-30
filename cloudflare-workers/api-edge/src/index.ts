// api-edge Worker — global API entry point.
//
// All routes are placeholders — implementation lands in subsequent commits.
// Until then this Worker rejects everything with 501 Not Implemented except
// /health (so wrangler deploy succeeds and connectivity can be verified).

export { CreditAccount } from "../../shared/credit_account";

export interface Env {
  OPENCOMPUTER_DB: D1Database;
  SESSIONS_KV: KVNamespace;
  CREDIT_ACCOUNT: DurableObjectNamespace;
  SESSION_JWT_SECRET: string;
  CF_ADMIN_SECRET: string;
  STRIPE_WEBHOOK_SECRET: string;
  STRIPE_API_KEY: string;
  WORKOS_API_KEY: string;
  WORKOS_CLIENT_ID: string;
  EVENT_SECRET: string;
  WORKER_ENV: string;
  CELLS: string;
}

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const url = new URL(req.url);

    if (url.pathname === "/health") {
      return new Response(
        JSON.stringify({ ok: true, env: env.WORKER_ENV, cells: env.CELLS.split(",") }),
        { headers: { "content-type": "application/json" } },
      );
    }

    // TODO routes (in this order, see docs/dev-cutover-runbook.md):
    //   POST /auth/login
    //   POST /auth/refresh
    //   POST /auth/logout
    //   POST /webhooks/stripe
    //   POST /api/sandboxes               — auth, /check on DO, cell select, cap-mint, proxy
    //   GET  /api/sandboxes               — D1 sandboxes_index
    //   GET  /api/sandboxes/{id}          — metadata + cell_endpoint
    //   ANY  /api/sandboxes/{id}/*        — 307 to {cell_id}.app.opensandbox.ai
    //   GET  /internal/halt-list          — HMAC, called by CP halt_reconciler

    return new Response("not implemented", { status: 501 });
  },
} satisfies ExportedHandler<Env>;
