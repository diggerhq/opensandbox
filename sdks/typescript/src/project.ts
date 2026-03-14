function resolveApiUrl(url: string): string {
  const base = url.replace(/\/+$/, "");
  return base.endsWith("/api") ? base : `${base}/api`;
}

export interface SecretStoreInfo {
  id: string;
  orgId: string;
  name: string;
  egressAllowlist: string[];
  createdAt: string;
  updatedAt: string;
}

export interface SecretEntryInfo {
  id: string;
  storeId: string;
  name: string;
  allowedHosts: string[];
  createdAt: string;
  updatedAt: string;
}

export interface SecretStoreOpts {
  apiKey?: string;
  apiUrl?: string;
}

export interface CreateSecretStoreOpts extends SecretStoreOpts {
  name: string;
  egressAllowlist?: string[];
}

export interface UpdateSecretStoreOpts extends SecretStoreOpts {
  name?: string;
  egressAllowlist?: string[];
}

function getConfig(opts: SecretStoreOpts) {
  const apiUrl = resolveApiUrl(opts.apiUrl ?? process.env.OPENCOMPUTER_API_URL ?? "https://app.opencomputer.dev");
  const apiKey = opts.apiKey ?? process.env.OPENCOMPUTER_API_KEY ?? "";
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (apiKey) headers["X-API-Key"] = apiKey;
  return { apiUrl, headers };
}

export class SecretStore {
  private constructor() {}

  static async create(opts: CreateSecretStoreOpts): Promise<SecretStoreInfo> {
    const { apiUrl, headers } = getConfig(opts);

    const body: Record<string, unknown> = { name: opts.name };
    if (opts.egressAllowlist) body.egressAllowlist = opts.egressAllowlist;

    const resp = await fetch(`${apiUrl}/secret-stores`, {
      method: "POST",
      headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create secret store: ${resp.status} ${text}`);
    }

    return resp.json();
  }

  static async list(opts: SecretStoreOpts = {}): Promise<SecretStoreInfo[]> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/secret-stores`, { headers });

    if (!resp.ok) {
      throw new Error(`Failed to list secret stores: ${resp.status}`);
    }

    return resp.json();
  }

  static async get(storeId: string, opts: SecretStoreOpts = {}): Promise<SecretStoreInfo> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/secret-stores/${storeId}`, { headers });

    if (!resp.ok) {
      throw new Error(`Failed to get secret store: ${resp.status}`);
    }

    return resp.json();
  }

  static async update(storeId: string, opts: UpdateSecretStoreOpts): Promise<SecretStoreInfo> {
    const { apiUrl, headers } = getConfig(opts);

    const body: Record<string, unknown> = {};
    if (opts.name) body.name = opts.name;
    if (opts.egressAllowlist) body.egressAllowlist = opts.egressAllowlist;

    const resp = await fetch(`${apiUrl}/secret-stores/${storeId}`, {
      method: "PUT",
      headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to update secret store: ${resp.status} ${text}`);
    }

    return resp.json();
  }

  static async delete(storeId: string, opts: SecretStoreOpts = {}): Promise<void> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/secret-stores/${storeId}`, {
      method: "DELETE",
      headers,
    });

    if (!resp.ok) {
      throw new Error(`Failed to delete secret store: ${resp.status}`);
    }
  }

  // ── Secret Entries ──────────────────────────────────────────────────────

  static async setSecret(storeId: string, name: string, value: string, opts: SecretStoreOpts & { allowedHosts?: string[] } = {}): Promise<void> {
    const { apiUrl, headers } = getConfig(opts);

    const body: Record<string, unknown> = { value };
    if (opts.allowedHosts) body.allowedHosts = opts.allowedHosts;

    const resp = await fetch(`${apiUrl}/secret-stores/${storeId}/secrets/${name}`, {
      method: "PUT",
      headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to set secret: ${resp.status} ${text}`);
    }
  }

  static async deleteSecret(storeId: string, name: string, opts: SecretStoreOpts = {}): Promise<void> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/secret-stores/${storeId}/secrets/${name}`, {
      method: "DELETE",
      headers,
    });

    if (!resp.ok && resp.status !== 404) {
      throw new Error(`Failed to delete secret: ${resp.status}`);
    }
  }

  static async listSecrets(storeId: string, opts: SecretStoreOpts = {}): Promise<SecretEntryInfo[]> {
    const { apiUrl, headers } = getConfig(opts);

    const resp = await fetch(`${apiUrl}/secret-stores/${storeId}/secrets`, { headers });

    if (!resp.ok) {
      throw new Error(`Failed to list secrets: ${resp.status}`);
    }

    return resp.json();
  }
}
