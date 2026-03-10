export interface PtyOpts {
  cols?: number;
  rows?: number;
  onOutput?: (data: Uint8Array) => void;
}

export interface PtySession {
  sessionId: string;
  send(data: string | Uint8Array): void;
  close(): void;
}

export class Pty {
  constructor(
    private apiUrl: string,
    private apiKey: string,
    private sandboxId: string,
    private token: string = "",
  ) {}

  private get headers(): Record<string, string> {
    const h: Record<string, string> = { "Content-Type": "application/json" };
    if (this.token) {
      h["Authorization"] = `Bearer ${this.token}`;
    } else if (this.apiKey) {
      h["X-API-Key"] = this.apiKey;
    }
    return h;
  }

  async create(opts: PtyOpts = {}): Promise<PtySession> {
    // Create session via REST
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/pty`, {
      method: "POST",
      headers: this.headers,
      body: JSON.stringify({
        cols: opts.cols ?? 80,
        rows: opts.rows ?? 24,
      }),
    });

    if (!resp.ok) {
      throw new Error(`Failed to create PTY: ${resp.status}`);
    }

    const data = await resp.json();
    const sessionId: string = data.sessionID;

    // Connect via WebSocket
    const wsUrl = this.apiUrl
      .replace("http://", "ws://")
      .replace("https://", "wss://");
    const tokenParam = this.token ? `?token=${encodeURIComponent(this.token)}` : "";
    const wsEndpoint = `${wsUrl}/sandboxes/${this.sandboxId}/pty/${sessionId}${tokenParam}`;

    const ws = new WebSocket(wsEndpoint);
    ws.binaryType = "arraybuffer";

    if (opts.onOutput) {
      const onOutput = opts.onOutput;
      ws.onmessage = (event) => {
        const data = event.data instanceof ArrayBuffer
          ? new Uint8Array(event.data)
          : new TextEncoder().encode(event.data as string);
        onOutput(data);
      };
    }

    return {
      sessionId,
      send(data: string | Uint8Array): void {
        if (typeof data === "string") {
          ws.send(data);
        } else {
          ws.send(data);
        }
      },
      close(): void {
        ws.close();
      },
    };
  }
}
