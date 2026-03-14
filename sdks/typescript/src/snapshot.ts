import { Image } from "./image.js";
import { parseSSEStream } from "./sse.js";

function resolveApiUrl(url: string): string {
  const base = url.replace(/\/+$/, "");
  return base.endsWith("/api") ? base : `${base}/api`;
}

export interface SnapshotInfo {
  id: string;
  orgId: string;
  name: string;
  contentHash: string;
  checkpointId: string;
  status: string;
  manifest: Record<string, unknown>;
  createdAt: string;
  lastUsedAt: string;
}

export interface CreateSnapshotOpts {
  name: string;
  image: Image;
  onBuildLogs?: (log: string) => void;
}

export interface SnapshotOpts {
  apiKey?: string;
  apiUrl?: string;
}

/**
 * Manage pre-built snapshots (named, persistent image checkpoints).
 */
export class Snapshots {
  private apiUrl: string;
  private apiKey: string;

  constructor(opts: SnapshotOpts = {}) {
    this.apiUrl = resolveApiUrl(
      opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev"
    );
    this.apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";
  }

  private get headers(): Record<string, string> {
    const h: Record<string, string> = { "Content-Type": "application/json" };
    if (this.apiKey) h["X-API-Key"] = this.apiKey;
    return h;
  }

  /**
   * Create a pre-built snapshot from a declarative image.
   * The server boots a sandbox, runs the image steps, checkpoints it, and stores it under the given name.
   */
  async create(opts: CreateSnapshotOpts): Promise<SnapshotInfo> {
    const headers: Record<string, string> = { ...this.headers };
    if (opts.onBuildLogs) {
      headers["Accept"] = "text/event-stream";
    }

    const resp = await fetch(`${this.apiUrl}/snapshots`, {
      method: "POST",
      headers,
      body: JSON.stringify({
        name: opts.name,
        image: opts.image.toJSON(),
      }),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create snapshot: ${resp.status} ${text}`);
    }

    if (opts.onBuildLogs && resp.headers.get("content-type")?.includes("text/event-stream")) {
      return parseSSEStream<SnapshotInfo>(resp, opts.onBuildLogs);
    }

    return resp.json();
  }

  /**
   * List all named snapshots for the current org.
   */
  async list(): Promise<SnapshotInfo[]> {
    const resp = await fetch(`${this.apiUrl}/snapshots`, {
      headers: this.headers,
    });

    if (!resp.ok) {
      throw new Error(`Failed to list snapshots: ${resp.status}`);
    }

    return resp.json();
  }

  /**
   * Get a snapshot by name.
   */
  async get(name: string): Promise<SnapshotInfo> {
    const resp = await fetch(`${this.apiUrl}/snapshots/${encodeURIComponent(name)}`, {
      headers: this.headers,
    });

    if (!resp.ok) {
      throw new Error(`Failed to get snapshot: ${resp.status}`);
    }

    return resp.json();
  }

  /**
   * Delete a named snapshot.
   */
  async delete(name: string): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/snapshots/${encodeURIComponent(name)}`, {
      method: "DELETE",
      headers: this.headers,
    });

    if (!resp.ok && resp.status !== 404) {
      throw new Error(`Failed to delete snapshot: ${resp.status}`);
    }
  }
}
