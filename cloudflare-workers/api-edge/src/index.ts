// api-edge Worker — global API entry point.
//
// Implemented so far (cross-cell test path):
//   POST /api/sandboxes        — auth (D1 api_keys) → pick org.home_cell →
//                                mint capability token → proxy to that cell's
//                                CP /internal/sandboxes/create → record in
//                                sandboxes_index → return the CP's response
//   GET  /api/sandboxes        — list this org's sandboxes from sandboxes_index
//   GET  /api/sandboxes/:id    — one row + cell_endpoint
//   ANY  /api/sandboxes/:id/*  — 307 to the owning cell's CP (dumb-client path)
//   GET  /health
//
// Still 501 (not on the cross-cell test path): /auth/*, /webhooks/stripe,
// /internal/halt-list. See docs/dev-cutover-runbook.md.

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

// ── small helpers ────────────────────────────────────────────────────────

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const b64url = (buf: ArrayBuffer | Uint8Array): string => {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
};

async function sha256Hex(s: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

// Mint the capability token the regional CP expects on /internal/sandboxes/create:
// HS256 JWT signed with SESSION_JWT_SECRET, iss="opensandbox-edge", carrying
// org_id + cell_id (+ optional user_id). Mirrors auth.CapabilityClaims in Go.
async function mintCapToken(
  secret: string,
  orgID: string,
  cellID: string,
  userID: string | null,
): Promise<string> {
  const now = Math.floor(Date.now() / 1000);
  const header = { alg: "HS256", typ: "JWT" };
  const payload: Record<string, unknown> = {
    sub: orgID,
    iss: "opensandbox-edge",
    iat: now,
    exp: now + 120, // short-lived — it's only the edge→CP hop
    org_id: orgID,
    cell_id: cellID,
  };
  if (userID) payload.user_id = userID;
  const enc = new TextEncoder();
  const signingInput =
    b64url(enc.encode(JSON.stringify(header))) + "." + b64url(enc.encode(JSON.stringify(payload)));
  const key = await crypto.subtle.importKey(
    "raw",
    enc.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = await crypto.subtle.sign("HMAC", key, enc.encode(signingInput));
  return signingInput + "." + b64url(sig);
}

interface Caller {
  orgID: string;
  userID: string | null;
}

// Authenticate via X-API-Key (looked up by sha256 in D1 api_keys). Session-JWT
// auth (browser flows) is a TODO; SDK/test traffic uses the API key.
async function authenticate(req: Request, env: Env): Promise<Caller | null> {
  const apiKey = req.headers.get("X-API-Key");
  if (!apiKey) return null;
  const hash = await sha256Hex(apiKey);
  const row = await env.OPENCOMPUTER_DB.prepare(
    "SELECT org_id, created_by, expires_at FROM api_keys WHERE key_hash = ?1",
  )
    .bind(hash)
    .first<{ org_id: string; created_by: string | null; expires_at: number | null }>();
  if (!row) return null;
  if (row.expires_at && row.expires_at < Math.floor(Date.now() / 1000)) return null;
  // best-effort last_used bump
  env.OPENCOMPUTER_DB.prepare("UPDATE api_keys SET last_used = ?1 WHERE key_hash = ?2")
    .bind(Math.floor(Date.now() / 1000), hash)
    .run()
    .catch(() => {});
  return { orgID: row.org_id, userID: row.created_by };
}

interface CellRow {
  cell_id: string;
  base_url: string;
  status: string;
}

async function lookupCell(env: Env, cellID: string): Promise<CellRow | null> {
  return env.OPENCOMPUTER_DB.prepare(
    "SELECT cell_id, base_url, status FROM cells WHERE cell_id = ?1",
  )
    .bind(cellID)
    .first<CellRow>();
}

// ── route handlers ───────────────────────────────────────────────────────

async function createSandbox(req: Request, env: Env): Promise<Response> {
  const caller = await authenticate(req, env);
  if (!caller) return json({ error: "missing or invalid API key" }, 401);

  const org = await env.OPENCOMPUTER_DB.prepare("SELECT home_cell FROM orgs WHERE id = ?1")
    .bind(caller.orgID)
    .first<{ home_cell: string }>();
  if (!org) return json({ error: "org not found" }, 401);

  const cell = await lookupCell(env, org.home_cell);
  if (!cell) return json({ error: `home cell ${org.home_cell} not registered` }, 503);
  if (cell.status !== "active") return json({ error: `cell ${cell.cell_id} is ${cell.status}` }, 503);

  const capToken = await mintCapToken(env.SESSION_JWT_SECRET, caller.orgID, cell.cell_id, caller.userID);

  const bodyText = await req.text();
  let cpResp: Response;
  try {
    cpResp = await fetch(cell.base_url.replace(/\/$/, "") + "/internal/sandboxes/create", {
      method: "POST",
      headers: { authorization: "Bearer " + capToken, "content-type": "application/json" },
      body: bodyText || "{}",
    });
  } catch (e) {
    return json({ error: `cell ${cell.cell_id} unreachable: ${(e as Error).message}` }, 502);
  }

  const cpText = await cpResp.text();
  if (cpResp.status >= 200 && cpResp.status < 300) {
    let parsed: { sandboxID?: string; workerID?: string; status?: string } = {};
    try {
      parsed = JSON.parse(cpText);
    } catch {
      /* leave parsed empty — still record what we can */
    }
    if (parsed.sandboxID) {
      await env.OPENCOMPUTER_DB.prepare(
        `INSERT OR REPLACE INTO sandboxes_index
           (id, org_id, user_id, cell_id, worker_id, status, created_at, last_event_at)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?7)`,
      )
        .bind(
          parsed.sandboxID,
          caller.orgID,
          caller.userID,
          cell.cell_id,
          parsed.workerID ?? null,
          parsed.status ?? "running",
          Math.floor(Date.now() / 1000),
        )
        .run();
    }
  }
  // Pass the CP's response through verbatim (status + body).
  return new Response(cpText, {
    status: cpResp.status,
    headers: { "content-type": "application/json" },
  });
}

async function listSandboxes(req: Request, env: Env): Promise<Response> {
  const caller = await authenticate(req, env);
  if (!caller) return json({ error: "missing or invalid API key" }, 401);
  const { results } = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, cell_id, worker_id, status, template_id, created_at, last_event_at, stopped_at
       FROM sandboxes_index WHERE org_id = ?1 ORDER BY created_at DESC LIMIT 200`,
  )
    .bind(caller.orgID)
    .all();
  return json({ sandboxes: results });
}

async function getSandbox(req: Request, env: Env, id: string): Promise<Response> {
  const caller = await authenticate(req, env);
  if (!caller) return json({ error: "missing or invalid API key" }, 401);
  const row = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, org_id, cell_id, worker_id, status, template_id, created_at, last_event_at, stopped_at
       FROM sandboxes_index WHERE id = ?1`,
  )
    .bind(id)
    .first<{ org_id: string; cell_id: string } & Record<string, unknown>>();
  if (!row || row.org_id !== caller.orgID) return json({ error: "sandbox not found" }, 404);
  const cell = await lookupCell(env, row.cell_id);
  return json({ ...row, cell_endpoint: cell ? cell.base_url : null });
}

// 307 the request to the owning cell's CP — same path + query, body preserved
// by the 307 semantics. Re-auth happens at the CP (API key / sandbox JWT).
async function redirectToCell(req: Request, env: Env, id: string, url: URL): Promise<Response> {
  const row = await env.OPENCOMPUTER_DB.prepare("SELECT cell_id FROM sandboxes_index WHERE id = ?1")
    .bind(id)
    .first<{ cell_id: string }>();
  if (!row) return json({ error: "sandbox not found" }, 404);
  const cell = await lookupCell(env, row.cell_id);
  if (!cell) return json({ error: `cell ${row.cell_id} not registered` }, 503);
  const target = cell.base_url.replace(/\/$/, "") + url.pathname + url.search;
  return new Response(null, { status: 307, headers: { location: target } });
}

// ── entrypoint ───────────────────────────────────────────────────────────

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const url = new URL(req.url);
    const path = url.pathname;

    if (path === "/health") {
      return json({ ok: true, env: env.WORKER_ENV, cells: env.CELLS.split(",") });
    }

    // /api/sandboxes and /api/sandboxes/:id[/...]
    if (path === "/api/sandboxes") {
      if (req.method === "POST") return createSandbox(req, env);
      if (req.method === "GET") return listSandboxes(req, env);
      return json({ error: "method not allowed" }, 405);
    }
    const m = path.match(/^\/api\/sandboxes\/([^/]+)(\/.*)?$/);
    if (m) {
      const id = m[1];
      const rest = m[2]; // undefined for /api/sandboxes/:id, "/exec/run" etc otherwise
      if (!rest) {
        if (req.method === "GET") return getSandbox(req, env, id);
        if (req.method === "DELETE") return redirectToCell(req, env, id, url); // delete runs on the cell
        return json({ error: "method not allowed" }, 405);
      }
      // Anything under /:id/* (exec, files, pty, hibernate, …) lives on the cell.
      return redirectToCell(req, env, id, url);
    }

    // Not yet implemented: /auth/*, /webhooks/stripe, /internal/halt-list.
    return new Response("not implemented", { status: 501 });
  },
} satisfies ExportedHandler<Env>;
