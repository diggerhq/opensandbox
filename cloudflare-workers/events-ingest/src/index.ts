// events-ingest Worker — accepts HMAC-signed event batches from regional CPs.
//
// Pipeline:
//   1. Verify HMAC signature + X-Timestamp freshness (±5 min)
//   2. Parse batch (JSON array of SandboxEvent envelopes)
//   3. D1 batch insert into events table (events.id PRIMARY KEY +
//      ON CONFLICT DO NOTHING is the dedup — KV was redundant and the
//      get() volume blew the daily KV quota, taking down ingest)
//   4. R2 archive raw batch at raw/{cell_id}/{yyyy-mm-dd}/{batch_id}.json.gz
//   5. DO /debit fan-out for free-tier usage_tick events (out-of-band via
//      waitUntil — never blocks the ack so CP forwarder isn't stalled by
//      cross-DO traffic latency)
//   6. Return 202 {accepted}

export interface Env {
  OPENCOMPUTER_DB: D1Database;
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

export default {
  async fetch(req: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
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
      return jsonResponse({ accepted: 0 }, 202);
    }

    // Dedup is handled by D1: events.id is PRIMARY KEY and the INSERT uses
    // ON CONFLICT(id) DO NOTHING, so replays are silently dropped at the
    // storage layer. We used to also gate behind a KV `seen:{id}` lookup,
    // but the per-request get() spent ~95% of the workers-KV daily cap and
    // ended up failing every single ingest call. Trust D1.
    const fresh = envelopes;

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

    // cell_capacity events: UPDATE the cells row so api-edge's pickCell() sees
    // fresh placement metrics. The WHERE clause makes the write **monotonic** —
    // stale retries (e.g. an out-of-order XAUTOCLAIM replay of an older event)
    // become no-ops because capacity_updated_at can only move forward in time.
    // Without this guard a stale retry could briefly clobber the row with old
    // values, making the edge cascade-fall-through a cell that's actually fine.
    //
    // Timestamp is the *event emit time* from the CP's envelope, not events-
    // ingest processing time — otherwise the comparison wouldn't be apples-to-
    // apples across retries that arrive at different times.
    const capUpdate = env.OPENCOMPUTER_DB.prepare(
      `UPDATE cells SET healthy_workers = ?1, available_workers = ?2,
                        running_sandboxes = ?3, capacity_updated_at = ?4
         WHERE cell_id = ?5
           AND (capacity_updated_at IS NULL OR capacity_updated_at < ?4)`,
    );

    // Sandbox lifecycle events: keep sandboxes_index in sync with cell-side
    // state changes so the dashboard's cross-cell view is accurate without a
    // separate reconciler. monotonic WHERE clause guards against out-of-order
    // delivery (XAUTOCLAIM replays, network retries) — last_event_at can only
    // move forward.
    //
    // - stopped:    terminal; stamp stopped_at.
    // - hibernated: not terminal; stopped_at stays null so wake works.
    // - running:    create/wake; clear stopped_at to handle the
    //               hibernated → running flip after a wake.
    const lifecycleStopped = env.OPENCOMPUTER_DB.prepare(
      `UPDATE sandboxes_index
          SET status = 'stopped', stopped_at = ?1, last_event_at = ?1
        WHERE id = ?2 AND (last_event_at IS NULL OR last_event_at < ?1)`,
    );
    const lifecycleHibernated = env.OPENCOMPUTER_DB.prepare(
      `UPDATE sandboxes_index
          SET status = 'hibernated', last_event_at = ?1
        WHERE id = ?2 AND (last_event_at IS NULL OR last_event_at < ?1)`,
    );
    const lifecycleRunning = env.OPENCOMPUTER_DB.prepare(
      `UPDATE sandboxes_index
          SET status = 'running', stopped_at = NULL, last_event_at = ?1
        WHERE id = ?2 AND (last_event_at IS NULL OR last_event_at < ?1)`,
    );
    const capacityBatches = fresh
      .filter((e) => e.type === "cell_capacity")
      .map((e) => {
        const p = (e.payload ?? {}) as {
          healthy_workers?: number;
          available_workers?: number;
          running_sandboxes?: number;
        };
        const tsMs = Date.parse(e.timestamp) || Date.now();
        const tsSec = Math.floor(tsMs / 1000);
        return capUpdate.bind(
          p.healthy_workers ?? 0,
          p.available_workers ?? 0,
          p.running_sandboxes ?? 0,
          tsSec,
          e.cell_id,
        );
      });

    const lifecycleBatches = fresh
      .filter((e) => e.sandbox_id && (e.type === "stopped" || e.type === "hibernated" || e.type === "running" || e.type === "woke" || e.type === "created"))
      .map((e) => {
        const tsSec = Math.floor((Date.parse(e.timestamp) || Date.now()) / 1000);
        if (e.type === "stopped") return lifecycleStopped.bind(tsSec, e.sandbox_id);
        if (e.type === "hibernated") return lifecycleHibernated.bind(tsSec, e.sandbox_id);
        // "running", "woke", "created" all set the row to running (created
        // is handled by the edge on POST, but accept here for redundancy
        // in case of replay against a future cell-only create path).
        return lifecycleRunning.bind(tsSec, e.sandbox_id);
      });

    try {
      await env.OPENCOMPUTER_DB.batch([...batches, ...capacityBatches, ...lifecycleBatches]);
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

    // DO /debit fan-out — for free-tier usage_tick events only. Each event
    // routes to a per-org DO instance; we hand off to waitUntil so cross-DO
    // latency never holds the CP forwarder's ack.
    //
    // Pro orgs are filtered here (cheap) rather than in the DO so we avoid
    // even allocating the DO stub for orgs that don't need debit accounting.
    // Events without plan="free" are silently skipped — the CP forwarder
    // populates `plan` from PG at emit time.
    const debitTargets = fresh.filter((e) => e.type === "usage_tick" && e.plan === "free" && e.org_id);
    if (debitTargets.length > 0) {
      ctx.waitUntil(fanoutDebits(env, debitTargets));
    }

    return jsonResponse({ accepted: fresh.length }, 202);
  },
} satisfies ExportedHandler<Env>;

// fanoutDebits dispatches one /debit per usage_tick event to its org's DO.
// Per-event errors are swallowed — DO is idempotent on event_id (LRU dedup)
// so the CP forwarder's automatic retry covers transient DO unavailability
// without risking double-debit.
async function fanoutDebits(env: Env, events: SandboxEventEnvelope[]): Promise<void> {
  await Promise.all(
    events.map(async (e) => {
      try {
        const id = env.CREDIT_ACCOUNT.idFromName(e.org_id!);
        const stub = env.CREDIT_ACCOUNT.get(id);
        const payload = (e.payload ?? {}) as { cost_cents?: number };
        const body = JSON.stringify({
          event_id: e.id,
          amount_cents: payload.cost_cents ?? 0,
        });
        const resp = await stub.fetch(`https://do/debit?org_id=${encodeURIComponent(e.org_id!)}`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body,
        });
        if (resp.status >= 400) {
          console.error(`events-ingest: DO /debit ${e.org_id} returned ${resp.status} (event ${e.id})`);
        }
      } catch (err) {
        console.error(`events-ingest: DO /debit ${e.org_id} threw`, err);
      }
    }),
  );
}

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

// Returns gzipped bytes as a Uint8Array. R2 requires a known length on
// the body — a raw pipeThrough(CompressionStream) ReadableStream has
// unknown length and trips "Provided readable stream must have a known
// length". Buffering into a Uint8Array gives R2 a length without a
// streaming primitive.
async function gzip(input: string): Promise<Uint8Array> {
  const body = new Response(input).body;
  if (!body) throw new Error("body stream null");
  const compressed = body.pipeThrough(new CompressionStream("gzip"));
  return new Uint8Array(await new Response(compressed).arrayBuffer());
}
