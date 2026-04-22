// Usage + tags — TypeScript SDK surface.
//
// All numeric fields are GB-seconds. Dollars live in Stripe; the SDK
// intentionally mirrors the server's "physical quantity" model so
// the invoice stays the source of truth for currency.

export interface UsageSandboxItem {
  sandboxId: string;
  alias?: string;
  status?: string;
  tags: Record<string, string>;
  tagsLastUpdatedAt: string | null;
  memoryGbSeconds: number;
  diskOverageGbSeconds: number;
}

export interface UsageTagItem {
  tagKey: string;
  tagValue: string;
  memoryGbSeconds: number;
  diskOverageGbSeconds: number;
  sandboxCount: number;
}

export interface UsageTotals {
  memoryGbSeconds: number;
  diskOverageGbSeconds: number;
}

export interface UsageUntaggedBucket extends UsageTotals {
  sandboxCount: number;
}

export interface UsageBySandboxResponse {
  from: string;
  to: string;
  groupBy: "sandbox";
  total: UsageTotals;
  items: UsageSandboxItem[];
  nextCursor: string | null;
}

export interface UsageByTagResponse {
  from: string;
  to: string;
  groupBy: string; // "tag:<key>"
  total: UsageTotals;
  untagged: UsageUntaggedBucket;
  items: UsageTagItem[];
  nextCursor: string | null;
}

export interface UsageFilterMap {
  /** `filter[tag:<key>]=v1,v2` — repeatable, AND'd. Empty string = "key absent." */
  [tagFilter: string]: string;
}

export interface UsageQueryOpts {
  from?: string; // RFC3339; default: now - 30d
  to?: string;
  filter?: UsageFilterMap;
  sort?: "-memoryGbSeconds" | "-diskOverageGbSeconds";
  limit?: number;
  cursor?: string;
}

export interface SandboxUsageResponse {
  sandboxId: string;
  alias?: string;
  status?: string;
  from: string;
  to: string;
  memoryGbSeconds: number;
  diskOverageGbSeconds: number;
  tags: Record<string, string>;
  tagsLastUpdatedAt: string | null;
  firstStartedAt: string | null;
  lastEndedAt: string | null;
}

export interface TagKeyInfo {
  key: string;
  sandboxCount: number;
  valueCount: number;
}

export class Usage {
  constructor(
    private apiUrl: string,
    private apiKey: string,
  ) {}

  private get headers(): Record<string, string> {
    const h: Record<string, string> = { "Content-Type": "application/json" };
    if (this.apiKey) h["X-API-Key"] = this.apiKey;
    return h;
  }

  private buildQueryString(params: Record<string, string | undefined>, filter?: UsageFilterMap): string {
    const u = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== "") u.set(k, v);
    }
    if (filter) {
      for (const [k, v] of Object.entries(filter)) {
        u.append(`filter[${k}]`, v);
      }
    }
    const s = u.toString();
    return s ? `?${s}` : "";
  }

  /** `GET /usage?groupBy=sandbox` — top sandboxes by spend. */
  async bySandbox(opts: UsageQueryOpts = {}): Promise<UsageBySandboxResponse> {
    const qs = this.buildQueryString({
      groupBy: "sandbox",
      from: opts.from,
      to: opts.to,
      sort: opts.sort,
      limit: opts.limit?.toString(),
      cursor: opts.cursor,
    }, opts.filter);
    const resp = await fetch(`${this.apiUrl}/usage${qs}`, { headers: this.headers });
    if (!resp.ok) throw new Error(`Failed to fetch usage: ${resp.status}`);
    return resp.json();
  }

  /** `GET /usage?groupBy=tag:<key>` — spend per tag value, plus untagged sibling bucket. */
  async byTag(tagKey: string, opts: UsageQueryOpts = {}): Promise<UsageByTagResponse> {
    const qs = this.buildQueryString({
      groupBy: `tag:${tagKey}`,
      from: opts.from,
      to: opts.to,
      sort: opts.sort,
      limit: opts.limit?.toString(),
      cursor: opts.cursor,
    }, opts.filter);
    const resp = await fetch(`${this.apiUrl}/usage${qs}`, { headers: this.headers });
    if (!resp.ok) throw new Error(`Failed to fetch usage: ${resp.status}`);
    return resp.json();
  }

  /** `GET /sandboxes/:id/usage` — per-sandbox drilldown. */
  async forSandbox(sandboxId: string, opts: Pick<UsageQueryOpts, "from" | "to"> = {}): Promise<SandboxUsageResponse> {
    const qs = this.buildQueryString({ from: opts.from, to: opts.to });
    const resp = await fetch(`${this.apiUrl}/sandboxes/${sandboxId}/usage${qs}`, { headers: this.headers });
    if (!resp.ok) throw new Error(`Failed to fetch sandbox usage: ${resp.status}`);
    return resp.json();
  }
}

export class Tags {
  constructor(
    private apiUrl: string,
    private apiKey: string,
  ) {}

  private get headers(): Record<string, string> {
    const h: Record<string, string> = { "Content-Type": "application/json" };
    if (this.apiKey) h["X-API-Key"] = this.apiKey;
    return h;
  }

  /** `GET /tags` — discovery of all tag keys across the org. */
  async listKeys(): Promise<TagKeyInfo[]> {
    const resp = await fetch(`${this.apiUrl}/tags`, { headers: this.headers });
    if (!resp.ok) throw new Error(`Failed to list tag keys: ${resp.status}`);
    const body = await resp.json();
    return body.keys;
  }

  /** `GET /sandboxes/:id/tags` — current tag set. */
  async get(sandboxId: string): Promise<{ tags: Record<string, string>; tagsLastUpdatedAt: string | null }> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${sandboxId}/tags`, { headers: this.headers });
    if (!resp.ok) throw new Error(`Failed to get tags: ${resp.status}`);
    return resp.json();
  }

  /** `PUT /sandboxes/:id/tags` — full replace. `{}` clears all tags. */
  async set(sandboxId: string, tags: Record<string, string>): Promise<{ tags: Record<string, string>; tagsLastUpdatedAt: string | null }> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${sandboxId}/tags`, {
      method: "PUT",
      headers: this.headers,
      body: JSON.stringify(tags),
    });
    if (!resp.ok) throw new Error(`Failed to set tags: ${resp.status}`);
    return resp.json();
  }
}
