export interface AgentEvent {
  type:
    | "ready"
    | "configured"
    | "assistant"
    | "result"
    | "system"
    | "tool_use_summary"
    | "turn_complete"
    | "interrupted"
    | "error"
    | string;
  [key: string]: unknown;
}

export interface McpServerConfig {
  command: string;
  args?: string[];
  env?: Record<string, string>;
}

export interface AgentConfig {
  model?: string;
  systemPrompt?: string;
  allowedTools?: string[];
  permissionMode?: string;
  maxTurns?: number;
  cwd?: string;
  mcpServers?: Record<string, McpServerConfig>;
  resume?: string;
}

export interface AgentStartOpts extends AgentConfig {
  prompt?: string;
  onEvent?: (event: AgentEvent) => void;
  onError?: (data: string) => void;
  onExit?: (exitCode: number) => void;
  onScrollbackEnd?: () => void;
}

export interface AgentSession {
  sessionId: string;
  done: Promise<number>;
  sendPrompt(text: string): void;
  interrupt(): void;
  configure(config: AgentConfig): void;
  kill(signal?: number): Promise<void>;
  close(): void;
}

export class Agent {
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

  async start(opts: AgentStartOpts = {}): Promise<AgentSession> {
    const body: Record<string, unknown> = {};
    if (opts.prompt) body.prompt = opts.prompt;
    if (opts.model) body.model = opts.model;
    if (opts.systemPrompt) body.systemPrompt = opts.systemPrompt;
    if (opts.allowedTools) body.allowedTools = opts.allowedTools;
    if (opts.permissionMode) body.permissionMode = opts.permissionMode;
    if (opts.maxTurns != null) body.maxTurns = opts.maxTurns;
    if (opts.cwd) body.cwd = opts.cwd;
    if (opts.mcpServers) body.mcpServers = opts.mcpServers;
    if (opts.resume) body.resume = opts.resume;

    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/agent`, {
      method: "POST",
      headers: this.headers,
      body: JSON.stringify(body),
    });

    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`Failed to create agent session: ${resp.status} ${text}`);
    }

    const data = await resp.json();
    const sessionId: string = data.sessionID;

    return this.attach(sessionId, opts);
  }

  async attach(sessionId: string, opts: Omit<AgentStartOpts, "prompt" | keyof AgentConfig> = {}): Promise<AgentSession> {
    // Connect via the existing exec session WebSocket endpoint
    const wsUrl = this.apiUrl
      .replace("http://", "ws://")
      .replace("https://", "wss://");
    const tokenParam = this.token ? `?token=${encodeURIComponent(this.token)}` : "";
    const wsEndpoint = `${wsUrl}/sandboxes/${this.sandboxId}/exec/${sessionId}${tokenParam}`;

    const ws = new WebSocket(wsEndpoint);
    ws.binaryType = "arraybuffer";

    let gotExit = false;
    let resolveDone: (code: number) => void;
    const done = new Promise<number>((resolve) => { resolveDone = resolve; });

    // Line buffer for JSON-line parsing from stdout
    let lineBuf = "";

    const parseLines = (text: string) => {
      lineBuf += text;
      const lines = lineBuf.split("\n");
      // Keep incomplete last line in buffer
      lineBuf = lines.pop() ?? "";
      for (const line of lines) {
        const trimmed = line.trim();
        if (!trimmed) continue;
        try {
          const event: AgentEvent = JSON.parse(trimmed);
          opts.onEvent?.(event);
        } catch {
          // Not JSON — treat as raw output
          opts.onError?.(`non-JSON stdout: ${trimmed}`);
        }
      }
    };

    const decoder = new TextDecoder();

    ws.onmessage = (event) => {
      const buf = new Uint8Array(event.data as ArrayBuffer);
      if (buf.length < 1) return;

      const streamId = buf[0];
      const payload = buf.slice(1);

      switch (streamId) {
        case 0x01: // stdout — JSON lines from wrapper
          parseLines(decoder.decode(payload, { stream: true }));
          break;
        case 0x02: // stderr — wrapper's own logs
          opts.onError?.(decoder.decode(payload, { stream: true }));
          break;
        case 0x03: // exit
          gotExit = true;
          if (payload.length >= 4) {
            const view = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
            const exitCode = view.getInt32(0, false);
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
      // Flush remaining line buffer
      if (lineBuf.trim()) {
        try {
          const event: AgentEvent = JSON.parse(lineBuf.trim());
          opts.onEvent?.(event);
        } catch {
          opts.onError?.(`non-JSON stdout: ${lineBuf.trim()}`);
        }
        lineBuf = "";
      }
      if (!gotExit) {
        opts.onExit?.(-1);
        resolveDone(-1);
      }
    };

    ws.onerror = () => {
      if (!gotExit) {
        opts.onExit?.(-1);
        resolveDone(-1);
      }
    };

    const agent = this;

    const sendStdin = (data: string) => {
      if (ws.readyState !== WebSocket.OPEN) return;
      const payload = new TextEncoder().encode(data);
      const msg = new Uint8Array(1 + payload.length);
      msg[0] = 0x00;
      msg.set(payload, 1);
      ws.send(msg);
    };

    return {
      sessionId,
      done,
      sendPrompt(text: string): void {
        sendStdin(JSON.stringify({ type: "prompt", text }) + "\n");
      },
      interrupt(): void {
        sendStdin(JSON.stringify({ type: "interrupt" }) + "\n");
      },
      configure(config: AgentConfig): void {
        sendStdin(JSON.stringify({ type: "configure", ...config }) + "\n");
      },
      async kill(signal?: number): Promise<void> {
        const body: Record<string, unknown> = {};
        if (signal != null) body.signal = signal;

        const resp = await fetch(`${agent.apiUrl}/sandboxes/${agent.sandboxId}/agent/${sessionId}/kill`, {
          method: "POST",
          headers: agent.headers,
          body: JSON.stringify(body),
        });

        if (!resp.ok) {
          const text = await resp.text();
          throw new Error(`Failed to kill agent session: ${resp.status} ${text}`);
        }
      },
      close(): void {
        ws.close();
      },
    };
  }

  async list(): Promise<Array<{ sessionID: string; sandboxID: string; running: boolean; startedAt: string }>> {
    const resp = await fetch(`${this.apiUrl}/sandboxes/${this.sandboxId}/agent`, {
      headers: this.headers,
    });

    if (!resp.ok) {
      throw new Error(`Failed to list agent sessions: ${resp.status}`);
    }

    return resp.json();
  }
}
