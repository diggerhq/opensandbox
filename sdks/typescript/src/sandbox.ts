import { Filesystem } from "./filesystem.js";
import { Commands } from "./commands.js";
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
  readonly files: Filesystem;
  readonly commands: Commands;
  readonly pty: Pty;

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

    this.files = new Filesystem(opsUrl, opsKey, this.sandboxId, opsToken);
    this.commands = new Commands(opsUrl, opsKey, this.sandboxId, opsToken);
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

    (this as any).files = new Filesystem(opsUrl, opsKey, this.sandboxId, opsToken);
    (this as any).commands = new Commands(opsUrl, opsKey, this.sandboxId, opsToken);
    (this as any).pty = new Pty(opsUrl, opsKey, this.sandboxId, opsToken);
  }

  async setTimeout(timeout: number): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/timeout`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.apiKey ? { "X-API-Key": this.apiKey } : {}),
      },
      body: JSON.stringify({ timeout }),
    });

    if (!resp.ok) {
      throw new Error(`Failed to set timeout: ${resp.status}`);
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
