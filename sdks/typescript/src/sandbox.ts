import { Agent } from "./agent.js";
import { Filesystem } from "./filesystem.js";
import { Exec } from "./exec.js";
import { Pty } from "./pty.js";

function resolveApiUrl(url: string): string {
  const base = url.replace(/\/+$/, "");
  return base.endsWith("/api") ? base : `${base}/api`;
}

export interface SandboxOpts {
  template?: string;
  timeout?: number;
  apiKey?: string;
  apiUrl?: string;
  envs?: Record<string, string>;
  metadata?: Record<string, string>;
  cpuCount?: number;
  memoryMB?: number;
}

interface SandboxData {
  sandboxID: string;
  status: string;
  templateID?: string;
  connectURL?: string;
  token?: string;
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

  private constructor(data: SandboxData, apiUrl: string, apiKey: string) {
    this.sandboxId = data.sandboxID;
    this._status = data.status;
    this.apiUrl = apiUrl;
    this.apiKey = apiKey;
    this.connectUrl = data.connectURL || "";
    this.token = data.token || "";

    // Use direct worker URL for data operations if available
    const opsUrl = this.connectUrl || apiUrl;
    const opsKey = this.connectUrl ? "" : apiKey;
    const opsToken = this.connectUrl ? this.token : "";

    this.agent = new Agent(opsUrl, opsKey, this.sandboxId, opsToken);
    this.files = new Filesystem(opsUrl, opsKey, this.sandboxId, opsToken);
    this.exec = new Exec(opsUrl, opsKey, this.sandboxId, opsToken);
    this.commands = this.exec; // backwards-compatible alias
    this.pty = new Pty(opsUrl, opsKey, this.sandboxId, opsToken);
  }

  get status(): string {
    return this._status;
  }

  static async create(opts: SandboxOpts = {}): Promise<Sandbox> {
    const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
    const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";

    const body: Record<string, unknown> = {
      templateID: opts.template ?? "base",
      timeout: opts.timeout ?? 300,
    };
    if (opts.envs) body.envs = opts.envs;
    if (opts.metadata) body.metadata = opts.metadata;
    if (opts.cpuCount != null) body.cpuCount = opts.cpuCount;
    if (opts.memoryMB != null) body.memoryMB = opts.memoryMB;

    const resp = await fetch(`${apiUrl}/sandboxes`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(apiKey ? { "X-API-Key": apiKey } : {}),
      },
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create sandbox: ${resp.status} ${text}`);
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
      body: JSON.stringify({ timeout: opts.timeout ?? 300 }),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to wake sandbox: ${resp.status} ${text}`);
    }

    const data: SandboxData = await resp.json();
    this._status = data.status;
    this.connectUrl = data.connectURL || "";
    this.token = data.token || "";

    // Rebuild ops clients with new worker connection
    const opsUrl = this.connectUrl || this.apiUrl;
    const opsKey = this.connectUrl ? "" : this.apiKey;
    const opsToken = this.connectUrl ? this.token : "";

    (this as any).agent = new Agent(opsUrl, opsKey, this.sandboxId, opsToken);
    (this as any).files = new Filesystem(opsUrl, opsKey, this.sandboxId, opsToken);
    (this as any).exec = new Exec(opsUrl, opsKey, this.sandboxId, opsToken);
    (this as any).pty = new Pty(opsUrl, opsKey, this.sandboxId, opsToken);
  }

  async setTimeout(timeout: number): Promise<void> {
    // Route to worker directly (like commands/files/pty) — the control plane
    // rejects this call in server mode.
    const url = this.connectUrl || this.apiUrl;
    const headers: Record<string, string> = { "Content-Type": "application/json" };
    if (this.connectUrl && this.token) {
      headers["Authorization"] = `Bearer ${this.token}`;
    } else if (this.apiKey) {
      headers["X-API-Key"] = this.apiKey;
    }

    const resp = await fetch(`${url}/sandboxes/${this.sandboxId}/timeout`, {
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
    const opsUrl = this.connectUrl || this.apiUrl;
    const opsKey = this.connectUrl ? "" : this.apiKey;
    const opsToken = this.connectUrl ? this.token : "";
    (this as any).agent = new Agent(opsUrl, opsKey, this.sandboxId, opsToken);
    (this as any).files = new Filesystem(opsUrl, opsKey, this.sandboxId, opsToken);
    (this as any).exec = new Exec(opsUrl, opsKey, this.sandboxId, opsToken);
    (this as any).pty = new Pty(opsUrl, opsKey, this.sandboxId, opsToken);
  }

  static async createFromCheckpoint(checkpointId: string, opts: Pick<SandboxOpts, "apiKey" | "apiUrl" | "timeout"> = {}): Promise<Sandbox> {
    const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
    const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";

    const body: Record<string, unknown> = {};
    if (opts.timeout != null) body.timeout = opts.timeout;

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
