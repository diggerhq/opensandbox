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
  cloud: string;
  region: string;
  base_url: string;
  status: string;
  available_workers: number;
  capacity_updated_at: number | null;
}

async function lookupCell(env: Env, cellID: string): Promise<CellRow | null> {
  return env.OPENCOMPUTER_DB.prepare(
    `SELECT cell_id, cloud, region, base_url, status, available_workers, capacity_updated_at
       FROM cells WHERE cell_id = ?1`,
  )
    .bind(cellID)
    .first<CellRow>();
}

// Freshness window — the CP emits capacity events every ~30s; 120s is a
// generous 4× margin that covers a missed sample without flapping.
const CAPACITY_FRESH_SEC = 120;

function isHealthy(cell: CellRow, nowSec: number): boolean {
  if (cell.status !== "active") return false;
  if (cell.capacity_updated_at == null) return false;
  if (nowSec - cell.capacity_updated_at > CAPACITY_FRESH_SEC) return false;
  if (cell.available_workers <= 0) return false;
  return true;
}

// Continent buckets used by distanceRank when cells span clouds. Coarse on
// purpose — we just need "near" vs "far" for the cascade. Unknown regions
// fall through to tier 3 (global).
//
// Region names follow the cell-id convention: AWS-style hyphenated form for
// every cloud (e.g., Azure's westus2 is mapped to us-west-2 at provision
// time, so the cells table never sees the cloud-native variant). One table
// for all clouds.
const REGION_CONTINENT: Record<string, string> = {
  // North America
  "us-east-1": "na", "us-east-2": "na",
  "us-west-1": "na", "us-west-2": "na", "us-west-3": "na",
  "us-central-1": "na", "us-north-central-1": "na", "us-south-central-1": "na",
  "ca-central-1": "na", "ca-east-1": "na",
  // Europe
  "eu-west-1": "eu", "eu-west-2": "eu", "eu-west-3": "eu",
  "eu-north-1": "eu", "eu-central-1": "eu", "eu-south-1": "eu",
  "uk-south-1": "eu", "uk-west-1": "eu",
  // Asia / Pacific
  "ap-southeast-1": "ap", "ap-southeast-2": "ap",
  "ap-northeast-1": "ap", "ap-northeast-2": "ap", "ap-northeast-3": "ap",
  "ap-east-1": "ap", "ap-south-1": "ap",
};

// Tier distance from `a` to `b`. Lower is closer.
//   0 — same cloud + same region (cell siblings)
//   1 — same cloud, different region
//   2 — different cloud, same continent
//   3 — anywhere else (different continent, or unknown region)
function distanceRank(a: CellRow, b: CellRow): number {
  if (a.cloud === b.cloud && a.region === b.region) return 0;
  if (a.cloud === b.cloud) return 1;
  const aCont = REGION_CONTINENT[a.region];
  const bCont = REGION_CONTINENT[b.region];
  if (aCont && bCont && aCont === bCont) return 2;
  return 3;
}

// pickCell — layered placement.
//   0. Hard pin from request body (cellId) — strict; if pinned cell is
//      unhealthy/missing, fail rather than silently fall back.
//   1. Healthy candidates (status+freshness+available_workers gates).
//   2. Home cell first, then siblings ordered by tier-distance from home.
//   3. First candidate with capacity wins.
// Returns null if nothing is eligible — caller turns that into 503.
async function pickCell(
  env: Env,
  homeCell: string,
  requestedCellID: string | null,
): Promise<CellRow | null> {
  const nowSec = Math.floor(Date.now() / 1000);

  // 0. Hard pin
  if (requestedCellID) {
    const c = await lookupCell(env, requestedCellID);
    return c && isHealthy(c, nowSec) ? c : null;
  }

  // Look up home regardless of health — we still want its {cloud, region}
  // as the distance anchor even if home itself is currently loaded.
  const home = await lookupCell(env, homeCell);

  const { results } = await env.OPENCOMPUTER_DB.prepare(
    `SELECT cell_id, cloud, region, base_url, status, available_workers, capacity_updated_at
       FROM cells WHERE status = 'active'`,
  ).all<CellRow>();
  const healthy = (results ?? []).filter((c) => isHealthy(c, nowSec));
  if (healthy.length === 0) return null;

  if (home) {
    healthy.sort((a, b) => {
      const da = distanceRank(home, a);
      const db = distanceRank(home, b);
      if (da !== db) return da - db;
      // Tie-break: home wins ties (distance 0 to itself), then alphabetical for
      // deterministic ordering across cells the same distance from home.
      if (a.cell_id === home.cell_id) return -1;
      if (b.cell_id === home.cell_id) return 1;
      return a.cell_id.localeCompare(b.cell_id);
    });
  } else {
    // Home cell not registered in the table at all — degenerate config; pick
    // alphabetically rather than randomly so behavior is at least deterministic.
    healthy.sort((a, b) => a.cell_id.localeCompare(b.cell_id));
  }

  return healthy[0] ?? null;
}

// ── route handlers ───────────────────────────────────────────────────────

async function createSandbox(req: Request, env: Env): Promise<Response> {
  const caller = await authenticate(req, env);
  if (!caller) return json({ error: "missing or invalid API key" }, 401);

  const org = await env.OPENCOMPUTER_DB.prepare("SELECT home_cell FROM orgs WHERE id = ?1")
    .bind(caller.orgID)
    .first<{ home_cell: string }>();
  if (!org) return json({ error: "org not found" }, 401);

  // Read body once — used for the hard-pin peek + forwarded to the CP verbatim.
  const bodyText = await req.text();
  let requestedCellID: string | null = null;
  try {
    if (bodyText) {
      const parsed = JSON.parse(bodyText) as { cellId?: unknown };
      if (typeof parsed.cellId === "string") requestedCellID = parsed.cellId;
    }
  } catch {
    /* malformed JSON — let the CP reject with a proper 400 */
  }

  const cell = await pickCell(env, org.home_cell, requestedCellID);
  if (!cell) {
    return json(
      requestedCellID
        ? { error: `cell ${requestedCellID} is not available` }
        : { error: "no cells available with capacity" },
      503,
    );
  }

  const capToken = await mintCapToken(env.SESSION_JWT_SECRET, caller.orgID, cell.cell_id, caller.userID);
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
