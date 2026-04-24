import type { ExecSession, ProcessResult } from "./exec.js";

export interface ShellOpts {
  cwd?: string;
  env?: Record<string, string>;
}

export interface ShellRunOpts {
  onStdout?: (data: Uint8Array) => void;
  onStderr?: (data: Uint8Array) => void;
}

export interface Shell {
  readonly sessionId: string;
  /**
   * Run a command inside the shell and wait for it to complete.
   *
   * State set by previous calls (cwd, exported variables, shell functions)
   * persists into this call. Rejects with `ShellBusyError` if another run is
   * in flight, or `ShellClosedError` if the shell has exited (including when
   * the user command calls `exit` — same as closing a terminal tab).
   *
   * Per-call `cwd`/`env`/`timeout` are intentionally not supported in v1 —
   * use shell syntax inline (`cd /x && cmd`, `FOO=bar cmd`). Timeouts will
   * land once we have a "signal foreground job" primitive.
   */
  run(cmd: string, opts?: ShellRunOpts): Promise<ProcessResult>;
  /**
   * Write `exit` to the shell and wait for bash to terminate.
   */
  close(): Promise<void>;
}

export class ShellBusyError extends Error {
  constructor() {
    super("shell is already running a command; shell.run is foreground-only");
    this.name = "ShellBusyError";
  }
}

export class ShellClosedError extends Error {
  constructor(reason?: string) {
    super(reason ? `shell is closed: ${reason}` : "shell is closed");
    this.name = "ShellClosedError";
  }
}

type State = "idle" | "running" | "closed";

interface PendingRun {
  token: string;
  stdoutDoneMarker: string;      // "\n__OC_<TOK>_DONE__\n"
  stderrExitPrefix: string;      // "\n__OC_<TOK>_EXIT_"
  stdoutBuf: string;
  stderrBuf: string;
  stdoutScanIdx: number;         // bytes emitted to onStdout
  stderrScanIdx: number;         // bytes emitted to onStderr
  stdoutDoneIdx: number | null;  // index of DONE marker in stdoutBuf, once found
  stderrExitCode: number | null; // parsed exit code, once EXIT sentinel found
  stderrExitIdx: number | null;  // index of EXIT sentinel start in stderrBuf
  onStdout?: (data: Uint8Array) => void;
  onStderr?: (data: Uint8Array) => void;
  resolve: (r: ProcessResult) => void;
  reject: (e: Error) => void;
}

const encoder = new TextEncoder();

/**
 * Hex-ish 128-bit token; `crypto.randomUUID` is Node 18+ / browser standard.
 * Fallback uses `Math.random` only if the global `crypto` is absent.
 */
function newToken(): string {
  const g = globalThis as { crypto?: { randomUUID?: () => string } };
  if (g.crypto?.randomUUID) return g.crypto.randomUUID().replace(/-/g, "");
  let s = "";
  for (let i = 0; i < 32; i++) s += Math.floor(Math.random() * 16).toString(16);
  return s;
}

export class ShellImpl implements Shell {
  readonly sessionId: string;
  private session: ExecSession;
  private state: State = "idle";
  private pending: PendingRun | null = null;
  private stdoutDecoder = new TextDecoder("utf-8", { fatal: false });
  private stderrDecoder = new TextDecoder("utf-8", { fatal: false });
  // Chunks arriving before scrollback_end (0x04) are historical replay of
  // the session's prior output. Drop them — our sentinel protocol only
  // matches per-call tokens, but the caller's onStdout/onStderr callbacks
  // should not see stale data either.
  private scrollbackDone = false;

  constructor(session: ExecSession) {
    this.session = session;
    this.sessionId = session.sessionId;

    // If bash exits while a run is pending, fail the pending run.
    this.session.done.then((exitCode) => {
      const prev = this.state;
      this.state = "closed";
      if (this.pending) {
        const p = this.pending;
        this.pending = null;
        if (prev !== "idle") {
          p.reject(new ShellClosedError(`bash exited with code ${exitCode}`));
        }
      }
    });
  }

  onScrollbackEnd(): void {
    this.scrollbackDone = true;
  }

  onStdoutChunk(chunk: Uint8Array): void {
    if (!this.scrollbackDone) return;
    const p = this.pending;
    if (!p) return;
    p.stdoutBuf += this.stdoutDecoder.decode(chunk, { stream: true });
    this.scanStdout();
  }

  onStderrChunk(chunk: Uint8Array): void {
    if (!this.scrollbackDone) return;
    const p = this.pending;
    if (!p) return;
    p.stderrBuf += this.stderrDecoder.decode(chunk, { stream: true });
    this.scanStderr();
  }

  /** Extract user stdout up to the DONE marker; emit incremental chunks. */
  private scanStdout(): void {
    const p = this.pending;
    if (!p || p.stdoutDoneIdx !== null) return;

    const idx = p.stdoutBuf.indexOf(p.stdoutDoneMarker, p.stdoutScanIdx);

    if (idx === -1) {
      // Hold back a tail that could be a partial marker.
      const hold = p.stdoutDoneMarker.length - 1;
      const safe = Math.max(p.stdoutScanIdx, p.stdoutBuf.length - hold);
      if (safe > p.stdoutScanIdx) {
        const chunk = p.stdoutBuf.slice(p.stdoutScanIdx, safe);
        p.onStdout?.(encoder.encode(chunk));
        p.stdoutScanIdx = safe;
      }
      return;
    }

    // Emit any un-emitted bytes before the marker.
    if (idx > p.stdoutScanIdx) {
      const chunk = p.stdoutBuf.slice(p.stdoutScanIdx, idx);
      p.onStdout?.(encoder.encode(chunk));
      p.stdoutScanIdx = idx;
    }

    p.stdoutDoneIdx = idx;
    this.tryComplete();
  }

  /** Extract user stderr up to the EXIT sentinel; parse the exit code. */
  private scanStderr(): void {
    const p = this.pending;
    if (!p || p.stderrExitCode !== null) return;

    const idx = p.stderrBuf.indexOf(p.stderrExitPrefix, p.stderrScanIdx);

    if (idx === -1) {
      const hold = p.stderrExitPrefix.length - 1;
      const safe = Math.max(p.stderrScanIdx, p.stderrBuf.length - hold);
      if (safe > p.stderrScanIdx) {
        const chunk = p.stderrBuf.slice(p.stderrScanIdx, safe);
        p.onStderr?.(encoder.encode(chunk));
        p.stderrScanIdx = safe;
      }
      return;
    }

    if (idx > p.stderrScanIdx) {
      const chunk = p.stderrBuf.slice(p.stderrScanIdx, idx);
      p.onStderr?.(encoder.encode(chunk));
      p.stderrScanIdx = idx;
    }

    const afterPrefix = idx + p.stderrExitPrefix.length;
    const closeIdx = p.stderrBuf.indexOf("__", afterPrefix);
    if (closeIdx === -1) return; // wait for more bytes

    const exitStr = p.stderrBuf.slice(afterPrefix, closeIdx);
    const exitCode = parseInt(exitStr, 10);
    if (!Number.isFinite(exitCode)) {
      this.state = "closed";
      this.pending = null;
      p.reject(new ShellClosedError(`corrupt sentinel: "${exitStr}"`));
      return;
    }

    p.stderrExitCode = exitCode;
    p.stderrExitIdx = idx;
    this.tryComplete();
  }

  /** Resolve once both markers are in — guarantees both pipes are drained. */
  private tryComplete(): void {
    const p = this.pending;
    if (!p) return;
    if (p.stdoutDoneIdx === null || p.stderrExitCode === null) return;

    const result: ProcessResult = {
      exitCode: p.stderrExitCode,
      stdout: p.stdoutBuf.slice(0, p.stdoutDoneIdx),
      stderr: p.stderrBuf.slice(0, p.stderrExitIdx ?? p.stderrBuf.length),
    };

    this.pending = null;
    if (this.state !== "closed") this.state = "idle";
    p.resolve(result);
  }

  async run(cmd: string, opts: ShellRunOpts = {}): Promise<ProcessResult> {
    if (this.state === "closed") throw new ShellClosedError();
    if (this.state === "running") throw new ShellBusyError();

    const token = newToken();
    const stdoutDoneMarker = `\n__OC_${token}_DONE__\n`;
    const stderrExitPrefix = `\n__OC_${token}_EXIT_`;

    // `{ cmd\n}` groups the user command so `$?` captures its exit. The
    // newline before `}` is required — `{ cmd\n; }` is a bash syntax error.
    //
    // Two markers, one per stream: DONE to stdout, EXIT to stderr. We
    // resolve only when both are seen, which prevents a race where the
    // stderr sentinel arrives before the user's stdout is drained (the
    // agent reads stdout and stderr on separate goroutines, so wire
    // ordering isn't guaranteed). Empty command becomes `:` (POSIX no-op).
    const inner = cmd.trim() === "" ? ":" : cmd;
    const wrapped =
      `{ ${inner}\n} ; __oc_ec=$? ; ` +
      `printf '\\n__OC_%s_DONE__\\n' '${token}' ; ` +
      `printf '\\n__OC_%s_EXIT_%d__\\n' '${token}' "$__oc_ec" >&2\n`;

    this.state = "running";
    const run: PendingRun = {
      token,
      stdoutDoneMarker,
      stderrExitPrefix,
      stdoutBuf: "",
      stderrBuf: "",
      stdoutScanIdx: 0,
      stderrScanIdx: 0,
      stdoutDoneIdx: null,
      stderrExitCode: null,
      stderrExitIdx: null,
      onStdout: opts.onStdout,
      onStderr: opts.onStderr,
      resolve: () => {},
      reject: () => {},
    };
    const promise = new Promise<ProcessResult>((resolve, reject) => {
      run.resolve = resolve;
      run.reject = reject;
    });
    this.pending = run;

    try {
      this.session.sendStdin(wrapped);
    } catch (err) {
      this.pending = null;
      this.state = "closed";
      throw err;
    }

    return promise;
  }

  async close(): Promise<void> {
    if (this.state === "closed") return;
    this.state = "closed";
    try {
      this.session.sendStdin("exit\n");
    } catch {
      // session already dead
    }
    try {
      await this.session.done;
    } finally {
      this.session.close();
    }
  }
}
