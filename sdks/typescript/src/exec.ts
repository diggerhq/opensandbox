import { ShellImpl, type Shell, type ShellOpts } from "./shell.js";

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
  onScrollbackEnd?: () => void;
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
  /** Resolves with the exit code when the process exits. */
  done: Promise<number>;
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
      onScrollbackEnd: opts.onScrollbackEnd,
    });
  }

  async attach(sessionId: string, opts: ExecAttachOpts = {}): Promise<ExecSession> {
    const wsUrl = this.apiUrl
      .replace("http://", "ws://")
      .replace("https://", "wss://");
    // WebSocket can't set custom headers, so pass credentials as query params.
    // Prefer JWT token (direct worker access); fall back to API key (control plane).
    const authParam = this.token
      ? `?token=${encodeURIComponent(this.token)}`
      : this.apiKey
        ? `?api_key=${encodeURIComponent(this.apiKey)}`
        : "";
    const wsEndpoint = `${wsUrl}/sandboxes/${this.sandboxId}/exec/${sessionId}${authParam}`;

    const ws = new WebSocket(wsEndpoint);
    ws.binaryType = "arraybuffer";

    let gotExit = false;
    let opened = false;
    let resolveDone: (code: number) => void;
    const done = new Promise<number>((resolve) => { resolveDone = resolve; });

    await new Promise<void>((resolve, reject) => {
      ws.onopen = () => {
        opened = true;
        resolve();
      };
      ws.onerror = () => {
        if (!opened) reject(new Error(`WebSocket connection failed: ${wsEndpoint}`));
      };
      ws.onclose = () => {
        if (!opened) reject(new Error(`WebSocket closed before opening: ${wsEndpoint}`));
      };
    });

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
          gotExit = true;
          if (payload.length >= 4) {
            const view = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
            const exitCode = view.getInt32(0, false); // big-endian
            opts.onExit?.(exitCode);
            resolveDone(exitCode);
          } else {
            opts.onExit?.(0);
            resolveDone(0);
          }
          break;
        case 0x04: // scrollback_end
          opts.onScrollbackEnd?.();
          break;
      }
    };

    ws.onclose = () => {
      if (!gotExit) {
        opts.onExit?.(-1);
        resolveDone(-1);
      }
    };

    ws.onerror = () => {
      // Post-open errors are followed by onclose, which handles exit.
    };

    const exec = this;

    return {
      sessionId,
      done,
      sendStdin(data: string | Uint8Array): void {
        if (ws.readyState !== WebSocket.OPEN) return;
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

  /**
   * Alias for {@link start}. Use when the intent is "run this command in the
   * background and observe it" rather than the more ambiguous "start". Same
   * options, same return type.
   */
  async background(command: string, opts: ExecStartOpts = {}): Promise<ExecSession> {
    return this.start(command, opts);
  }

  /**
   * Open a stateful shell session. Subsequent `.run()` calls share the same
   * bash process, so `cwd`, exported env vars, and shell functions persist
   * across calls — the ergonomics of a terminal tab.
   *
   * Backed by a long-running exec session running `bash --noprofile --norc`.
   * Foreground-only: concurrent `.run()` rejects. Use `exec.background()` for
   * fire-and-forget processes. If the user command calls `exit`, the shell
   * closes (same as closing a terminal tab) and subsequent `.run()` rejects.
   */
  async shell(opts: ShellOpts = {}): Promise<Shell> {
    let impl: ShellImpl | null = null;
    const session = await this.start("bash", {
      args: ["--noprofile", "--norc", "+m"],
      env: opts.env,
      cwd: opts.cwd,
      onStdout: (chunk) => impl?.onStdoutChunk(chunk),
      onStderr: (chunk) => impl?.onStderrChunk(chunk),
      onScrollbackEnd: () => impl?.onScrollbackEnd(),
    });
    impl = new ShellImpl(session);
    return impl;
  }

  /**
   * Re-attach to a shell session that was previously opened by `shell()` and
   * whose sessionId you've kept. Useful for revisiting a long-lived terminal
   * tab from a different process invocation.
   *
   * Assumes the shell is idle (no in-flight `.run()` from another client).
   * If another client has a run in flight, output will interleave and the
   * results are undefined — coordinate at the application level.
   */
  async reattachShell(sessionId: string): Promise<Shell> {
    let impl: ShellImpl | null = null;
    const session = await this.attach(sessionId, {
      onStdout: (chunk) => impl?.onStdoutChunk(chunk),
      onStderr: (chunk) => impl?.onStderrChunk(chunk),
      onScrollbackEnd: () => impl?.onScrollbackEnd(),
    });
    impl = new ShellImpl(session);
    return impl;
  }

  async run(command: string, opts: RunOpts = {}): Promise<ProcessResult> {
    const body: Record<string, unknown> = {
      cmd: "sh",
      args: ["-c", command],
    };
    if (opts.env) body.envs = opts.env;
    if (opts.cwd) body.cwd = opts.cwd;
    if (opts.timeout != null) body.timeout = opts.timeout;
    else body.timeout = 60;

    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/exec/run`, {
      method: "POST",
      headers: this.headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to run command: ${resp.status} ${text}`);
    }

    return resp.json();
  }
}
