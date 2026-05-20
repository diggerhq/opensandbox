// Edge-side templates — HMAC fetch + register endpoints called by the CP.
//
// Templates have no user-facing CRUD today (no /api/templates routes); they
// are created via the CP's "save sandbox as template" snapshot flow and
// looked up by name at sandbox-create time. After migration 041 strips the
// per-cell `templates` table, both paths route through these /internal/*
// HMAC endpoints so D1 becomes the single source of truth.

export interface TemplatesEnv {
  OPENCOMPUTER_DB: D1Database;
  EVENT_SECRET: string;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

interface TemplateRow {
  id: string;
  org_id: string | null;
  name: string;
  tag: string;
  template_type: string;
  image_ref: string | null;
  rootfs_s3_key: string | null;
  workspace_s3_key: string | null;
  dockerfile: string | null;
  is_public: number;
  status: string;
  cells_available: string;
  created_at: number;
}

// Mirrors db.Template (Go) field-by-field, camelCase JSON keys to match
// existing dashboard/SDK responses.
function templateToJSON(row: TemplateRow): Record<string, unknown> {
  let cells: string[] = [];
  try {
    cells = JSON.parse(row.cells_available) as string[];
  } catch {
    /* malformed JSON — degrade to empty list */
  }
  return {
    id: row.id,
    orgID: row.org_id,
    name: row.name,
    tag: row.tag,
    templateType: row.template_type,
    imageRef: row.image_ref,
    rootfsS3Key: row.rootfs_s3_key,
    workspaceS3Key: row.workspace_s3_key,
    dockerfile: row.dockerfile,
    isPublic: !!row.is_public,
    status: row.status,
    cellsAvailable: cells,
    createdAt: row.created_at,
  };
}

// ── HMAC (matches secret_stores.ts) ─────────────────────────────────────

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

async function verifyHMAC(req: Request, env: TemplatesEnv, bodyText?: string): Promise<boolean> {
  const ts = req.headers.get("X-Timestamp") ?? "";
  const sig = req.headers.get("X-Signature") ?? "";
  if (!ts || !sig) return false;
  const tsNum = Number.parseInt(ts, 10);
  if (!Number.isFinite(tsNum)) return false;
  if (Math.abs(Math.floor(Date.now() / 1000) - tsNum) > 5 * 60) return false;
  const url = new URL(req.url);
  // POST/PUT include the body in the signed payload to prevent body-swap
  // attacks; GET signs path+query only.
  const data = bodyText !== undefined ? `${ts}.${url.pathname}${url.search}.${bodyText}` : `${ts}.${url.pathname}${url.search}`;
  const expected = await hmacHex(env.EVENT_SECRET, data);
  return constantTimeEqual(expected, sig);
}

// ── /internal/templates/by-name ─────────────────────────────────────────

// Org-scoped lookup with public fallback — mirrors db.Store.GetTemplateByName.
// Sandbox-create on the CP calls this with cfg.Template (the user-supplied
// name) and the cap-token's claims.OrgID. If the org has a private template
// with that name it wins; otherwise we fall back to the public catalog.
export async function internalGetByName(req: Request, env: TemplatesEnv): Promise<Response> {
  if (!(await verifyHMAC(req, env))) return json({ error: "signature mismatch" }, 401);
  const url = new URL(req.url);
  const orgID = url.searchParams.get("org_id") ?? "";
  const name = url.searchParams.get("name") ?? "";
  if (!name) return json({ error: "name is required" }, 400);

  // Private (org-scoped) first.
  if (orgID) {
    const row = await env.OPENCOMPUTER_DB.prepare(
      `SELECT id, org_id, name, tag, template_type, image_ref, rootfs_s3_key, workspace_s3_key,
              dockerfile, is_public, status, cells_available, created_at
         FROM templates
        WHERE org_id = ?1 AND name = ?2
        ORDER BY created_at DESC LIMIT 1`,
    )
      .bind(orgID, name)
      .first<TemplateRow>();
    if (row) return json(templateToJSON(row));
  }
  // Public fallback.
  const pub = await env.OPENCOMPUTER_DB.prepare(
    `SELECT id, org_id, name, tag, template_type, image_ref, rootfs_s3_key, workspace_s3_key,
            dockerfile, is_public, status, cells_available, created_at
       FROM templates
      WHERE is_public = 1 AND name = ?1
      ORDER BY created_at DESC LIMIT 1`,
  )
    .bind(name)
    .first<TemplateRow>();
  if (!pub) return json({ error: "template not found" }, 404);
  return json(templateToJSON(pub));
}

// ── /internal/templates (POST) — register a new template in D1 ─────────

// CP's "save sandbox as template" flow uploads the rootfs/workspace to Tigris
// (already implemented via internal/blobstore.Store), then calls this to
// register the metadata row. Replaces the local PG INSERT at
// internal/db/store.go:1750.
interface RegisterReq {
  id?: string;
  orgID?: string | null;
  name?: string;
  tag?: string;
  templateType?: string;
  imageRef?: string | null;
  rootfsS3Key?: string | null;
  workspaceS3Key?: string | null;
  dockerfile?: string | null;
  isPublic?: boolean;
  status?: string;
  cellsAvailable?: string[];
  createdBySandboxID?: string | null;
}

export async function internalRegister(req: Request, env: TemplatesEnv): Promise<Response> {
  // Read body once for both verification and parsing — POST signs payload too.
  const bodyText = await req.text();
  if (!(await verifyHMAC(req, env, bodyText))) return json({ error: "signature mismatch" }, 401);
  let body: RegisterReq;
  try {
    body = JSON.parse(bodyText) as RegisterReq;
  } catch {
    return json({ error: "invalid JSON" }, 400);
  }
  if (!body.name) return json({ error: "name is required" }, 400);

  const id = body.id ?? crypto.randomUUID();
  const tag = body.tag ?? "latest";
  const cellsJSON = JSON.stringify(body.cellsAvailable ?? []);
  const now = Math.floor(Date.now() / 1000);

  try {
    await env.OPENCOMPUTER_DB.prepare(
      `INSERT INTO templates
         (id, org_id, name, tag, template_type, image_ref, rootfs_s3_key, workspace_s3_key,
          dockerfile, is_public, status, cells_available, created_at)
       VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13)`,
    )
      .bind(
        id,
        body.orgID ?? null,
        body.name,
        tag,
        body.templateType ?? "dockerfile",
        body.imageRef ?? null,
        body.rootfsS3Key ?? null,
        body.workspaceS3Key ?? null,
        body.dockerfile ?? null,
        body.isPublic ? 1 : 0,
        body.status ?? "ready",
        cellsJSON,
        now,
      )
      .run();
  } catch (e) {
    const msg = (e as Error).message ?? "";
    if (/UNIQUE/.test(msg)) return json({ error: "template (org, name, tag) already exists" }, 409);
    throw e;
  }
  return json({ id, name: body.name, tag, status: body.status ?? "ready" }, 201);
}

// Status update — CP flips status='ready' once a snapshot finishes uploading.
// Matches db.Store.UpdateTemplateStatus.
export async function internalUpdateStatus(req: Request, env: TemplatesEnv, id: string): Promise<Response> {
  const bodyText = await req.text();
  if (!(await verifyHMAC(req, env, bodyText))) return json({ error: "signature mismatch" }, 401);
  let body: { status?: string };
  try {
    body = JSON.parse(bodyText);
  } catch {
    return json({ error: "invalid JSON" }, 400);
  }
  if (!body.status) return json({ error: "status is required" }, 400);
  const res = await env.OPENCOMPUTER_DB.prepare(`UPDATE templates SET status = ?1 WHERE id = ?2`)
    .bind(body.status, id)
    .run();
  if ((res.meta?.changes ?? 0) === 0) return json({ error: "template not found" }, 404);
  return json({ id, status: body.status });
}
