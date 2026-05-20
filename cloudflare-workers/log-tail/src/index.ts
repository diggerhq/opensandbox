// Tail Worker — subscribes to console output and exception traces from
// other Workers in this account and POSTs them as JSON events to Axiom.
//
// Wire it up by adding to each producer Worker's wrangler.toml:
//   [[tail_consumers]]
//   service = "opensandbox-log-tail"
//
// Records are shaped to match the structure the cell/worker Go services
// already emit (level, msg, service, ...) so the dataset stays one schema
// regardless of producer.

interface Env {
  AXIOM_HOST: string;
  AXIOM_DATASET: string;
  AXIOM_TOKEN: string;
}

interface TraceLog {
  timestamp: number;
  level: "log" | "info" | "warn" | "error" | "debug";
  message: unknown[];
}

interface TraceException {
  timestamp: number;
  name: string;
  message: string;
  stack?: string;
}

interface TraceItem {
  scriptName: string;
  outcome: "ok" | "exception" | "exceededCpu" | "exceededMemory" | "canceled" | "unknown";
  eventTimestamp?: number;
  event?: {
    request?: { url?: string; method?: string };
    response?: { status?: number };
    cron?: string;
    [k: string]: unknown;
  };
  logs: TraceLog[];
  exceptions: TraceException[];
  scriptVersion?: { id?: string };
}

function fmtMessage(parts: unknown[]): string {
  return parts
    .map((p) => (typeof p === "string" ? p : safeStringify(p)))
    .join(" ");
}

function safeStringify(v: unknown): string {
  try { return JSON.stringify(v); }
  catch { return String(v); }
}

// Map CF console level → Axiom-friendly level string matching what our Go
// slog handler emits ("INFO", "WARN", "ERROR", "DEBUG").
function level(l: TraceLog["level"]): string {
  switch (l) {
    case "error": return "ERROR";
    case "warn":  return "WARN";
    case "debug": return "DEBUG";
    default:      return "INFO";
  }
}

export default {
  async tail(items: TraceItem[], env: Env, _ctx: ExecutionContext): Promise<void> {
    const records: Record<string, unknown>[] = [];

    for (const item of items) {
      const baseEnvelope = {
        service: item.scriptName,
        service_id: item.scriptName,
        cell_id: "cf-edge",
        region: "global",
        outcome: item.outcome,
        request_url: item.event?.request?.url,
        request_method: item.event?.request?.method,
        response_status: item.event?.response?.status,
        cron: item.event?.cron,
        script_version: item.scriptVersion?.id,
      };

      for (const log of item.logs) {
        records.push({
          _time: new Date(log.timestamp).toISOString(),
          time: new Date(log.timestamp).toISOString(),
          level: level(log.level),
          msg: fmtMessage(log.message),
          ...baseEnvelope,
        });
      }

      for (const ex of item.exceptions) {
        records.push({
          _time: new Date(ex.timestamp).toISOString(),
          time: new Date(ex.timestamp).toISOString(),
          level: "ERROR",
          msg: `${ex.name}: ${ex.message}`,
          stack: ex.stack,
          ...baseEnvelope,
        });
      }

      // Synthetic record when a request ended in exception/exceededCpu/etc
      // but had no console output — gives us a row to count "failed
      // requests by script" in Axiom without joining streams.
      if (item.outcome !== "ok" && item.logs.length === 0 && item.exceptions.length === 0) {
        records.push({
          _time: new Date(item.eventTimestamp ?? Date.now()).toISOString(),
          time: new Date(item.eventTimestamp ?? Date.now()).toISOString(),
          level: "ERROR",
          msg: `worker request ended with outcome=${item.outcome}`,
          ...baseEnvelope,
        });
      }
    }

    if (records.length === 0) return;

    const url = `${env.AXIOM_HOST}/v1/datasets/${env.AXIOM_DATASET}/ingest`;
    const resp = await fetch(url, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${env.AXIOM_TOKEN}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify(records),
    });
    if (!resp.ok) {
      const body = await resp.text().catch(() => "");
      console.error(`axiom ingest failed status=${resp.status} body=${body.slice(0, 200)}`);
    }
  },
};
