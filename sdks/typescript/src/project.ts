function resolveApiUrl(url: string): string {
  const base = url.replace(/\/+$/, "");
  return base.endsWith("/api") ? base : `${base}/api`;
}

export interface ProjectInfo {
  id: string;
  orgId: string;
  name: string;
  template: string;
  cpuCount: number;
  memoryMB: number;
  timeoutSec: number;
  egressAllowlist: string[];
  createdAt: string;
  updatedAt: string;
}

export interface ProjectOpts {
  apiKey?: string;
  apiUrl?: string;
}

export interface CreateProjectOpts extends ProjectOpts {
  name: string;
  template?: string;
  cpuCount?: number;
  memoryMB?: number;
  timeoutSec?: number;
  egressAllowlist?: string[];
}

export interface UpdateProjectOpts extends ProjectOpts {
  name?: string;
  template?: string;
  cpuCount?: number;
  memoryMB?: number;
  timeoutSec?: number;
  egressAllowlist?: string[];
}

function getConfig(opts: ProjectOpts) {
  const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
  const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (apiKey) headers["X-API-Key"] = apiKey;
  return { apiUrl, headers };
}

export class Project {
  private constructor() {}

  static async create(opts: CreateProjectOpts): Promise<ProjectInfo> {
    const { apiUrl, headers } = getConfig(opts);

    const body: Record<string, unknown> = { name: opts.name };
    if (opts.template) body.template = opts.template;
    if (opts.cpuCount != null) body.cpuCount = opts.cpuCount;
    if (opts.memoryMB != null) body.memoryMB = opts.memoryMB;
    if (opts.timeoutSec != null) body.timeoutSec = opts.timeoutSec;
    if (opts.egressAllowlist) body.egressAllowlist = opts.egressAllowlist;

    const resp = await fetch(`${apiUrl}/projects`, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create project: ${resp.status} ${text}`);
    }

    return resp.json();
  }

  static async list(opts: ProjectOpts = {}): Promise<ProjectInfo[]> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/projects`, { headers });

    if (!resp.ok) {
      throw new Error(`Failed to list projects: ${resp.status}`);
    }

    return resp.json();
  }

  static async get(projectId: string, opts: ProjectOpts = {}): Promise<ProjectInfo> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/projects/${projectId}`, { headers });

    if (!resp.ok) {
      throw new Error(`Failed to get project: ${resp.status}`);
    }

    return resp.json();
  }

  static async update(projectId: string, opts: UpdateProjectOpts): Promise<ProjectInfo> {
    const { apiUrl, headers } = getConfig(opts);

    const body: Record<string, unknown> = {};
    if (opts.name) body.name = opts.name;
    if (opts.template) body.template = opts.template;
    if (opts.cpuCount != null) body.cpuCount = opts.cpuCount;
    if (opts.memoryMB != null) body.memoryMB = opts.memoryMB;
    if (opts.timeoutSec != null) body.timeoutSec = opts.timeoutSec;
    if (opts.egressAllowlist) body.egressAllowlist = opts.egressAllowlist;

    const resp = await fetch(`${apiUrl}/projects/${projectId}`, {
      method: "PUT",
      headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to update project: ${resp.status} ${text}`);
    }

    return resp.json();
  }

  static async delete(projectId: string, opts: ProjectOpts = {}): Promise<void> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/projects/${projectId}`, {
      method: "DELETE",
      headers,
    });

    if (!resp.ok) {
      throw new Error(`Failed to delete project: ${resp.status}`);
    }
  }

  // ── Secrets ──────────────────────────────────────────────────────────────

  static async setSecret(projectId: string, name: string, value: string, opts: ProjectOpts = {}): Promise<void> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/projects/${projectId}/secrets/${name}`, {
      method: "PUT",
      headers,
      body: JSON.stringify({ value }),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to set secret: ${resp.status} ${text}`);
    }
  }

  static async deleteSecret(projectId: string, name: string, opts: ProjectOpts = {}): Promise<void> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/projects/${projectId}/secrets/${name}`, {
      method: "DELETE",
      headers,
    });

    if (!resp.ok && resp.status !== 404) {
      throw new Error(`Failed to delete secret: ${resp.status}`);
    }
  }

  static async listSecrets(projectId: string, opts: ProjectOpts = {}): Promise<string[]> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/projects/${projectId}/secrets`, { headers });

    if (!resp.ok) {
      throw new Error(`Failed to list secrets: ${resp.status}`);
    }

    return resp.json();
  }
}
