export interface EntryInfo {
  name: string;
  isDir: boolean;
  path: string;
  size: number;
}

export class Filesystem {
  constructor(
    private apiUrl: string,
    private apiKey: string,
    private sandboxId: string,
    private token: string = "",
  ) {}

  private get headers(): Record<string, string> {
    if (this.token) return { "Authorization": `Bearer ${this.token}` };
    return this.apiKey ? { "X-API-Key": this.apiKey } : {};
  }

  async read(path: string): Promise<string> {
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files?path=${encodeURIComponent(path)}`,
      { headers: this.headers },
    );
    if (!resp.ok) throw new Error(`Failed to read ${path}: ${resp.status}`);
    return resp.text();
  }

  async readBytes(path: string): Promise<Uint8Array> {
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files?path=${encodeURIComponent(path)}`,
      { headers: this.headers },
    );
    if (!resp.ok) throw new Error(`Failed to read ${path}: ${resp.status}`);
    return new Uint8Array(await resp.arrayBuffer());
  }

  async write(path: string, content: string | Uint8Array): Promise<void> {
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files?path=${encodeURIComponent(path)}`,
      {
        method: "PUT",
        headers: this.headers,
        body: content as BodyInit,
      },
    );
    if (!resp.ok) throw new Error(`Failed to write ${path}: ${resp.status}`);
  }

  async list(path: string = "/"): Promise<EntryInfo[]> {
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files/list?path=${encodeURIComponent(path)}`,
      { headers: this.headers },
    );
    if (!resp.ok) throw new Error(`Failed to list ${path}: ${resp.status}`);
    const data = await resp.json();
    return data ?? [];
  }

  async makeDir(path: string): Promise<void> {
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files/mkdir?path=${encodeURIComponent(path)}`,
      { method: "POST", headers: this.headers },
    );
    if (!resp.ok) throw new Error(`Failed to mkdir ${path}: ${resp.status}`);
  }

  async remove(path: string): Promise<void> {
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files?path=${encodeURIComponent(path)}`,
      { method: "DELETE", headers: this.headers },
    );
    if (!resp.ok) throw new Error(`Failed to remove ${path}: ${resp.status}`);
  }

  /**
   * Returns the raw response body as a ReadableStream for large file downloads.
   * Avoids buffering the entire file in memory.
   */
  async readStream(path: string): Promise<ReadableStream<Uint8Array>> {
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files?path=${encodeURIComponent(path)}`,
      { headers: this.headers },
    );
    if (!resp.ok) throw new Error(`Failed to read ${path}: ${resp.status}`);
    if (!resp.body) throw new Error(`No response body for ${path}`);
    return resp.body;
  }

  /**
   * Streams a ReadableStream or Uint8Array as the request body for large file uploads.
   * Avoids buffering the entire file in memory when given a ReadableStream.
   */
  async writeStream(path: string, stream: ReadableStream<Uint8Array> | Uint8Array): Promise<void> {
    const opts: RequestInit & Record<string, unknown> = {
      method: "PUT",
      headers: this.headers,
      body: stream as BodyInit,
    };
    // duplex: "half" is required for ReadableStream bodies in Node.js fetch,
    // but must NOT be set for Uint8Array/Buffer bodies.
    if (stream instanceof ReadableStream) {
      opts.duplex = "half";
    }
    const resp = await fetch(
      `${this.apiUrl}/sandboxes/${this.sandboxId}/files?path=${encodeURIComponent(path)}`,
      opts,
    );
    if (!resp.ok) throw new Error(`Failed to write ${path}: ${resp.status}`);
  }

  async exists(path: string): Promise<boolean> {
    try {
      const resp = await fetch(
        `${this.apiUrl}/sandboxes/${this.sandboxId}/files?path=${encodeURIComponent(path)}`,
        { headers: this.headers },
      );
      return resp.ok;
    } catch {
      return false;
    }
  }
}
