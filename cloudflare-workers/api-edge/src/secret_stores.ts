// Edge-side secret stores — replaces the legacy per-cell PG-backed
// /api/secret-stores routes. Stores live in D1; entries are envelope-
// encrypted at the edge with SECRET_ENCRYPTION_KEY (Infisical /shared/,
// also distributed to every CP). The CP fetches encrypted blobs from the
// /internal/secret-stores/:id HMAC endpoint at sandbox-create time and
// decrypts inside the cell using internal/crypto.
//
// JSON shapes mirror internal/api/projects.go (camelCase) so the existing
// CLI/SDK and dashboard talk to the edge unchanged.

import { encryptSecret } from "./crypto";

export interface SecretStoresEnv {
  OPENCOMPUTER_DB: D1Database;
  SECRET_ENCRYPTION_KEY: string;
  EVENT_SECRET: string;
}

interface Caller {
  orgID: string;
  userID: string | null;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

// ── store shape mapping ──────────────────────────────────────────────────

interface StoreRow {
  id: string;
  org_id: string;
  name: string;
  egress_allowlist: string;
  created_at: number;
  updated_at: number;
}

function storeToJSON(row: StoreRow): Record<string, unknown> {
  let allowlist: string[] = [];
  try {
    allowlist = JSON.parse(row.egress_allowlist) as string[];
  } catch {
    /* malformed JSON in D1 — degrade to empty list rather than 500 */
  }
  return {
    id: row.id,
    orgID: row.org_id,
    name: row.name,
    egressAllowlist: allowlist,
    // ISO strings (not unix seconds) — matches the legacy CP /api/secret-stores
    // shape that the Go CLI's struct expects. Storing seconds in D1 is fine;
    // serialize for the wire.
    createdAt: new Date(row.created_at * 1000).toISOString(),
    updatedAt: new Date(row.updated_at * 1000).toISOString(),
  };
}

// ── CRUD: stores ────────────────────────────────────────────────────────

export async function createStore(req: Request, env: SecretStoresEnv, caller: Caller): Promise<Response> {
  const body = (await req.json().catch(() => null)) as { name?: string; egressAllowlist?: string[] } | null;
  if (!body || !body.name) return json({ error: "name is required" }, 400);
  const allowlist = Array.isArray(body.egressAllowlist) ? body.egressAllowlist : [];

  const id = crypto.randomUUID();
  const now = Math.floor(Date.now() / 1000);
  try {
    await env.OPENCOMPUTER_DB.prepare(
      `INSERT INTO secret_stores (id, org_id, name, egress_allowlist, created_at, updated_at)
       VALUES (?1, ?2, ?3, ?4, ?5, ?5)`,
    )
      .bind(id, caller.orgID, body.name, JSON.stringify(allowlist), now)
      .run();
  } catch (e) {
    // UNIQUE(org_id, name) collision → 409
    const msg = (e as Error).message ?? "";
    if (/UNIQUE/.test(msg)) return json({ error: "a store with that name already exists" }, 409);
    throw e;
  }
  return json(
    storeToJSON({ id, org_id: caller.orgID, name: body.name, egress_allowlist: JSON.stringify(allowlist), created_at: now, updated_at: now }),
    201,
  );
}

export async function listStores(_req: Request, env: SecretStoresEnv, caller: Caller): Promise<Response> {
  const { results } = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, org_id, name, egress_allowlist, created_at, updated_at
       FROM secret_stores WHERE org_id = ?1 ORDER BY name ASC`,
  )
    .bind(caller.orgID)
    .all<StoreRow>();
  return json((results ?? []).map(storeToJSON));
}

async function loadStore(env: SecretStoresEnv, orgID: string, id: string): Promise<StoreRow | null> {
  return env.OPENCOMPUTER_DB.prepare(
    `SELECT id, org_id, name, egress_allowlist, created_at, updated_at
       FROM secret_stores WHERE id = ?1 AND org_id = ?2`,
  )
    .bind(id, orgID)
    .first<StoreRow>();
}

export async function getStore(_req: Request, env: SecretStoresEnv, caller: Caller, id: string): Promise<Response> {
  const row = await loadStore(env, caller.orgID, id);
  if (!row) return json({ error: "secret store not found" }, 404);
  return json(storeToJSON(row));
}

export async function updateStore(req: Request, env: SecretStoresEnv, caller: Caller, id: string): Promise<Response> {
  const existing = await loadStore(env, caller.orgID, id);
  if (!existing) return json({ error: "secret store not found" }, 404);
  const body = (await req.json().catch(() => null)) as { name?: string | null; egressAllowlist?: string[] | null } | null;

  const name = typeof body?.name === "string" ? body.name : existing.name;
  const allowlist = Array.isArray(body?.egressAllowlist) ? body!.egressAllowlist! : (JSON.parse(existing.egress_allowlist) as string[]);
  const now = Math.floor(Date.now() / 1000);
  await env.OPENCOMPUTER_DB.prepare(
    `UPDATE secret_stores SET name = ?1, egress_allowlist = ?2, updated_at = ?3
      WHERE id = ?4 AND org_id = ?5`,
  )
    .bind(name, JSON.stringify(allowlist), now, id, caller.orgID)
    .run();
  return json(
    storeToJSON({ id, org_id: caller.orgID, name, egress_allowlist: JSON.stringify(allowlist), created_at: existing.created_at, updated_at: now }),
  );
}

export async function deleteStore(_req: Request, env: SecretStoresEnv, caller: Caller, id: string): Promise<Response> {
  const row = await loadStore(env, caller.orgID, id);
  if (!row) return json({ error: "secret store not found" }, 404);
  // Cascade-delete entries (D1 doesn't enforce FK CASCADE — do it explicitly).
  await env.OPENCOMPUTER_DB.batch([
    env.OPENCOMPUTER_DB.prepare(`DELETE FROM secret_store_entries WHERE store_id = ?1`).bind(id),
    env.OPENCOMPUTER_DB.prepare(`DELETE FROM secret_stores WHERE id = ?1 AND org_id = ?2`).bind(id, caller.orgID),
  ]);
  return new Response(null, { status: 204 });
}

// ── CRUD: entries ──────────────────────────────────────────────────────

interface EntryRow {
  id: string;
  store_id: string;
  name: string;
  encrypted_value: ArrayBuffer | Uint8Array;
  allowed_hosts: string;
  created_at: number;
  updated_at: number;
}

function entryToJSON(row: EntryRow, includeValue = false): Record<string, unknown> {
  let hosts: string[] = [];
  try {
    hosts = JSON.parse(row.allowed_hosts) as string[];
  } catch {
    /* malformed JSON */
  }
  const out: Record<string, unknown> = {
    name: row.name,
    allowedHosts: hosts,
    createdAt: new Date(row.created_at * 1000).toISOString(),
    updatedAt: new Date(row.updated_at * 1000).toISOString(),
  };
  if (includeValue) {
    const bytes = row.encrypted_value instanceof Uint8Array ? row.encrypted_value : new Uint8Array(row.encrypted_value);
    out.encryptedValueB64 = btoa(String.fromCharCode(...bytes));
  }
  return out;
}

export async function setEntry(
  req: Request,
  env: SecretStoresEnv,
  caller: Caller,
  storeID: string,
  name: string,
): Promise<Response> {
  if (!name) return json({ error: "secret name required" }, 400);
  const store = await loadStore(env, caller.orgID, storeID);
  if (!store) return json({ error: "secret store not found" }, 404);

  const body = (await req.json().catch(() => null)) as { value?: string; allowedHosts?: string[] } | null;
  if (!body || typeof body.value !== "string" || body.value === "") {
    return json({ error: 'secret value required: {"value": "...", "allowedHosts": [...]}' }, 400);
  }
  const hosts = Array.isArray(body.allowedHosts) ? body.allowedHosts.map((h) => h.trim()) : [];
  if (hosts.some((h) => h === "")) return json({ error: "allowedHosts entries cannot be empty" }, 400);

  const encrypted = await encryptSecret(env.SECRET_ENCRYPTION_KEY, body.value);
  const now = Math.floor(Date.now() / 1000);
  const id = crypto.randomUUID();
  // UPSERT on (store_id, name).
  await env.OPENCOMPUTER_DB.prepare(
    `INSERT INTO secret_store_entries (id, store_id, name, encrypted_value, allowed_hosts, created_at, updated_at)
     VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?6)
     ON CONFLICT(store_id, name) DO UPDATE SET
       encrypted_value = excluded.encrypted_value,
       allowed_hosts   = excluded.allowed_hosts,
       updated_at      = excluded.updated_at`,
  )
    .bind(id, storeID, name, encrypted, JSON.stringify(hosts), now)
    .run();

  // Fan the new value out to running sandboxes via /internal/secret-refresh
  // on every active cell. Each CP checks its local sandbox_sessions for the
  // store-binding column and pushes the new value to the right workers over
  // gRPC, so live sandboxes don't need a restart to pick up the change.
  // Wire format: encrypted blob over HMAC; CP decrypts with its copy of the
  // shared SECRET_ENCRYPTION_KEY so plaintext never crosses the cell boundary.
  const fanout = await fanoutSecretRefresh(env, storeID, name, encrypted);
  return json({ name, status: "set", refreshed: fanout.refreshed, cellsContacted: fanout.cells, failures: fanout.failures });
}

// fanoutSecretRefresh queries D1 for active cells, signs an HMAC POST per
// cell with the encrypted blob, and aggregates the per-cell refresh counts.
// Total time-bounded: a slow cell can't stall the user's PUT past ~12s.
async function fanoutSecretRefresh(
  env: SecretStoresEnv,
  storeID: string,
  name: string,
  encryptedValue: Uint8Array,
): Promise<{ refreshed: number; cells: number; failures: string[] }> {
  const { results: cells } = await env.OPENCOMPUTER_DB.prepare(
    `SELECT cell_id, base_url FROM cells WHERE status = 'active'`,
  ).all<{ cell_id: string; base_url: string }>();
  const active = cells ?? [];
  if (active.length === 0) return { refreshed: 0, cells: 0, failures: [] };

  const encryptedB64 = btoa(String.fromCharCode(...encryptedValue));
  const body = JSON.stringify({ storeId: storeID, name, encryptedValueB64: encryptedB64 });
  const ts = Math.floor(Date.now() / 1000).toString();

  // Sign once — every cell shares EVENT_SECRET so the same signature works
  // across the fan-out. Body is part of the signed payload to prevent body
  // swap, matching the AdminAuth scheme on the CP side ("{ts}.{body}").
  const sig = await hmacBody(env.EVENT_SECRET, ts, body);

  // Per-cell timeout — bounded so one slow cell doesn't block the others.
  // The CP-side fanoutSecretRefresh already has its own 15s ceiling against
  // worker gRPC calls; this is the outer bound the user's PUT waits on.
  const perCellTimeoutMs = 12_000;

  const settled = await Promise.allSettled(
    active.map(async (c) => {
      const ctrl = new AbortController();
      const t = setTimeout(() => ctrl.abort(), perCellTimeoutMs);
      try {
        const resp = await fetch(c.base_url.replace(/\/$/, "") + "/internal/secret-refresh", {
          method: "POST",
          headers: {
            "content-type": "application/json",
            "X-Timestamp": ts,
            "X-Signature": sig,
          },
          body,
          signal: ctrl.signal,
        });
        const text = await resp.text();
        if (resp.status !== 200) {
          return { cell: c.cell_id, refreshed: 0, error: `status ${resp.status}: ${text.slice(0, 200)}` };
        }
        const parsed = JSON.parse(text) as { refreshed?: number; failures?: string[] };
        return { cell: c.cell_id, refreshed: parsed.refreshed ?? 0, error: null as string | null };
      } catch (e) {
        return { cell: c.cell_id, refreshed: 0, error: (e as Error).message };
      } finally {
        clearTimeout(t);
      }
    }),
  );

  let totalRefreshed = 0;
  const failures: string[] = [];
  for (const s of settled) {
    if (s.status === "fulfilled") {
      totalRefreshed += s.value.refreshed;
      if (s.value.error) failures.push(`${s.value.cell}: ${s.value.error}`);
    } else {
      failures.push(`unknown cell: ${s.reason}`);
    }
  }
  return { refreshed: totalRefreshed, cells: active.length, failures };
}

// hmacBody returns hex(HMAC-SHA256(secret, ts + "." + body)) — matches the
// CP-side controlplane.AdminAuth verifier so /internal/secret-refresh
// accepts our signature.
async function hmacBody(secret: string, ts: string, body: string): Promise<string> {
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(`${ts}.${body}`));
  return [...new Uint8Array(sig)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export async function deleteEntry(
  _req: Request,
  env: SecretStoresEnv,
  caller: Caller,
  storeID: string,
  name: string,
): Promise<Response> {
  const store = await loadStore(env, caller.orgID, storeID);
  if (!store) return json({ error: "secret store not found" }, 404);
  const res = await env.OPENCOMPUTER_DB.prepare(
    `DELETE FROM secret_store_entries WHERE store_id = ?1 AND name = ?2`,
  )
    .bind(storeID, name)
    .run();
  if ((res.meta?.changes ?? 0) === 0) return json({ error: "secret entry not found" }, 404);
  return new Response(null, { status: 204 });
}

export async function listEntries(
  _req: Request,
  env: SecretStoresEnv,
  caller: Caller,
  storeID: string,
): Promise<Response> {
  const store = await loadStore(env, caller.orgID, storeID);
  if (!store) return json({ error: "secret store not found" }, 404);
  const { results } = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, store_id, name, encrypted_value, allowed_hosts, created_at, updated_at
       FROM secret_store_entries WHERE store_id = ?1 ORDER BY name ASC`,
  )
    .bind(storeID)
    .all<EntryRow>();
  // Never return plaintext or ciphertext on the user-facing route — names only.
  return json((results ?? []).map((r) => entryToJSON(r, false)));
}

// ── /internal/secret-stores/:id (HMAC, called by CP at sandbox-create) ──

// HMAC scheme matches /internal/halt-list: "{X-Timestamp}.{path-with-query}"
// signed with EVENT_SECRET (shared between edge + every CP), SHA-256 hex.
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

async function verifyHMAC(req: Request, env: SecretStoresEnv): Promise<boolean> {
  const ts = req.headers.get("X-Timestamp") ?? "";
  const sig = req.headers.get("X-Signature") ?? "";
  if (!ts || !sig) return false;
  const tsNum = Number.parseInt(ts, 10);
  if (!Number.isFinite(tsNum)) return false;
  if (Math.abs(Math.floor(Date.now() / 1000) - tsNum) > 5 * 60) return false;
  const url = new URL(req.url);
  const expected = await hmacHex(env.EVENT_SECRET, `${ts}.${url.pathname}${url.search}`);
  return constantTimeEqual(expected, sig);
}

async function bundleStore(env: SecretStoresEnv, store: StoreRow): Promise<Response> {
  const { results } = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, store_id, name, encrypted_value, allowed_hosts, created_at, updated_at
       FROM secret_store_entries WHERE store_id = ?1`,
  )
    .bind(store.id)
    .all<EntryRow>();
  return json({
    store: storeToJSON(store),
    entries: (results ?? []).map((r) => entryToJSON(r, true)),
  });
}

export async function internalGetStore(req: Request, env: SecretStoresEnv, storeID: string): Promise<Response> {
  if (!(await verifyHMAC(req, env))) return json({ error: "signature mismatch" }, 401);
  // No org scoping — CP already validated the cap-token whose claims pinned org_id.
  const store = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, org_id, name, egress_allowlist, created_at, updated_at FROM secret_stores WHERE id = ?1`,
  )
    .bind(storeID)
    .first<StoreRow>();
  if (!store) return json({ error: "secret store not found" }, 404);
  return bundleStore(env, store);
}

// Lookup by (org_id, name) — what CP's resolveSecretStoreInto calls with
// the user-supplied cfg.SecretStore string at sandbox-create time. Same
// HMAC scheme as internalGetStore. Returns 404 if no store matches.
export async function internalGetStoreByName(req: Request, env: SecretStoresEnv): Promise<Response> {
  if (!(await verifyHMAC(req, env))) return json({ error: "signature mismatch" }, 401);
  const url = new URL(req.url);
  const orgID = url.searchParams.get("org_id") ?? "";
  const name = url.searchParams.get("name") ?? "";
  if (!orgID || !name) return json({ error: "org_id and name are required" }, 400);
  const store = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, org_id, name, egress_allowlist, created_at, updated_at
       FROM secret_stores WHERE org_id = ?1 AND name = ?2`,
  )
    .bind(orgID, name)
    .first<StoreRow>();
  if (!store) return json({ error: "secret store not found" }, 404);
  return bundleStore(env, store);
}
