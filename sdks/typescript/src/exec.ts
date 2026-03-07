export interface ProcessResult {
  exitCode: number;
  stdout: string;
  stderr: string;
}

export interface RunOpts {
  timeout?: number;
  env?: Record<string, string>;
  cwd?: string;
}

export interface ExecStartOpts {
  args?: string[];
  env?: Record<string, string>;
  cwd?: string;
  timeout?: number;
  maxRunAfterDisconnect?: number;
  onStdout?: (data: Uint8Array) => void;
  onStderr?: (data: Uint8Array) => void;
  onExit?: (exitCode: number) => void;
}

export interface ExecAttachOpts {
  onStdout?: (data: Uint8Array) => void;
  onStderr?: (data: Uint8Array) => void;
  onExit?: (exitCode: number) => void;
  onScrollbackEnd?: () => void;
}

export interface ExecSessionInfo {
  sessionID: string;
  sandboxID: string;
  command: string;
  args: string[];
  running: boolean;
  exitCode?: number;
  startedAt: string;
  attachedClients: number;
}

export interface ExecSession {
  sessionId: string;
  sendStdin(data: string | Uint8Array): void;
  kill(signal?: number): Promise<void>;
  close(): void;
}

export class Exec {
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

  async start(command: string, opts: ExecStartOpts = {}): Promise<ExecSession> {
    const body: Record<string, unknown> = { cmd: command };
    if (opts.args) body.args = opts.args;
    if (opts.env) body.envs = opts.env;
    if (opts.cwd) body.cwd = opts.cwd;
    if (opts.timeout != null) body.timeout = opts.timeout;
    if (opts.maxRunAfterDisconnect != null) body.maxRunAfterDisconnect = opts.maxRunAfterDisconnect;

    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/exec`, {
      method: "POST",
      headers: this.headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create exec session: ${resp.status} ${text}`);
    }

    const data = await resp.json();
    const sessionId: string = data.sessionID;

    return this.attach(sessionId, {
      onStdout: opts.onStdout,
      onStderr: opts.onStderr,
      onExit: opts.onExit,
    });
  }

  async attach(sessionId: string, opts: ExecAttachOpts = {}): Promise<ExecSession> {
    const wsUrl = this.apiUrl
      .replace("http://", "ws://")
      .replace("https://", "wss://");
    const tokenParam = this.token ? `?token=${encodeURIComponent(this.token)}` : "";
    const wsEndpoint = `${wsUrl}/sandboxes/${this.sandboxId}/exec/${sessionId}${tokenParam}`;

    const ws = new WebSocket(wsEndpoint);
    ws.binaryType = "arraybuffer";

    ws.onmessage = (event) => {
      const buf = new Uint8Array(event.data as ArrayBuffer);
      if (buf.length < 1) return;

      const streamId = buf[0];
      const payload = buf.slice(1);

      switch (streamId) {
        case 0x01: // stdout
          opts.onStdout?.(payload);
          break;
        case 0x02: // stderr
          opts.onStderr?.(payload);
          break;
        case 0x03: // exit
          if (payload.length >= 4) {
            const view = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
            const exitCode = view.getInt32(0, false); // big-endian
            opts.onExit?.(exitCode);
          }
          break;
        case 0x04: // scrollback_end
          opts.onScrollbackEnd?.();
          break;
      }
    };

    const exec = this;

    return {
      sessionId,
      sendStdin(data: string | Uint8Array): void {
        const payload = typeof data === "string"
          ? new TextEncoder().encode(data)
          : data;
        const msg = new Uint8Array(1 + payload.length);
        msg[0] = 0x00; // stdin stream ID
        msg.set(payload, 1);
        ws.send(msg);
      },
      async kill(signal?: number): Promise<void> {
        await exec.kill(sessionId, signal);
      },
      close(): void {
        ws.close();
      },
    };
  }

  async list(): Promise<ExecSessionInfo[]> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/exec`, {
      headers: this.headers,
    });

    if (!resp.ok) {
      throw new Error(`Failed to list exec sessions: ${resp.status}`);
    }

    return resp.json();
  }

  async kill(sessionId: string, signal?: number): Promise<void> {
    const body: Record<string, unknown> = {};
    if (signal != null) body.signal = signal;

    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/exec/${sessionId}/kill`, {
      method: "POST",
      headers: this.headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to kill exec session: ${resp.status} ${text}`);
    }
  }

  async run(command: string, opts: RunOpts = {}): Promise<ProcessResult> {
    let stdout = "";
    let stderr = "";

    return new Promise<ProcessResult>((resolve, reject) => {
      const decoder = new TextDecoder();

      this.start("sh", {
        args: ["-c", command],
        env: opts.env,
        cwd: opts.cwd,
        timeout: opts.timeout,
        onStdout: (data) => {
          stdout += decoder.decode(data, { stream: true });
        },
        onStderr: (data) => {
          stderr += decoder.decode(data, { stream: true });
        },
        onExit: (exitCode) => {
          resolve({ exitCode, stdout, stderr });
        },
      }).catch(reject);
    });
  }
}
