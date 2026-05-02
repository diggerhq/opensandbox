import { Agent } from "./agent.js";
import { Filesystem } from "./filesystem.js";
import { Exec } from "./exec.js";
import { Pty } from "./pty.js";
import { Image } from "./image.js";
import { parseSSEStream } from "./sse.js";

function resolveApiUrl(url: string): string {
  const base = url.replace(/\/+$/, "");
  return base.endsWith("/api") ? base : `${base}/api`;
}


export interface SandboxOpts {
  template?: string;
  /**
   * Idle timeout in seconds after which the sandbox auto-hibernates.
   * Default: `0` (persistent — never auto-hibernate).
   */
  timeout?: number;
  apiKey?: string;
  apiUrl?: string;
  envs?: Record<string, string>;
  metadata?: Record<string, string>;
  cpuCount?: number;
  memoryMB?: number;
  /**
   * Workspace disk size in MB (default 20480 = 20GB). Any additional GB above
   * 20GB is metered at a per-second rate comparable to EBS gp3.
   *
   * Closed beta: requests above 20GB require the org's `max_disk_mb` to be
   * raised. Contact us: https://cal.com/team/digger/opencomputer-founder-chat
   */
  diskMB?: number;
  /** Secret store name — resolves encrypted secrets and egress allowlist. */
  secretStore?: string;
  /** Declarative image definition. The server builds and caches it as a checkpoint. */
  image?: Image;
  /** Name of a pre-built snapshot to create the sandbox from. */
  snapshot?: string;
  /** Callback for build log streaming when using `image`. Called with each build step message. */
  onBuildLog?: (log: string) => void;
}

interface SandboxData {
  sandboxID: string;
  status: string;
  templateID?: string;
  connectURL?: string;
  token?: string;
  sandboxDomain?: string;
}

export interface CheckpointInfo {
  id: string;
  sandboxId: string;
  orgId: string;
  name: string;
  rootfsS3Key?: string;
  workspaceS3Key?: string;
  sandboxConfig: Record<string, unknown>;
  status: string;
  sizeBytes: number;
  createdAt: string;
}

export interface PatchInfo {
  id: string;
  checkpointId: string;
  sequence: number;
  script: string;
  description: string;
  strategy: string;
  createdAt: string;
}

export interface PatchResult {
  patch: PatchInfo;
}

export interface PreviewURLResult {
  id: string;
  sandboxId: string;
  orgId: string;
  hostname: string;
  customHostname?: string;
  port: number;
  cfHostnameId?: string;
  sslStatus: string;
  authConfig: Record<string, unknown>;
  createdAt: string;
}

/**
 * Thrown by `scale`, `setAutoscale`, and resource-changing calls when the
 * sandbox has a scaling lock active. Catch this to handle the lock case
 * specifically — typically by surfacing a clear message to the user, or
 * unlocking via `sandbox.setScalingLock(false)` first.
 */
export class ScalingLockedError extends Error {
  readonly code = "scaling_locked";
  constructor(message?: string) {
    super(message ?? "scaling is locked on this sandbox");
    this.name = "ScalingLockedError";
  }
}

/**
 * Thrown by `scale` and `setAutoscale` when the requested size exceeds the
 * organization's plan cap (e.g. free tier blocks > 4 GB). The HTTP layer
 * returns 402 Payment Required for this case.
 */
export class PlanLimitError extends Error {
  constructor(message?: string) {
    super(message ?? "plan limit exceeded for requested resources");
    this.name = "PlanLimitError";
  }
}

/**
 * Inspect a non-OK response from a scaling endpoint and throw the most
 * specific error type. Falls back to a generic Error when the response
 * doesn't match a known shape so callers still see the status + body.
 */
async function throwScalingError(resp: Response, action: string): Promise<never> {
  const text = await resp.text();
  let body: { error?: string; code?: string } = {};
  try {
    body = JSON.parse(text);
  } catch {
    // Non-JSON body — fall through to generic Error below.
  }
  if (resp.status === 403 && body.code === "scaling_locked") {
    throw new ScalingLockedError(body.error);
  }
  if (resp.status === 402) {
    throw new PlanLimitError(body.error);
  }
  throw new Error(`Failed to ${action}: ${resp.status} ${text}`);
}

export interface ScaleResult {
  sandboxID: string;
  memoryMB: number;
  cpuPercent: number;
}

export interface AutoscaleConfig {
  enabled: boolean;
  /** Minimum tier the autoscaler will shrink to. Must be an allowed memory tier (1024, 4096, 8192, 16384, 32768, 65536). */
  minMemoryMB?: number;
  /** Maximum tier the autoscaler will grow to. Must be an allowed memory tier and ≥ minMemoryMB. */
  maxMemoryMB?: number;
}

export interface AutoscaleStatus {
  sandboxID: string;
  enabled: boolean;
  minMemoryMB: number;
  maxMemoryMB: number;
}

export interface ScalingLockStatus {
  sandboxID: string;
  locked: boolean;
}

export class Sandbox {
  readonly sandboxId: string;
  readonly agent: Agent;
  readonly files: Filesystem;
  readonly exec: Exec;
  readonly pty: Pty;
  /** @deprecated Use `sandbox.exec` instead. This alias exists for backwards compatibility. */
  readonly commands: Exec;

  private apiUrl: string;
  private apiKey: string;
  private connectUrl: string;
  private token: string;
  private _status: string;
  private _sandboxDomain: string;

  private constructor(data: SandboxData, apiUrl: string, apiKey: string) {
    this.sandboxId = data.sandboxID;
    this._status = data.status;
    this.apiUrl = apiUrl;
    this.apiKey = apiKey;
    this.connectUrl = data.connectURL || "";
    this.token = data.token || "";
    this._sandboxDomain = data.sandboxDomain || "";

    // Always route through the CP — it handles readiness waiting and proxies to workers.
    this.agent = new Agent(apiUrl, apiKey, this.sandboxId, "");
    this.files = new Filesystem(apiUrl, apiKey, this.sandboxId, "");
    this.exec = new Exec(apiUrl, apiKey, this.sandboxId, "");
    this.commands = this.exec; // backwards-compatible alias
    this.pty = new Pty(apiUrl, apiKey, this.sandboxId, "");
  }

  get status(): string {
    return this._status;
  }

  /** Preview URL domain for port 80 (e.g., "sb-xxx-p80.workers.opencomputer.dev"). */
  get domain(): string {
    if (!this._sandboxDomain) return "";
    return `${this.sandboxId}-p80.${this._sandboxDomain}`;
  }

  /** Get the preview URL domain for a specific port. */
  getPreviewDomain(port: number): string {
    if (!this._sandboxDomain) return "";
    return `${this.sandboxId}-p${port}.${this._sandboxDomain}`;
  }

  static async create(opts: SandboxOpts = {}): Promise<Sandbox> {
    const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
    const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";

    const body: Record<string, unknown> = {
      templateID: opts.template ?? "base",
      // Default to 0 (persistent). Callers who want auto-hibernate must opt in.
      timeout: opts.timeout ?? 0,
    };
    if (opts.envs) body.envs = opts.envs;
    if (opts.metadata) body.metadata = opts.metadata;
    if (opts.cpuCount != null) body.cpuCount = opts.cpuCount;
    if (opts.memoryMB != null) body.memoryMB = opts.memoryMB;
    if (opts.diskMB != null) body.diskMB = opts.diskMB;
    if (opts.secretStore) body.secretStore = opts.secretStore;
    if (opts.image) body.image = opts.image.toJSON();
    if (opts.snapshot) body.snapshot = opts.snapshot;

    // Always use SSE for image/snapshot creation to keep the connection alive
    // through proxies (Cloudflare has a 100s idle timeout).
    const useSSE = !!(opts.image || opts.snapshot);

    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      ...(apiKey ? { "X-API-Key": apiKey } : {}),
    };
    if (useSSE) {
      headers["Accept"] = "text/event-stream";
    }

    const resp = await fetch(`${apiUrl}/sandboxes`, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create sandbox: ${resp.status} ${text}`);
    }

    if (useSSE && resp.headers.get("content-type")?.includes("text/event-stream")) {
      const onLog = opts.onBuildLog ?? (() => {});
      const data = await parseSSEStream<SandboxData>(resp, onLog);
      return new Sandbox(data, apiUrl, apiKey);
    }

    const data: SandboxData = await resp.json();
    return new Sandbox(data, apiUrl, apiKey);
  }

  static async connect(sandboxId: string, opts: Pick<SandboxOpts, "apiKey" | "apiUrl"> = {}): Promise<Sandbox> {
    const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
    const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";

    const resp = await fetch(`${apiUrl}/sandboxes/${sandboxId}`, {
      headers: apiKey ? { "X-API-Key": apiKey } : {},
    });

    if (!resp.ok) {
      throw new Error(`Failed to connect to sandbox ${sandboxId}: ${resp.status}`);
    }

    const data: SandboxData = await resp.json();
    return new Sandbox(data, apiUrl, apiKey);
  }

  async kill(): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}`, {
      method: "DELETE",
      headers: this.apiKey ? { "X-API-Key": this.apiKey } : {},
    });

    if (!resp.ok) {
      throw new Error(`Failed to kill sandbox: ${resp.status}`);
    }
    this._status = "stopped";
  }

  async isRunning(): Promise<boolean> {
    try {
      const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}`, {
        headers: this.apiKey ? { "X-API-Key": this.apiKey } : {},
      });
      if (!resp.ok) return false;
      const data: SandboxData = await resp.json();
      this._status = data.status;
      return data.status === "running";
    } catch {
      return false;
    }
  }

  async hibernate(): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/hibernate`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to hibernate sandbox: ${resp.status} ${text}`);
    }
    this._status = "hibernated";
  }

  async wake(opts: { timeout?: number } = {}): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/wake`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
      // Default to 0 (persistent) — matches create() default.
      body: JSON.stringify({ timeout: opts.timeout ?? 0 }),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to wake sandbox: ${resp.status} ${text}`);
    }

    const data: SandboxData = await resp.json();
    this._status = data.status;
    this.connectUrl = data.connectURL || "";
    this.token = data.token || "";

    // Always route through the CP
    (this as any).agent = new Agent(this.apiUrl, this.apiKey, this.sandboxId, "");
    (this as any).files = new Filesystem(this.apiUrl, this.apiKey, this.sandboxId, "");
    (this as any).exec = new Exec(this.apiUrl, this.apiKey, this.sandboxId, "");
    (this as any).pty = new Pty(this.apiUrl, this.apiKey, this.sandboxId, "");
  }

  /**
   * Soft restart of the running sandbox. The guest CPU is reset and the
   * kernel reboots — equivalent to running `reboot` inside the box. The
   * QEMU process, network mapping, and persistent disks all stay; only
   * in-memory state (running processes, page caches) is wiped.
   *
   * Use to recover from in-guest wedges: zombie pile-ups, OOM-killed
   * agents, runaway processes, broken-but-isolated systemd state.
   *
   * For the rare case where the VMM itself is wedged (e.g. QMP
   * unresponsive), use `powerCycle()` instead.
   */
  async reboot(): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/reboot`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to reboot sandbox: ${resp.status} ${text}`);
    }
  }

  /**
   * Hard restart of the sandbox. The QEMU process is killed and a fresh
   * one is started with the same on-disk drives. Sandbox keeps its ID,
   * project, secrets, env, and persistent workspace data; gets a new
   * external host port and TAP. Use when the VMM itself is wedged or a
   * `reboot()` doesn't recover.
   */
  async powerCycle(): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/power-cycle`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to power-cycle sandbox: ${resp.status} ${text}`);
    }
  }

  /**
   * Manually resize the sandbox to a specific memory tier. CPU is bundled
   * with memory per the platform's tier table (e.g. 8 GB → 4 vCPU). Allowed
   * tiers: 1024, 4096, 8192, 16384, 32768, 65536 MB.
   *
   * Side effect: a manual scale disables autoscale on this sandbox. If you
   * want size to track load again, call `setAutoscale({ enabled: true, ... })`
   * after.
   *
   * Throws `ScalingLockedError` if the sandbox has a scaling lock; throws
   * `PlanLimitError` if the requested size exceeds your plan cap.
   *
   * [HTTP API →](/api-reference/sandboxes/scale)
   */
  async scale(opts: { memoryMB: number }): Promise<ScaleResult> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/scale`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
      body: JSON.stringify({ memoryMB: opts.memoryMB }),
    });

    if (!resp.ok) {
      await throwScalingError(resp, "scale sandbox");
    }

    return resp.json();
  }

  /**
   * Enable or disable per-sandbox autoscale. When enabled, the platform
   * resizes the sandbox between `minMemoryMB` and `maxMemoryMB` based on
   * observed memory pressure — scaling up fast on a 1-min spike, down
   * slowly after sustained idle.
   *
   * Both bounds must be allowed memory tiers (1024, 4096, 8192, 16384,
   * 32768, 65536). Pass `enabled: false` to turn autoscale off (bounds
   * are ignored in that case).
   *
   * Throws `ScalingLockedError` if the sandbox has a scaling lock; throws
   * `PlanLimitError` if `maxMemoryMB` exceeds your plan cap.
   */
  async setAutoscale(opts: AutoscaleConfig): Promise<AutoscaleStatus> {
    const body: Record<string, unknown> = { enabled: opts.enabled };
    if (opts.minMemoryMB != null) body.minMemoryMB = opts.minMemoryMB;
    if (opts.maxMemoryMB != null) body.maxMemoryMB = opts.maxMemoryMB;

    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/autoscale`, {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      await throwScalingError(resp, "set autoscale");
    }

    return resp.json();
  }

  /**
   * Get the current autoscale configuration for the sandbox.
   */
  async getAutoscale(): Promise<AutoscaleStatus> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/autoscale`, {
      headers: this.apiKey ? { "X-API-Key": this.apiKey } : {},
    });
    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to get autoscale: ${resp.status} ${text}`);
    }
    return resp.json();
  }

  /**
   * Lock or unlock the sandbox's resources against scaling. When locked:
   *
   *   - `scale()` rejects with `ScalingLockedError`.
   *   - `setAutoscale({ enabled: true })` rejects with `ScalingLockedError`.
   *   - The platform autoscaler skips this sandbox entirely.
   *
   * Locking ALSO disables autoscale as a side effect (single-knob
   * semantics — "I don't want this scaling, period"). Unlocking does NOT
   * re-enable autoscale; call `setAutoscale({ enabled: true, ... })`
   * explicitly if you want it back.
   */
  async setScalingLock(locked: boolean): Promise<ScalingLockStatus> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/scaling-lock`, {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
      body: JSON.stringify({ locked }),
    });
    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to set scaling lock: ${resp.status} ${text}`);
    }
    return resp.json();
  }

  /**
   * Get the current scaling-lock state for the sandbox.
   */
  async getScalingLock(): Promise<ScalingLockStatus> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/scaling-lock`, {
      headers: this.apiKey ? { "X-API-Key": this.apiKey } : {},
    });
    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to get scaling lock: ${resp.status} ${text}`);
    }
    return resp.json();
  }

  async setTimeout(timeout: number): Promise<void> {
    const headers: Record<string, string> = { "Content-Type": "application/json" };
    if (this.apiKey) {
      headers["X-API-Key"] = this.apiKey;
    }

    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/timeout`, {
      method: "POST",
      headers,
      body: JSON.stringify({ timeout }),
    });

    if (!resp.ok) {
      throw new Error(`Failed to set timeout: ${resp.status}`);
    }
  }

  async createCheckpoint(name: string): Promise<CheckpointInfo> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/checkpoints`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
      body: JSON.stringify({ name }),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create checkpoint: ${resp.status} ${text}`);
    }

    return resp.json();
  }

  async listCheckpoints(): Promise<CheckpointInfo[]> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/checkpoints`, {
      headers: this.apiKey ? { "X-API-Key": this.apiKey } : {},
    });

    if (!resp.ok) {
      throw new Error(`Failed to list checkpoints: ${resp.status}`);
    }

    return resp.json();
  }

  async restoreCheckpoint(checkpointId: string): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/checkpoints/${checkpointId}/restore`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to restore checkpoint: ${resp.status} ${text}`);
    }

    // After restore, rebuild ops clients since the VM was rebooted
    const data: SandboxData = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}`, {
      headers: this.apiKey ? { "X-API-Key": this.apiKey } : {},
    }).then((r) => r.json());

    this.connectUrl = data.connectURL || "";
    this.token = data.token || "";

    // Always route through the CP
    (this as any).agent = new Agent(this.apiUrl, this.apiKey, this.sandboxId, "");
    (this as any).files = new Filesystem(this.apiUrl, this.apiKey, this.sandboxId, "");
    (this as any).exec = new Exec(this.apiUrl, this.apiKey, this.sandboxId, "");
    (this as any).pty = new Pty(this.apiUrl, this.apiKey, this.sandboxId, "");
  }

  static async createFromCheckpoint(checkpointId: string, opts: Pick<SandboxOpts, "apiKey" | "apiUrl" | "timeout" | "envs" | "secretStore"> = {}): Promise<Sandbox> {
    const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
    const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";

    const body: Record<string, unknown> = {};
    if (opts.timeout != null) body.timeout = opts.timeout;
    if (opts.envs) body.envs = opts.envs;
    if (opts.secretStore) body.secretStore = opts.secretStore;

    const resp = await fetch(`${apiUrl}/sandboxes/from-checkpoint/${checkpointId}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(apiKey ? { "X-API-Key": apiKey } : {}),
      },
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create sandbox from checkpoint: ${resp.status} ${text}`);
    }

    const data: SandboxData = await resp.json();
    return new Sandbox(data, apiUrl, apiKey);
  }

  async deleteCheckpoint(checkpointId: string): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/checkpoints/${checkpointId}`, {
      method: "DELETE",
      headers: this.apiKey ? { "X-API-Key": this.apiKey } : {},
    });

    if (!resp.ok && resp.status !== 404) {
      throw new Error(`Failed to delete checkpoint: ${resp.status}`);
    }
  }

  static async createCheckpointPatch(
    checkpointId: string,
    opts: { script: string; description?: string; apiKey?: string; apiUrl?: string }
  ): Promise<PatchResult> {
    const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
    const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";

    const resp = await fetch(`${apiUrl}/sandboxes/checkpoints/${checkpointId}/patches`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(apiKey ? { "X-API-Key": apiKey } : {}),
      },
      body: JSON.stringify({
        script: opts.script,
        description: opts.description ?? "",
      }),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create checkpoint patch: ${resp.status} ${text}`);
    }

    return resp.json();
  }

  static async listCheckpointPatches(
    checkpointId: string,
    opts: { apiKey?: string; apiUrl?: string } = {}
  ): Promise<PatchInfo[]> {
    const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
    const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";

    const resp = await fetch(`${apiUrl}/sandboxes/checkpoints/${checkpointId}/patches`, {
      headers: apiKey ? { "X-API-Key": apiKey } : {},
    });

    if (!resp.ok) {
      throw new Error(`Failed to list checkpoint patches: ${resp.status}`);
    }

    return resp.json();
  }

  static async deleteCheckpointPatch(
    checkpointId: string,
    patchId: string,
    opts: { apiKey?: string; apiUrl?: string } = {}
  ): Promise<void> {
    const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
    const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";

    const resp = await fetch(`${apiUrl}/sandboxes/checkpoints/${checkpointId}/patches/${patchId}`, {
      method: "DELETE",
      headers: apiKey ? { "X-API-Key": apiKey } : {},
    });

    if (!resp.ok && resp.status !== 404) {
      throw new Error(`Failed to delete checkpoint patch: ${resp.status}`);
    }
  }

  /**
   * Generate a signed download URL for a file in the sandbox.
   * The URL can be used by anyone (e.g. in a browser) without an API key.
   * @param path - absolute path inside the sandbox
   * @param opts.expiresIn - URL validity in seconds (default: 3600, max: 86400)
   */
  async downloadUrl(path: string, opts?: { expiresIn?: number }): Promise<string> {
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files/download-url`,
      {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
        },
        body: JSON.stringify({ path, expiresIn: opts?.expiresIn ?? 3600 }),
      },
    );

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to get download URL: ${resp.status} ${text}`);
    }

    const data: { url: string } = await resp.json();
    return data.url;
  }

  /**
   * Generate a signed upload URL for a file in the sandbox.
   * The URL can be used by anyone (e.g. in a browser) to PUT file content without an API key.
   * @param path - absolute path inside the sandbox
   * @param opts.expiresIn - URL validity in seconds (default: 3600, max: 86400)
   */
  async uploadUrl(path: string, opts?: { expiresIn?: number }): Promise<string> {
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files/upload-url`,
      {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
        },
        body: JSON.stringify({ path, expiresIn: opts?.expiresIn ?? 3600 }),
      },
    );

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to get upload URL: ${resp.status} ${text}`);
    }

    const data: { url: string } = await resp.json();
    return data.url;
  }

  async createPreviewURL(opts: { port: number; domain?: string; authConfig?: Record<string, unknown> }): Promise<PreviewURLResult> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/preview`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
      body: JSON.stringify({ port: opts.port, ...(opts.domain ? { domain: opts.domain } : {}), authConfig: opts.authConfig ?? {} }),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create preview URL: ${resp.status} ${text}`);
    }

    return resp.json();
  }

  async listPreviewURLs(): Promise<PreviewURLResult[]> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/preview`, {
      headers: this.apiKey ? { "X-API-Key": this.apiKey } : {},
    });

    if (!resp.ok) {
      throw new Error(`Failed to list preview URLs: ${resp.status}`);
    }

    return resp.json();
  }

  async deletePreviewURL(port: number): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/preview/${port}`, {
      method: "DELETE",
      headers: this.apiKey ? { "X-API-Key": this.apiKey } : {},
    });

    if (!resp.ok && resp.status !== 404) {
      throw new Error(`Failed to delete preview URL: ${resp.status}`);
    }
  }
}
