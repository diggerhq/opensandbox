// events-ingest Worker — accepts HMAC-signed event batches from regional CPs.
//
// Pipeline:
//   1. Verify HMAC signature + X-Timestamp freshness (±5 min)
//   2. Parse batch (JSON array of SandboxEvent envelopes)
//   3. KV dedup via seen:{event_id}
//   4. D1 batch insert into events table
//   5. R2 archive raw batch at raw/{cell_id}/{yyyy-mm-dd}/{batch_id}.json.gz
//   6. KV put seen:{event_id} TTL 24h for each accepted event
//   7. Return 202 {accepted, deduped}
//
// Stage 1 stops here. The DO /debit fan-out for free-tier usage_tick events
// is added in Stage 2.

export interface Env {
  OPENCOMPUTER_DB: D1Database;
  SESSIONS_KV: KVNamespace;
  EVENTS_ARCHIVE: R2Bucket;
  CREDIT_ACCOUNT: DurableObjectNamespace;
  EVENT_SECRET: string;
  CF_ADMIN_SECRET: string;
  WORKER_ENV: string;
}

interface SandboxEventEnvelope {
  id: string;
  type: string;
  sandbox_id: string;
  org_id?: string;
  plan?: string;
  worker_id: string;
  cell_id: string;
  payload: unknown;
  timestamp: string;
}

const SIGNATURE_WINDOW_SEC = 5 * 60;
const KV_DEDUP_TTL_SEC = 24 * 60 * 60;

export default {
  async fetch(req: Request, env: Env): Promise<Response> {
    const url = new URL(req.url);

    if (url.pathname === "/health") {
      return jsonResponse({ ok: true, env: env.WORKER_ENV });
    }

    if (url.pathname !== "/ingest" || req.method !== "POST") {
      return new Response("not found", { status: 404 });
    }

    const cellId = req.headers.get("X-Cell-Id") ?? "";
    const tsHeader = req.headers.get("X-Timestamp") ?? "";
    const sigHeader = req.headers.get("X-Signature") ?? "";

    if (!cellId || !tsHeader || !sigHeader) {
      return jsonResponse({ error: "missing signature headers" }, 400);
    }

    // Read raw body once — needed both for signature verification and parsing.
    const bodyText = await req.text();

    const ts = Number.parseInt(tsHeader, 10);
    if (!Number.isFinite(ts)) {
      return jsonResponse({ error: "invalid timestamp" }, 400);
    }
    const now = Math.floor(Date.now() / 1000);
    if (Math.abs(now - ts) > SIGNATURE_WINDOW_SEC) {
      return jsonResponse({ error: "timestamp out of window" }, 401);
    }

    const expected = await hmacHex(env.EVENT_SECRET, `${ts}.${bodyText}`);
    if (!constantTimeEqual(expected, sigHeader)) {
      return jsonResponse({ error: "signature mismatch" }, 401);
    }

    let envelopes: SandboxEventEnvelope[];
    try {
      const parsed = JSON.parse(bodyText);
      if (!Array.isArray(parsed)) {
        return jsonResponse({ error: "body must be a JSON array" }, 400);
      }
      envelopes = parsed as SandboxEventEnvelope[];
    } catch (err) {
      return jsonResponse({ error: "invalid JSON" }, 400);
    }
    if (envelopes.length === 0) {
      return jsonResponse({ accepted: 0, deduped: 0 }, 202);
    }

    // KV dedup. Each get is a separate edge round-trip but parallelizable.
    const seenChecks = await Promise.all(
      envelopes.map((e) => env.SESSIONS_KV.get(`seen:${e.id}`)),
    );
    const fresh: SandboxEventEnvelope[] = [];
    let deduped = 0;
    for (let i = 0; i < envelopes.length; i++) {
      if (seenChecks[i]) {
        deduped++;
      } else {
        fresh.push(envelopes[i]);
      }
    }

    if (fresh.length === 0) {
      return jsonResponse({ accepted: 0, deduped }, 202);
    }

    // D1 batch insert. 11 columns: id, cell_id, type, org_id, sandbox_id,
    // user_id (null for now — not in envelope), worker_id, ts (unix ms), payload.
    const stmt = env.OPENCOMPUTER_DB.prepare(
      `INSERT INTO events (id, cell_id, type, org_id, sandbox_id, user_id, worker_id, ts, payload)
       VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?)
       ON CONFLICT(id) DO NOTHING`,
    );
    const batches = fresh.map((e) =>
      stmt.bind(
        e.id,
        e.cell_id,
        e.type,
        e.org_id ?? null,
        e.sandbox_id ?? null,
        e.worker_id ?? null,
        Date.parse(e.timestamp) || Date.now(),
        JSON.stringify(e.payload ?? {}),
      ),
    );
    try {
      await env.OPENCOMPUTER_DB.batch(batches);
    } catch (err) {
      // Database errors are retryable — return 5xx so the CP forwarder
      // leaves the batch in the PEL.
      console.error("events-ingest: D1 batch insert failed", err);
      return jsonResponse({ error: "db insert failed" }, 503);
    }

    // R2 archive — gzipped raw batch. Best effort: archive failure does not
    // block the ack (events are already in D1, which is the durable record).
    try {
      const date = new Date().toISOString().slice(0, 10);
      const batchId = `${ts}-${crypto.randomUUID()}`;
      const key = `raw/${cellId}/${date}/${batchId}.json.gz`;
      const gzipped = await gzip(bodyText);
      await env.EVENTS_ARCHIVE.put(key, gzipped, {
        httpMetadata: { contentType: "application/gzip" },
      });
    } catch (err) {
      console.error("events-ingest: R2 archive failed (continuing)", err);
    }

    // KV dedup markers (24h TTL) — fire and forget per event.
    await Promise.all(
      fresh.map((e) =>
        env.SESSIONS_KV.put(`seen:${e.id}`, "1", { expirationTtl: KV_DEDUP_TTL_SEC }),
      ),
    );

    return jsonResponse({ accepted: fresh.length, deduped }, 202);
  },
} satisfies ExportedHandler<Env>;

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
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
  for (let i = 0; i < a.length; i++) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}

async function gzip(input: string): Promise<ReadableStream<Uint8Array>> {
  const stream = new Response(input).body;
  if (!stream) throw new Error("body stream null");
  return stream.pipeThrough(new CompressionStream("gzip"));
}
