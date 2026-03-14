export interface TemplateInfo {
  templateID: string;
  name: string;
  tag: string;
  status: string;
}

export class Templates {
  constructor(
    private apiUrl: string,
    private apiKey: string,
  ) {}

  private get headers(): Record<string, string> {
    const h: Record<string, string> = { "Content-Type": "application/json" };
    if (this.apiKey) h["X-API-Key"] = this.apiKey;
    return h;
  }

  async build(name: string, dockerfile: string): Promise<TemplateInfo> {
    const resp = await fetch(`${this.apiUrl}/templates`, {
      method: "POST",
      headers: this.headers,
      body: JSON.stringify({ name, dockerfile }),
    });

    if (!resp.ok) {
      throw new Error(`Failed to build template: ${resp.status}`);
    }

    return resp.json();
  }

  async list(): Promise<TemplateInfo[]> {
    const resp = await fetch(`${this.apiUrl}/templates`, {
      headers: this.headers,
    });

    if (!resp.ok) {
      throw new Error(`Failed to list templates: ${resp.status}`);
    }

    return resp.json();
  }

  async get(name: string): Promise<TemplateInfo> {
    const resp = await fetch(`${this.apiUrl}/templates/${name}`, {
      headers: this.headers,
    });

    if (!resp.ok) {
      throw new Error(`Failed to get template: ${resp.status}`);
    }

    return resp.json();
  }

  async delete(name: string): Promise<void> {
    const resp = await fetch(`${this.apiUrl}/templates/${name}`, {
      method: "DELETE",
      headers: this.headers,
    });

    if (!resp.ok) {
      throw new Error(`Failed to delete template: ${resp.status}`);
    }
  }
}
