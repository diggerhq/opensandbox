// api-edge Worker — global API entry point.
//
// Implemented:
//   POST /api/sandboxes        — auth (D1 api_keys) → pick org.home_cell →
//                                mint capability token → proxy to that cell's
//                                CP /internal/sandboxes/create → record in
//                                sandboxes_index → return the CP's response
//   GET  /api/sandboxes        — list this org's sandboxes from sandboxes_index
//   GET  /api/sandboxes/:id    — one row + cell_endpoint
//   ANY  /api/sandboxes/:id/*  — 307 to the owning cell's CP (dumb-client path)
//   GET  /internal/halt-list   — HMAC-auth'd; halted org_ids from D1 (CP halt_reconciler)
//   GET  /auth/login           — kicks off WorkOS Authkit flow
//   GET  /auth/callback        — WorkOS code exchange → upsert user/org → session JWT cookie
//   POST /auth/logout          — clear session cookie
//   POST /auth/refresh         — rotate session JWT (extends expiry)
//   POST /webhooks/stripe      — Stripe webhook → DO /mark-pro or /mark-free
//   GET  /health

export { CreditAccount } from "../../shared/credit_account";
import { handleDashboard, type DashboardEnv } from "./dashboard";
import * as secretStores from "./secret_stores";
import * as templates from "./templates";

export interface Env extends DashboardEnv {
  CF_ADMIN_SECRET: string;
  STRIPE_WEBHOOK_SECRET: string;
  EVENT_SECRET: string;
  // Shared with every CP via Infisical /shared/ → per-cell KV/SM. Used for
  // envelope encryption of secret_store_entries.encrypted_value. Matches
  // internal/crypto.Encryptor key format (hex-encoded 32 bytes).
  SECRET_ENCRYPTION_KEY: string;
  // CF_API_TOKEN and CF_ZONE_ID are optional in DashboardEnv (custom domain
  // feature gates on them). Inherited.
  ASSETS?: Fetcher;
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
// org_id + cell_id + plan (+ optional user_id). Mirrors auth.CapabilityClaims
// in Go. Plan flows through so the worker can tag usage_tick events without
// a per-event PG lookup.
async function mintCapToken(
  secret: string,
  orgID: string,
  cellID: string,
  plan: string,
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
    plan,
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

// ── preview URL dispatch ─────────────────────────────────────────────────

// parsePreviewHost detects whether the request's hostname is a sandbox
// preview URL of the form `sb-{id}-p{port}.{anything}` and pulls out the
// sandbox_id + port. Returns null for anything else (the request falls
// through to the regular /api routes / /health / etc).
//
// The sandbox_id itself may contain hyphens (it's "sb-" + 8 hex chars in
// practice, but we don't lock the format here — only the trailing -p<port>
// shape matters), so the regex anchors `-p<digits>` at the END of the first
// subdomain label and grabs everything before it as the id.
function parsePreviewHost(hostname: string): { sandboxID: string; port: number } | null {
  const firstLabel = hostname.split(".", 1)[0];
  if (!firstLabel.startsWith("sb-")) return null;
  const m = firstLabel.match(/^(sb-.+)-p(\d+)$/);
  if (!m) return null;
  const port = Number.parseInt(m[2], 10);
  if (!Number.isFinite(port) || port < 1 || port > 65535) return null;
  return { sandboxID: m[1], port };
}

// handlePreviewURL is the edge-routed equivalent of the cell-local
// ControlPlaneProxy.Middleware: resolve the sandbox to its owning cell via
// D1, then forward the request through that cell's Tunnel to its CP's
// /internal/preview/{id}/{port}/* route. The CP synthesizes the Host
// header the worker's SandboxProxy expects, then routes to the worker.
//
// Cross-cell migration becomes invisible from this design — moving a
// sandbox from cell A to cell B updates sandboxes_index.cell_id, and the
// next request resolves to the new cell. No DNS or hostname changes.
async function handlePreviewURL(
  req: Request,
  env: Env,
  m: { sandboxID: string; port: number },
): Promise<Response> {
  const row = await env.OPENCOMPUTER_DB.prepare(
    `SELECT s.cell_id, s.status, c.base_url
       FROM sandboxes_index s
       JOIN cells c ON s.cell_id = c.cell_id
      WHERE s.id = ?1`,
  )
    .bind(m.sandboxID)
    .first<{ cell_id: string; status: string; base_url: string }>();

  if (!row) return new Response(`sandbox ${m.sandboxID} not found`, { status: 404 });
  if (row.status === "stopped" || row.status === "error") {
    return new Response(`sandbox ${m.sandboxID} is ${row.status}`, { status: 410 });
  }
  // status="hibernated" is fine — CP's doProxy will wake-on-request.

  const url = new URL(req.url);
  const base = row.base_url.replace(/\/$/, "");
  const target = `${base}/internal/preview/${m.sandboxID}/${m.port}${url.pathname}${url.search}`;

  try {
    // Forward the request as-is via the Request copy-constructor — preserves
    // method, body (including streamed/large bodies), headers, AND the
    // Upgrade: websocket handshake. Cloudflare's fetch propagates WebSocket
    // pairs transparently when both ends speak it.
    return await fetch(new Request(target, req));
  } catch (e) {
    return new Response(
      `cell ${row.cell_id} unreachable: ${(e as Error).message}`,
      { status: 502 },
    );
  }
}

// ── route handlers ───────────────────────────────────────────────────────

async function createSandbox(req: Request, env: Env): Promise<Response> {
  const caller = await authenticate(req, env);
  if (!caller) return json({ error: "missing or invalid API key" }, 401);

  const org = await env.OPENCOMPUTER_DB.prepare("SELECT home_cell, plan, is_halted FROM orgs WHERE id = ?1")
    .bind(caller.orgID)
    .first<{ home_cell: string; plan: string; is_halted: number }>();
  if (!org) return json({ error: "org not found" }, 401);
  const plan = org.plan === "pro" ? "pro" : "free";

  // Gate on billing state BEFORE picking a cell. Free orgs hit the DO
  // /check for an authoritative balance read; pro orgs skip the round trip.
  // is_halted is a D1 fast path — if it's 1, we don't even need to ask the DO.
  if (plan === "free") {
    if (org.is_halted === 1) {
      return json({ error: "free trial credits exhausted — upgrade to resume" }, 402);
    }
    const doID = env.CREDIT_ACCOUNT.idFromName(caller.orgID);
    const doStub = env.CREDIT_ACCOUNT.get(doID);
    const checkResp = await doStub.fetch(`https://do/check?org_id=${encodeURIComponent(caller.orgID)}`, {
      method: "POST",
    });
    if (checkResp.status !== 200) {
      // DO failure shouldn't soft-fail open — credit gating exists for a reason.
      // If the DO is genuinely down we get a 5xx; surface that.
      return json({ error: "credit check unavailable" }, 503);
    }
    const check = await checkResp.json<{ allowed: boolean; balance_cents: number }>();
    if (!check.allowed) {
      return json({ error: "free trial credits exhausted — upgrade to resume", balance_cents: check.balance_cents }, 402);
    }
  }

  // Read body once — used for the hard-pin peek + forwarded to the CP verbatim.
  const bodyText = await req.text();
  let requestedCellID: string | null = null;
  let bodyCpuCount = 0;
  let bodyMemoryMB = 0;
  try {
    if (bodyText) {
      const parsed = JSON.parse(bodyText) as { cellId?: unknown; cpuCount?: unknown; memoryMB?: unknown };
      if (typeof parsed.cellId === "string") requestedCellID = parsed.cellId;
      if (typeof parsed.cpuCount === "number") bodyCpuCount = parsed.cpuCount;
      if (typeof parsed.memoryMB === "number") bodyMemoryMB = parsed.memoryMB;
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

  const capToken = await mintCapToken(env.SESSION_JWT_SECRET, caller.orgID, cell.cell_id, plan, caller.userID);
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
           (id, org_id, user_id, cell_id, worker_id, status, cpu_count, memory_mb, created_at, last_event_at)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?9)`,
      )
        .bind(
          parsed.sandboxID,
          caller.orgID,
          caller.userID,
          cell.cell_id,
          parsed.workerID ?? null,
          parsed.status ?? "running",
          bodyCpuCount,
          bodyMemoryMB,
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

// sandboxRowToJSON reshapes a D1 sandboxes_index row into the JSON the legacy
// CP /api/sandboxes returned — which the Go CLI's types.Sandbox struct + the
// Python/TS SDKs all unmarshal against. Important translations:
//   id            → sandboxID  (Go json tag is `sandboxID`)
//   template_id   → templateID
//   cell_id       → cellID
//   worker_id     → workerID
//   created_at    → startedAt  (unix int → ISO string)
//   stopped_at    → endAt      (unix int / null → ISO string; null becomes the
//                                Go time.Time zero value, "0001-01-01T00:00:00Z")
//   cpu_count     → cpuCount
//   memory_mb     → memoryMB
interface SandboxRow {
  id: string;
  cell_id: string;
  worker_id: string | null;
  status: string;
  template_id: string | null;
  cpu_count: number | null;
  memory_mb: number | null;
  created_at: number;
  last_event_at: number | null;
  stopped_at: number | null;
}

function isoFromUnix(secs: number | null): string {
  // Go time.Time zero value when the column is NULL. The CLI tolerates this.
  if (secs == null || secs === 0) return "0001-01-01T00:00:00Z";
  return new Date(secs * 1000).toISOString();
}

function sandboxRowToJSON(r: SandboxRow): Record<string, unknown> {
  return {
    sandboxID: r.id,
    templateID: r.template_id ?? "",
    cellID: r.cell_id,
    workerID: r.worker_id ?? "",
    status: r.status,
    cpuCount: r.cpu_count ?? 0,
    memoryMB: r.memory_mb ?? 0,
    startedAt: isoFromUnix(r.created_at),
    endAt: isoFromUnix(r.stopped_at),
  };
}

async function listSandboxes(req: Request, env: Env): Promise<Response> {
  const caller = await authenticate(req, env);
  if (!caller) return json({ error: "missing or invalid API key" }, 401);
  const { results } = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, cell_id, worker_id, status, template_id, cpu_count, memory_mb,
            created_at, last_event_at, stopped_at
       FROM sandboxes_index WHERE org_id = ?1 ORDER BY created_at DESC LIMIT 200`,
  )
    .bind(caller.orgID)
    .all<SandboxRow>();
  return json((results ?? []).map(sandboxRowToJSON));
}

async function getSandbox(req: Request, env: Env, id: string): Promise<Response> {
  const caller = await authenticate(req, env);
  if (!caller) return json({ error: "missing or invalid API key" }, 401);
  const row = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, org_id, cell_id, worker_id, status, template_id, cpu_count, memory_mb,
            created_at, last_event_at, stopped_at
       FROM sandboxes_index WHERE id = ?1`,
  )
    .bind(id)
    .first<SandboxRow & { org_id: string }>();
  if (!row || row.org_id !== caller.orgID) return json({ error: "sandbox not found" }, 404);
  const cell = await lookupCell(env, row.cell_id);
  return json({ ...sandboxRowToJSON(row), cellEndpoint: cell ? cell.base_url : null });
}

// Proxy the request to the owning cell's CP. Used for SDK runtime calls
// (`/api/sandboxes/:id/exec`, `/files`, `/pty`, etc.). The caller has been
// authenticated at the edge against D1; we mint a short-lived IdentityToken
// the cell's PGAPIKeyMiddleware already accepts (audience `opencomputer-api`)
// and stream the response back. Pre-fix this was a 307 redirect, which broke
// when the SDK's API key didn't exist in cell PG (api_keys are global in D1
// now, not mirrored per-cell).
async function proxyToCellSDK(req: Request, env: Env, ctx: ExecutionContext, caller: Caller, id: string): Promise<Response> {
  const row = await env.OPENCOMPUTER_DB.prepare("SELECT cell_id FROM sandboxes_index WHERE id = ?1")
    .bind(id)
    .first<{ cell_id: string }>();
  if (!row) return json({ error: "sandbox not found" }, 404);
  const cell = await lookupCell(env, row.cell_id);
  if (!cell) return json({ error: `cell ${row.cell_id} not registered` }, 503);

  const url = new URL(req.url);
  const target = cell.base_url.replace(/\/$/, "") + url.pathname + url.search;
  // Look up the org's plan so the cap-token carries it (worker resolver uses
  // plan to tag usage_tick events; without it free-tier debit fan-out skips
  // the org).
  const orgRow = await env.OPENCOMPUTER_DB.prepare("SELECT plan FROM orgs WHERE id = ?1")
    .bind(caller.orgID).first<{ plan: string }>();
  // Mint a cap-token (iss=opensandbox-edge, signed with SESSION_JWT_SECRET).
  // The cell's PGAPIKeyMiddleware accepts cap-tokens too (alongside identity
  // tokens and API keys), so the same handler chain that runs for SDK
  // X-API-Key auth runs here. cell_id in the token guards against replay
  // against a different cell.
  const token = await mintCapToken(env.SESSION_JWT_SECRET, caller.orgID, row.cell_id, orgRow?.plan ?? "free", caller.userID);

  const headers = new Headers();
  for (const [k, v] of req.headers.entries()) {
    const lk = k.toLowerCase();
    // Drop the caller's X-API-Key — the cell would try to validate it against
    // its own PG and fail. We replace it with the IdentityToken JWT below.
    if (lk === "host" || lk === "cookie" || lk === "x-api-key" || lk.startsWith("cf-") || lk.startsWith("x-forwarded-")) continue;
    headers.set(k, v);
  }
  headers.set("authorization", "Bearer " + token);

  const init: RequestInit = { method: req.method, headers };
  if (req.method !== "GET" && req.method !== "HEAD") init.body = req.body;

  // Intercept lifecycle ops to keep D1 sandboxes_index in sync with cell PG.
  // Edge already writes the row on CREATE; here we mirror the cell's status
  // changes on DELETE / hibernate / wake. Otherwise the dashboard accumulates
  // phantoms — D1 rows stuck at "running" after the actual sandbox stopped.
  const path = url.pathname;
  let postUpdate: { status: string; setStopped: boolean } | null = null;
  if (req.method === "DELETE" && path === `/api/sandboxes/${id}`) {
    postUpdate = { status: "stopped", setStopped: true };
  } else if (req.method === "POST" && path === `/api/sandboxes/${id}/hibernate`) {
    postUpdate = { status: "hibernated", setStopped: false };
  } else if (req.method === "POST" && path === `/api/sandboxes/${id}/wake`) {
    postUpdate = { status: "running", setStopped: false };
    // Halt-gate the wake. D1 is authoritative for is_halted. The cell-side
    // gate that used to do this read the dropped orgs table post-041 and
    // silently fell through, letting halted orgs wake. Mirror the create
    // flow's halt check here so wake gets the same treatment.
    const haltRow = await env.OPENCOMPUTER_DB.prepare(
      "SELECT is_halted FROM orgs WHERE id = ?1",
    )
      .bind(caller.orgID)
      .first<{ is_halted: number }>();
    if (haltRow?.is_halted === 1) {
      return json(
        { error: "org is halted — upgrade to pro or wait for credit refill" },
        402,
      );
    }
  }

  // WebSocket upgrade — preserve the upgrade context by cloning the inbound
  // Request, then swap Authorization. The manual fetch + Sec-WebSocket-Key
  // copy / WebSocketPair bridge dance below was buggy ("bad handshake" on
  // the CLI side because the upgrade headers got rebuilt without proper
  // accept-key derivation). CF Workers + CF Tunnel forward WebSocket
  // upgrades transparently when you pass a Request clone — same pattern
  // handlePreviewURL uses and that's verified to work end-to-end with WS.
  if (req.headers.get("upgrade")?.toLowerCase() === "websocket") {
    const fwd = new Request(target, req);
    fwd.headers.set("authorization", "Bearer " + token);
    fwd.headers.delete("x-api-key");
    return await fetch(fwd);
  }

  // Non-WebSocket path. Run the proxy and then, on success, fan out the
  // status update to D1 so the dashboard sees the new state immediately.
  // Otherwise the dashboard accumulates phantoms — D1 rows stuck at
  // "running" after the actual sandbox stopped.
  const resp = await fetch(target, init);
  if (postUpdate && resp.status >= 200 && resp.status < 300) {
    const nowSec = Math.floor(Date.now() / 1000);
    const updateSQL = postUpdate.setStopped
      ? "UPDATE sandboxes_index SET status = ?1, stopped_at = ?2, last_event_at = ?2 WHERE id = ?3"
      : "UPDATE sandboxes_index SET status = ?1, last_event_at = ?2 WHERE id = ?3";
    // ctx.waitUntil keeps the background D1 write alive after the response
    // returns. Without it the Worker terminates the in-flight Promise and
    // the UPDATE never runs — sandboxes_index drifts behind cell PG.
    ctx.waitUntil(
      env.OPENCOMPUTER_DB.prepare(updateSQL).bind(postUpdate.status, nowSec, id).run().catch((e) => {
        console.error(`sandboxes_index ${postUpdate!.status} update failed for ${id}:`, e);
      }),
    );
  }
  return resp;
}


// ── /internal/halt-list ─────────────────────────────────────────────────

// HMAC-auth'd endpoint the cell's halt_reconciler polls every 60s to
// reconcile any halt webhooks it might have missed. Returns the list of
// org_ids that the DO currently flags halted (mirrored in D1 orgs.is_halted).
// HMAC scheme matches the DO's dispatch: "{X-Timestamp}.{path-with-query}"
// signed with EVENT_SECRET (shared with CP), SHA-256 hex.
async function haltList(req: Request, env: Env): Promise<Response> {
  const ts = req.headers.get("X-Timestamp") ?? "";
  const sig = req.headers.get("X-Signature") ?? "";
  if (!ts || !sig) return json({ error: "missing signature headers" }, 400);
  const tsNum = Number.parseInt(ts, 10);
  if (!Number.isFinite(tsNum)) return json({ error: "invalid timestamp" }, 400);
  const now = Math.floor(Date.now() / 1000);
  if (Math.abs(now - tsNum) > 5 * 60) return json({ error: "timestamp out of window" }, 401);

  const url = new URL(req.url);
  const cellID = url.searchParams.get("cell") ?? "";
  const expected = await hmacHex(env.EVENT_SECRET, `${ts}.${url.pathname}${url.search}`);
  if (!constantTimeEqual(expected, sig)) return json({ error: "signature mismatch" }, 401);

  // Return halted orgs that have any sandbox on the requesting cell. The
  // reconciler only needs to act on orgs it can do something about — orgs
  // halted with sandboxes on a DIFFERENT cell are that cell's reconciler's
  // problem. If no `cell` param is supplied, return all halted orgs (used
  // by parity-check cron / debugging).
  let results: { id: string; halted_at: number | null }[];
  if (cellID) {
    const res = await env.OPENCOMPUTER_DB.prepare(
      `SELECT DISTINCT o.id, o.halted_at
         FROM orgs o
         JOIN sandboxes_index s ON s.org_id = o.id
        WHERE o.is_halted = 1 AND s.cell_id = ?1`,
    )
      .bind(cellID)
      .all<{ id: string; halted_at: number | null }>();
    results = res.results ?? [];
  } else {
    const res = await env.OPENCOMPUTER_DB.prepare(
      `SELECT id, halted_at FROM orgs WHERE is_halted = 1`,
    ).all<{ id: string; halted_at: number | null }>();
    results = res.results ?? [];
  }
  return json({
    org_ids: results.map((r) => r.id),
    halted_at: Object.fromEntries(results.map((r) => [r.id, r.halted_at])),
    as_of: now,
  });
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

function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

// ── WorkOS auth flow ─────────────────────────────────────────────────────
//
// Browser flow:
//   GET /auth/login           → 302 to WorkOS AuthKit authorization URL
//   GET /auth/callback?code=  → exchange code, upsert user+org in D1, mint
//                                session JWT, set cookie, redirect to /dashboard
//   POST /auth/logout         → clear session cookie
//   POST /auth/refresh        → rotate session JWT (extends expiry)
//
// Session JWT lives in an httpOnly Secure SameSite=Lax cookie named
// `oc_session`. The same secret signs the cap-token, so the cell can
// verify a session JWT presented directly (browser fetch from dashboard)
// using the existing capTokenMiddleware — we just set Issuer="opensandbox-session"
// to distinguish.

const SESSION_COOKIE = "oc_session";
const SESSION_TTL_SEC = 60 * 60 * 8; // 8h

interface WorkOSProfile {
  id: string;
  email: string;
  first_name?: string;
  last_name?: string;
  organization_id?: string;
}

async function authLogin(req: Request, env: Env): Promise<Response> {
  const reqURL = new URL(req.url);
  const redirectURI = `${reqURL.origin}/auth/callback`;
  // WorkOS AuthKit hosted login URL. authorize_url uses provider=authkit
  // for the "magic-link or oauth" hosted page.
  const authURL = new URL("https://api.workos.com/user_management/authorize");
  authURL.searchParams.set("client_id", env.WORKOS_CLIENT_ID);
  authURL.searchParams.set("provider", "authkit");
  authURL.searchParams.set("redirect_uri", redirectURI);
  authURL.searchParams.set("response_type", "code");
  return Response.redirect(authURL.toString(), 302);
}

async function authCallback(req: Request, env: Env): Promise<Response> {
  const reqURL = new URL(req.url);
  const code = reqURL.searchParams.get("code");
  if (!code) return json({ error: "missing code" }, 400);
  const redirectURI = `${reqURL.origin}/auth/callback`;

  // Exchange code for user profile.
  const tokenResp = await fetch("https://api.workos.com/user_management/authenticate", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      client_id: env.WORKOS_CLIENT_ID,
      client_secret: env.WORKOS_API_KEY,
      grant_type: "authorization_code",
      code,
      redirect_uri: redirectURI,
    }),
  });
  if (tokenResp.status !== 200) {
    const errText = await tokenResp.text();
    return json({ error: `workos exchange failed: ${tokenResp.status}: ${errText}` }, 401);
  }
  const tokenBody = await tokenResp.json<{ user: WorkOSProfile; organization_id?: string }>();
  const profile = tokenBody.user;

  // Upsert user + org. We share WorkOS with prod, so workos_user_id is the
  // stable identity across both. Org auto-created on first sign-in with
  // home_cell picked per the policy below.
  const nowSec = Math.floor(Date.now() / 1000);
  const userRow = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id FROM users WHERE workos_user_id = ?1`,
  )
    .bind(profile.id)
    .first<{ id: string }>();
  let userID: string;
  if (userRow) {
    userID = userRow.id;
  } else {
    userID = crypto.randomUUID();
    await env.OPENCOMPUTER_DB.prepare(
      `INSERT INTO users (id, email, workos_user_id, name, created_at)
       VALUES (?1, ?2, ?3, ?4, ?5)
       ON CONFLICT(email) DO UPDATE SET workos_user_id = excluded.workos_user_id`,
    )
      .bind(
        userID,
        profile.email,
        profile.id,
        [profile.first_name, profile.last_name].filter(Boolean).join(" ") || profile.email,
        nowSec,
      )
      .run();
  }

  // Find an org via membership; if none, create a personal one.
  const orgRow = await env.OPENCOMPUTER_DB.prepare(
    `SELECT o.id, o.plan FROM orgs o
       JOIN org_memberships m ON m.org_id = o.id
      WHERE m.user_id = ?1 LIMIT 1`,
  )
    .bind(userID)
    .first<{ id: string; plan: string }>();
  let orgID: string;
  let orgPlan: string;
  if (orgRow) {
    orgID = orgRow.id;
    orgPlan = orgRow.plan;
  } else {
    orgID = crypto.randomUUID();
    const homeCell = await pickHomeCell(env, req);
    orgPlan = "free";
    await env.OPENCOMPUTER_DB.prepare(
      `INSERT INTO orgs (id, name, slug, plan, home_cell, is_personal, owner_user_id, created_at, updated_at)
       VALUES (?1, ?2, ?3, 'free', ?4, 1, ?5, ?6, ?6)`,
    )
      .bind(orgID, `${profile.email}'s workspace`, slugify(profile.email + "-" + orgID.slice(0, 6)), homeCell, userID, nowSec)
      .run();
    await env.OPENCOMPUTER_DB.prepare(
      `INSERT INTO org_memberships (org_id, user_id, role, created_at) VALUES (?1, ?2, 'owner', ?3)`,
    )
      .bind(orgID, userID, nowSec)
      .run();
  }

  // Mint session JWT — same signing secret as cap-token but a different
  // Issuer so cell middleware can distinguish.
  const sessionJWT = await mintSessionJWT(env.SESSION_JWT_SECRET, orgID, userID, orgPlan);

  // Redirect to dashboard with the cookie set.
  const dashURL = `${reqURL.origin}/dashboard`;
  return new Response(null, {
    status: 302,
    headers: {
      location: dashURL,
      "set-cookie": `${SESSION_COOKIE}=${sessionJWT}; HttpOnly; Secure; SameSite=Lax; Path=/; Max-Age=${SESSION_TTL_SEC}`,
    },
  });
}

async function authLogout(): Promise<Response> {
  return new Response(null, {
    status: 204,
    headers: {
      "set-cookie": `${SESSION_COOKIE}=; HttpOnly; Secure; SameSite=Lax; Path=/; Max-Age=0`,
    },
  });
}

async function authRefresh(req: Request, env: Env): Promise<Response> {
  const cookie = req.headers.get("cookie") ?? "";
  const m = cookie.match(new RegExp(`${SESSION_COOKIE}=([^;]+)`));
  if (!m) return json({ error: "no session" }, 401);
  const claims = await verifySessionJWT(env.SESSION_JWT_SECRET, m[1]);
  if (!claims) return json({ error: "invalid session" }, 401);
  // Re-mint with fresh expiry. Plan is re-read from D1 in case it changed.
  const orgRow = await env.OPENCOMPUTER_DB.prepare(
    `SELECT plan FROM orgs WHERE id = ?1`,
  )
    .bind(claims.org_id)
    .first<{ plan: string }>();
  const plan = orgRow?.plan ?? "free";
  const fresh = await mintSessionJWT(env.SESSION_JWT_SECRET, claims.org_id, claims.user_id, plan);
  return new Response(JSON.stringify({ ok: true, plan }), {
    status: 200,
    headers: {
      "content-type": "application/json",
      "set-cookie": `${SESSION_COOKIE}=${fresh}; HttpOnly; Secure; SameSite=Lax; Path=/; Max-Age=${SESSION_TTL_SEC}`,
    },
  });
}

// pickHomeCell chooses a home cell for a brand-new org. Policy:
//   1. If the request carries a `cf-ipcountry` header, map to a continent
//      and prefer a cell whose region is on that continent.
//   2. Otherwise pick the first cell from env.CELLS that's currently
//      registered in D1 and healthy.
//   3. Last-resort fallback: first entry in env.CELLS regardless of D1 state.
//
// Geo lookup is intentionally coarse — continent-level is enough for
// "don't put a UK user on a US cell" without an IP-to-region service.
async function pickHomeCell(env: Env, req: Request): Promise<string> {
  const country = req.headers.get("cf-ipcountry") ?? "";
  const continent = COUNTRY_TO_CONTINENT[country.toUpperCase()] ?? "";

  const configured = env.CELLS.split(",").map((c) => c.trim()).filter(Boolean);
  if (configured.length === 0) return ""; // misconfigured — let downstream error

  const { results } = await env.OPENCOMPUTER_DB.prepare(
    `SELECT cell_id, region, status FROM cells WHERE status = 'active'`,
  ).all<{ cell_id: string; region: string; status: string }>();
  const activeCells = (results ?? []).filter((c) => configured.includes(c.cell_id));

  if (continent && activeCells.length > 0) {
    const onContinent = activeCells.find((c) => REGION_CONTINENT[c.region] === continent);
    if (onContinent) return onContinent.cell_id;
  }
  if (activeCells.length > 0) return activeCells[0].cell_id;
  return configured[0];
}

// COUNTRY_TO_CONTINENT covers the countries we'd actually see on the
// edge. Missing entries fall through to "no continent hint" — we don't
// need to be exhaustive; an unknown country just means we pick the
// first active cell.
const COUNTRY_TO_CONTINENT: Record<string, string> = {
  US: "na", CA: "na", MX: "na",
  GB: "eu", IE: "eu", DE: "eu", FR: "eu", IT: "eu", ES: "eu", NL: "eu", SE: "eu", PL: "eu", CH: "eu", AT: "eu", BE: "eu", DK: "eu", FI: "eu", NO: "eu", PT: "eu", CZ: "eu",
  JP: "ap", KR: "ap", CN: "ap", IN: "ap", SG: "ap", AU: "ap", NZ: "ap", HK: "ap", TW: "ap", ID: "ap", PH: "ap", VN: "ap", TH: "ap", MY: "ap",
  BR: "sa", AR: "sa", CL: "sa", CO: "sa", PE: "sa",
  ZA: "af", NG: "af", EG: "af", KE: "af",
};

function slugify(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 50);
}

interface SessionClaims {
  org_id: string;
  user_id: string;
  plan: string;
  iat: number;
  exp: number;
}

async function mintSessionJWT(secret: string, orgID: string, userID: string, plan: string): Promise<string> {
  const now = Math.floor(Date.now() / 1000);
  const header = { alg: "HS256", typ: "JWT" };
  const payload = {
    iss: "opensandbox-session",
    sub: userID,
    iat: now,
    exp: now + SESSION_TTL_SEC,
    org_id: orgID,
    user_id: userID,
    plan,
  };
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

async function verifySessionJWT(secret: string, token: string): Promise<SessionClaims | null> {
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  const [headerB64, payloadB64, sigB64] = parts;
  const enc = new TextEncoder();
  const key = await crypto.subtle.importKey(
    "raw",
    enc.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const expected = await crypto.subtle.sign("HMAC", key, enc.encode(`${headerB64}.${payloadB64}`));
  if (b64url(expected) !== sigB64) return null;
  try {
    const payload = JSON.parse(atob(payloadB64.replace(/-/g, "+").replace(/_/g, "/"))) as SessionClaims & { iss?: string };
    if (payload.iss !== "opensandbox-session") return null;
    if (payload.exp < Math.floor(Date.now() / 1000)) return null;
    return payload;
  } catch {
    return null;
  }
}

// ── Stripe webhook ───────────────────────────────────────────────────────
//
// Stripe POSTs subscription / invoice events. We verify the signature,
// translate the event into a CreditAccount DO call:
//   customer.subscription.created / checkout.session.completed → /mark-pro
//   customer.subscription.deleted                              → /mark-free
//
// org_id is recovered from Stripe customer metadata (set when we create
// the customer at upgrade-checkout time). For events without an org_id we
// log and return 200 — Stripe expects 2xx or it'll retry forever.

async function stripeWebhook(req: Request, env: Env): Promise<Response> {
  const sigHeader = req.headers.get("stripe-signature") ?? "";
  const body = await req.text();

  if (!(await verifyStripeSignature(env.STRIPE_WEBHOOK_SECRET, sigHeader, body))) {
    return json({ error: "invalid signature" }, 401);
  }

  let event: { type: string; data: { object: any } };
  try {
    event = JSON.parse(body);
  } catch {
    return json({ error: "invalid json" }, 400);
  }

  const obj = event.data?.object ?? {};
  const orgID = obj.metadata?.org_id || obj.customer_metadata?.org_id || "";

  switch (event.type) {
    case "customer.subscription.created":
    case "checkout.session.completed": {
      if (!orgID) {
        console.error(`stripe: ${event.type} without org_id metadata; logging and skipping`);
        return json({ received: true, skipped: "no org_id" });
      }
      const stub = env.CREDIT_ACCOUNT.get(env.CREDIT_ACCOUNT.idFromName(orgID));
      const resp = await stub.fetch(`https://do/mark-pro?org_id=${encodeURIComponent(orgID)}`, { method: "POST" });
      if (resp.status >= 400) {
        console.error(`stripe: DO /mark-pro ${orgID} returned ${resp.status}`);
      }
      // Stamp stripe IDs on the org row for the next callback round-trip.
      if (obj.customer || obj.subscription) {
        await env.OPENCOMPUTER_DB.prepare(
          `UPDATE orgs SET stripe_customer_id = COALESCE(?1, stripe_customer_id),
                            stripe_subscription_id = COALESCE(?2, stripe_subscription_id),
                            updated_at = ?3
            WHERE id = ?4`,
        )
          .bind(obj.customer ?? null, obj.subscription ?? null, Math.floor(Date.now() / 1000), orgID)
          .run();
      }
      return json({ received: true });
    }
    case "customer.subscription.deleted": {
      if (!orgID) {
        console.error(`stripe: subscription.deleted without org_id; skipping`);
        return json({ received: true, skipped: "no org_id" });
      }
      const stub = env.CREDIT_ACCOUNT.get(env.CREDIT_ACCOUNT.idFromName(orgID));
      const resp = await stub.fetch(`https://do/mark-free?org_id=${encodeURIComponent(orgID)}`, { method: "POST" });
      if (resp.status >= 400) {
        console.error(`stripe: DO /mark-free ${orgID} returned ${resp.status}`);
      }
      return json({ received: true });
    }
    default:
      // Many event types we don't care about (invoice.*, payment_method.*, etc.).
      // Ack so Stripe stops retrying.
      return json({ received: true, ignored: event.type });
  }
}

// verifyStripeSignature checks the t=… v1=… Stripe-Signature header.
// Stripe signs `${timestamp}.${body}` with HMAC-SHA256.
async function verifyStripeSignature(secret: string, header: string, body: string): Promise<boolean> {
  const parts = header.split(",").map((p) => p.split("="));
  const ts = parts.find((p) => p[0] === "t")?.[1];
  const v1 = parts.find((p) => p[0] === "v1")?.[1];
  if (!ts || !v1) return false;
  // Reject signatures older than 5 minutes (Stripe replay defense recommendation).
  const tsNum = Number.parseInt(ts, 10);
  if (!Number.isFinite(tsNum) || Math.abs(Math.floor(Date.now() / 1000) - tsNum) > 5 * 60) return false;
  const expected = await hmacHex(secret, `${ts}.${body}`);
  return constantTimeEqual(expected, v1);
}

// ── entrypoint ───────────────────────────────────────────────────────────

export default {
  async fetch(req: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const url = new URL(req.url);
    const path = url.pathname;

    // Sandbox preview URL dispatch — matched by HOSTNAME, not path. Has to
    // run before the path-based routes below so that a sandbox app serving
    // its own /health or /api/* doesn't get shadowed by ours.
    const preview = parsePreviewHost(url.hostname);
    if (preview) {
      return handlePreviewURL(req, env, preview);
    }

    if (path === "/health") {
      return json({ ok: true, env: env.WORKER_ENV, cells: env.CELLS.split(",") });
    }

    if (path === "/internal/halt-list") {
      if (req.method !== "GET") return json({ error: "method not allowed" }, 405);
      return haltList(req, env);
    }

    // /internal/admin/do-mark-free — operator-only escape hatch to flip a
    // CreditAccount DO's internal plan from "pro" back to "free" without
    // running a real Stripe subscription.deleted webhook. HMAC-auth'd with
    // the shared CF_ADMIN_SECRET. Body: { org_id }. Used by halt-flow tests
    // and incident recovery when Stripe webhooks are missed; not exposed
    // through any UI.
    if (path === "/internal/admin/do-mark-free" && req.method === "POST") {
      const ts = req.headers.get("X-Timestamp") ?? "";
      const sig = req.headers.get("X-Signature") ?? "";
      const body = await req.text();
      const expected = await hmacHex(env.CF_ADMIN_SECRET, `${ts}.${body}`);
      if (!constantTimeEqual(expected, sig)) return json({ error: "signature mismatch" }, 401);
      if (Math.abs(Math.floor(Date.now() / 1000) - Number(ts)) > 300) return json({ error: "timestamp out of window" }, 401);
      const parsed = JSON.parse(body) as { org_id?: string };
      if (!parsed.org_id) return json({ error: "org_id required" }, 400);
      const stub = env.CREDIT_ACCOUNT.get(env.CREDIT_ACCOUNT.idFromName(parsed.org_id));
      const r = await stub.fetch(`https://do/mark-free?org_id=${encodeURIComponent(parsed.org_id)}`, { method: "POST" });
      return new Response(await r.text(), { status: r.status, headers: { "content-type": "application/json" } });
    }

    // /internal/secret-stores/:id — HMAC-auth'd, called by CP at sandbox-create
    // time to materialize the encrypted entry list. CP decrypts with the
    // shared SECRET_ENCRYPTION_KEY before injecting into worker env.
    if (path === "/internal/secret-stores/by-name") {
      if (req.method !== "GET") return json({ error: "method not allowed" }, 405);
      return secretStores.internalGetStoreByName(req, env);
    }
    {
      const m = path.match(/^\/internal\/secret-stores\/([^/]+)$/);
      if (m) {
        if (req.method !== "GET") return json({ error: "method not allowed" }, 405);
        return secretStores.internalGetStore(req, env, m[1]);
      }
    }

    // /internal/templates/* — HMAC-auth'd. by-name = sandbox-create lookup;
    // POST / = "save sandbox as template" registration; PUT /:id/status =
    // flip status='ready' once snapshot upload finishes.
    if (path === "/internal/templates/by-name") {
      if (req.method !== "GET") return json({ error: "method not allowed" }, 405);
      return templates.internalGetByName(req, env);
    }
    if (path === "/internal/templates") {
      if (req.method !== "POST") return json({ error: "method not allowed" }, 405);
      return templates.internalRegister(req, env);
    }
    {
      const m = path.match(/^\/internal\/templates\/([^/]+)\/status$/);
      if (m) {
        if (req.method !== "PUT") return json({ error: "method not allowed" }, 405);
        return templates.internalUpdateStatus(req, env, m[1]);
      }
    }

    // /api/secret-stores — org-scoped CRUD. Same X-API-Key auth as
    // /api/sandboxes; replaces the legacy CP-side PG routes (deleted in
    // the same PR as migration 041).
    if (path === "/api/secret-stores") {
      const caller = await authenticate(req, env);
      if (!caller) return json({ error: "missing or invalid API key" }, 401);
      if (req.method === "POST") return secretStores.createStore(req, env, caller);
      if (req.method === "GET") return secretStores.listStores(req, env, caller);
      return json({ error: "method not allowed" }, 405);
    }
    {
      // /api/secret-stores/:id, /api/secret-stores/:id/secrets, /:id/secrets/:name
      const m = path.match(/^\/api\/secret-stores\/([^/]+)(?:\/secrets(?:\/([^/]+))?)?$/);
      if (m) {
        const storeID = m[1];
        const entryName = m[2];
        const isEntriesCollection = path.endsWith("/secrets");
        const isEntry = !!entryName;
        const caller = await authenticate(req, env);
        if (!caller) return json({ error: "missing or invalid API key" }, 401);
        if (isEntry) {
          if (req.method === "PUT") return secretStores.setEntry(req, env, caller, storeID, entryName);
          if (req.method === "DELETE") return secretStores.deleteEntry(req, env, caller, storeID, entryName);
          return json({ error: "method not allowed" }, 405);
        }
        if (isEntriesCollection) {
          if (req.method === "GET") return secretStores.listEntries(req, env, caller, storeID);
          return json({ error: "method not allowed" }, 405);
        }
        if (req.method === "GET") return secretStores.getStore(req, env, caller, storeID);
        if (req.method === "PUT") return secretStores.updateStore(req, env, caller, storeID);
        if (req.method === "DELETE") return secretStores.deleteStore(req, env, caller, storeID);
        return json({ error: "method not allowed" }, 405);
      }
    }

    // Auth flow (browser).
    if (path === "/auth/login")    { if (req.method === "GET")  return authLogin(req, env); }
    if (path === "/auth/callback") { if (req.method === "GET")  return authCallback(req, env); }
    if (path === "/auth/logout")   { if (req.method === "POST") return authLogout(); }
    if (path === "/auth/refresh")  { if (req.method === "POST") return authRefresh(req, env); }

    // Stripe webhook (test mode in app2, live in app).
    if (path === "/webhooks/stripe" && req.method === "POST") return stripeWebhook(req, env);

    // Dashboard API — everything under /api/dashboard/*. Edge-native handlers
    // back D1 reads/writes; sandbox-runtime calls proxy to the sandbox's cell.
    // Auth via the oc_session cookie minted at /auth/callback.
    if (path.startsWith("/api/dashboard")) {
      return handleDashboard(req, env, ctx, path);
    }

    // /api/sandboxes and /api/sandboxes/:id[/...]
    if (path === "/api/sandboxes") {
      if (req.method === "POST") return createSandbox(req, env);
      if (req.method === "GET") return listSandboxes(req, env);
      return json({ error: "method not allowed" }, 405);
    }

    // /api/sandboxes/from-checkpoint/{checkpointID} — spawn a new sandbox
    // from a checkpoint. Routing differs from regular sandbox-scoped ops
    // because the URL has no sandbox_id; we look up the cell from
    // checkpoints_index via the checkpoint UUID. The CP-side handler
    // (createFromCheckpoint) then pulls the checkpoint disks from Tigris
    // and boots a sandbox in the owning cell.
    {
      const fc = path.match(/^\/api\/sandboxes\/from-checkpoint\/([^/]+)$/);
      if (fc && req.method === "POST") {
        const caller = await authenticate(req, env);
        if (!caller) return json({ error: "missing or invalid API key" }, 401);
        const cpID = fc[1];
        const cpRow = await env.OPENCOMPUTER_DB.prepare(
          `SELECT owner_cell_id, org_id FROM checkpoints_index WHERE id = ?1`,
        )
          .bind(cpID)
          .first<{ owner_cell_id: string; org_id: string }>();
        if (!cpRow) return json({ error: "checkpoint not found" }, 404);
        if (cpRow.org_id !== caller.orgID) return json({ error: "checkpoint not in your org" }, 403);
        const cell = await lookupCell(env, cpRow.owner_cell_id);
        if (!cell) return json({ error: `cell ${cpRow.owner_cell_id} not registered` }, 503);
        const orgRow = await env.OPENCOMPUTER_DB.prepare("SELECT plan FROM orgs WHERE id = ?1")
          .bind(caller.orgID).first<{ plan: string }>();
        const token = await mintCapToken(env.SESSION_JWT_SECRET, caller.orgID, cpRow.owner_cell_id, orgRow?.plan ?? "free", caller.userID);
        const fwd = new Request(cell.base_url.replace(/\/$/, "") + path, req);
        fwd.headers.set("authorization", "Bearer " + token);
        fwd.headers.delete("x-api-key");
        return await fetch(fwd);
      }
    }

    const m = path.match(/^\/api\/sandboxes\/([^/]+)(\/.*)?$/);
    if (m) {
      const id = m[1];
      const rest = m[2]; // undefined for /api/sandboxes/:id, "/exec/run" etc otherwise
      if (!rest) {
        if (req.method === "GET") return getSandbox(req, env, id);
        if (req.method === "DELETE") {
          const caller = await authenticate(req, env);
          if (!caller) return json({ error: "missing or invalid API key" }, 401);
          return proxyToCellSDK(req, env, ctx, caller, id);
        }
        return json({ error: "method not allowed" }, 405);
      }
      // Anything under /:id/* (exec, files, pty, hibernate, …) lives on the
      // cell — proxy with an edge-minted IdentityToken (the cell's existing
      // API-key middleware accepts that JWT shape) so we don't depend on the
      // SDK's api-key existing in cell PG.
      const caller = await authenticate(req, env);
      if (!caller) return json({ error: "missing or invalid API key" }, 401);
      return proxyToCellSDK(req, env, ctx, caller, id);
    }

    // Anything not matched above is the dashboard SPA — delegate to the
    // assets binding. run_worker_first=true in wrangler.toml means CF runs
    // this Worker before checking assets, so we have to explicitly hand
    // requests off here. The assets binding's not_found_handling=
    // "single-page-application" serves index.html for client-side routes.
    if (env.ASSETS) return env.ASSETS.fetch(req);
    return new Response("not found", { status: 404 });
  },
} satisfies ExportedHandler<Env>;
