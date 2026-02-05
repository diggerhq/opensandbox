/**
 * Sandbox session class for interacting with a running sandbox.
 */

import * as grpc from '@grpc/grpc-js';
import type {
  CommandResult,
  RunOptions,
  FileEntry,
  BackgroundRunOptions,
  BackgroundRunResult,
} from './types';
import {
  SandboxNotFoundError,
  CommandExecutionError,
  FileOperationError,
} from './errors';

// gRPC client stub type (generated from proto)
export interface SandboxServiceClient {
  runCommand(
    request: RunCommandRequest,
    callback: (error: grpc.ServiceError | null, response: RunCommandResponse) => void
  ): void;
  writeFile(
    request: WriteFileRequest,
    callback: (error: grpc.ServiceError | null, response: WriteFileResponse) => void
  ): void;
  writeFiles(
    request: GrpcWriteFilesRequest,
    callback: (error: grpc.ServiceError | null, response: GrpcWriteFilesResponse) => void
  ): void;
  readFile(
    request: ReadFileRequest,
    callback: (error: grpc.ServiceError | null, response: ReadFileResponse) => void
  ): void;
  setEnv(
    request: SetEnvRequest,
    callback: (error: grpc.ServiceError | null, response: SetEnvResponse) => void
  ): void;
  setCwd(
    request: SetCwdRequest,
    callback: (error: grpc.ServiceError | null, response: SetCwdResponse) => void
  ): void;
}

// Request/Response types matching the proto definitions
interface RunCommandRequest {
  session_id: string;
  command: string[];
  time_ms: number;
  mem_kb: number;
  fsize_kb: number;
  nofile: number;
  env: Record<string, string>;
  cwd: string;
}

interface RunCommandResponse {
  stdout: string;
  stderr: string;
  exit_code: number;
  signal: number;
}

interface WriteFileRequest {
  session_id: string;
  path: string;
  content: Buffer;
}

interface WriteFileResponse {
  success: boolean;
  error: string;
}

interface ReadFileRequest {
  session_id: string;
  path: string;
}

interface ReadFileResponse {
  content: Buffer;
  error: string;
}

interface SetEnvRequest {
  session_id: string;
  env: Record<string, string>;
}

interface SetEnvResponse {
  success: boolean;
}

interface SetCwdRequest {
  session_id: string;
  cwd: string;
}

interface SetCwdResponse {
  success: boolean;
}

interface GrpcBulkFileEntry {
  path: string;
  content: Buffer;
}

interface GrpcWriteFilesRequest {
  session_id: string;
  files: GrpcBulkFileEntry[];
}

interface GrpcFileError {
  path: string;
  error: string;
}

interface GrpcWriteFilesResponse {
  success: boolean;
  errors: GrpcFileError[];
}

/**
 * A sandbox session that can execute commands and manage files.
 *
 * This class uses gRPC for fast command execution and file operations,
 * with HTTP fallback for session lifecycle management.
 */
export class Sandbox {
  /** Unique session identifier */
  readonly sessionId: string;

  /** Preview URL for accessing web servers in the sandbox */
  readonly previewUrl: string | null;

  private _grpcStub: SandboxServiceClient | null;
  private _httpBaseUrl: string;
  private _destroyed = false;

  constructor(
    sessionId: string,
    grpcStub: SandboxServiceClient | null,
    httpBaseUrl: string,
    previewUrl: string | null = null
  ) {
    this.sessionId = sessionId;
    this._grpcStub = grpcStub;
    this._httpBaseUrl = httpBaseUrl;
    this.previewUrl = previewUrl;
  }

  /**
   * Execute a shell command in the sandbox.
   *
   * @param command - The shell command to execute
   * @param options - Execution options (timeout, memory, etc.)
   * @returns Command result with stdout, stderr, exitCode, and signal
   * @throws {SandboxNotFoundError} If the session no longer exists
   * @throws {CommandExecutionError} If there was an error executing the command
   */
  async run(command: string, options: RunOptions = {}): Promise<CommandResult> {
    if (this._destroyed) {
      throw new SandboxNotFoundError('Sandbox has been destroyed');
    }

    const {
      timeoutMs = 300000,
      memKb = 2097152,
      fsizeKb = 1048576,
      nofile = 256,
      env = {},
      cwd = '',
    } = options;

    // Try gRPC first if available
    if (this._grpcStub) {
      return this._runViaGrpc(command, timeoutMs, memKb, fsizeKb, nofile, env, cwd);
    }

    // Fall back to HTTP
    return this._runViaHttp(command, timeoutMs, memKb, fsizeKb, nofile, env, cwd);
  }

  private async _runViaGrpc(
    command: string,
    timeoutMs: number,
    memKb: number,
    fsizeKb: number,
    nofile: number,
    env: Record<string, string>,
    cwd: string
  ): Promise<CommandResult> {
    const request: RunCommandRequest = {
      session_id: this.sessionId,
      command: ['/bin/sh', '-c', command],
      time_ms: timeoutMs,
      mem_kb: memKb,
      fsize_kb: fsizeKb,
      nofile: nofile,
      env: env,
      cwd: cwd,
    };

    return new Promise((resolve, reject) => {
      this._grpcStub!.runCommand(request, (error, response) => {
        if (error) {
          if (error.code === grpc.status.NOT_FOUND) {
            reject(new SandboxNotFoundError('Session not found'));
          } else {
            reject(new CommandExecutionError(`gRPC error: ${error.message}`));
          }
          return;
        }
        resolve({
          stdout: response.stdout,
          stderr: response.stderr,
          exitCode: response.exit_code,
          signal: response.signal,
        });
      });
    });
  }

  private async _runViaHttp(
    command: string,
    timeoutMs: number,
    memKb: number,
    fsizeKb: number,
    nofile: number,
    env: Record<string, string>,
    cwd: string
  ): Promise<CommandResult> {
    const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/run`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        command: ['/bin/sh', '-c', command],
        time: timeoutMs,
        mem: memKb,
        fsize: fsizeKb,
        nofile: nofile,
        env: env,
        cwd: cwd || '/',
      }),
    });

    if (!response.ok) {
      if (response.status === 404) {
        throw new SandboxNotFoundError('Session not found');
      }
      const text = await response.text();
      throw new CommandExecutionError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
    }

    const result = await response.json() as {
      stdout: string;
      stderr: string;
      exit_code: number | null;
      signal: number | null;
    };

    return {
      stdout: result.stdout,
      stderr: result.stderr,
      exitCode: result.exit_code ?? 0,
      signal: result.signal ?? 0,
    };
  }

  /**
   * Write content to a file in the sandbox.
   *
   * @param path - Absolute path in the sandbox filesystem
   * @param content - File content (string or Buffer)
   * @throws {SandboxNotFoundError} If the session no longer exists
   * @throws {FileOperationError} If the file cannot be written
   */
  async writeFile(path: string, content: string | Buffer): Promise<void> {
    if (this._destroyed) {
      throw new SandboxNotFoundError('Sandbox has been destroyed');
    }

    const buffer = typeof content === 'string' ? Buffer.from(content, 'utf-8') : content;

    // Try gRPC first if available
    if (this._grpcStub) {
      return this._writeFileViaGrpc(path, buffer);
    }

    // Fall back to HTTP
    return this._writeFileViaHttp(path, buffer);
  }

  private async _writeFileViaGrpc(path: string, content: Buffer): Promise<void> {
    const request: WriteFileRequest = {
      session_id: this.sessionId,
      path: path,
      content: content,
    };

    return new Promise((resolve, reject) => {
      this._grpcStub!.writeFile(request, (error, response) => {
        if (error) {
          if (error.code === grpc.status.NOT_FOUND) {
            reject(new SandboxNotFoundError('Session not found'));
          } else {
            reject(new FileOperationError(`gRPC error: ${error.message}`));
          }
          return;
        }
        if (!response.success) {
          reject(new FileOperationError(`Failed to write file: ${response.error}`));
          return;
        }
        resolve();
      });
    });
  }

  private async _writeFileViaHttp(path: string, content: Buffer): Promise<void> {
    const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/files/write`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        path: path,
        content: content.toString('base64'),
      }),
    });

    if (!response.ok) {
      if (response.status === 404) {
        throw new SandboxNotFoundError('Session not found');
      }
      const text = await response.text();
      throw new FileOperationError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
    }
  }

  /**
   * Write multiple files to the sandbox in a single request.
   *
   * @param files - Array of files with path and content
   * @throws {SandboxNotFoundError} If the session no longer exists
   * @throws {FileOperationError} If any file cannot be written (partial success possible)
   */
  async writeFiles(files: Array<{ path: string; content: string | Buffer }>): Promise<void> {
    if (this._destroyed) {
      throw new SandboxNotFoundError('Sandbox has been destroyed');
    }

    const buffered = files.map((f) => ({
      path: f.path,
      content: typeof f.content === 'string' ? Buffer.from(f.content, 'utf-8') : f.content,
    }));

    // Try gRPC first if available
    if (this._grpcStub) {
      return this._writeFilesViaGrpc(buffered);
    }

    // Fall back to HTTP
    return this._writeFilesViaHttp(buffered);
  }

  private async _writeFilesViaGrpc(files: Array<{ path: string; content: Buffer }>): Promise<void> {
    const request: GrpcWriteFilesRequest = {
      session_id: this.sessionId,
      files: files.map((f) => ({ path: f.path, content: f.content })),
    };

    return new Promise((resolve, reject) => {
      this._grpcStub!.writeFiles(request, (error, response) => {
        if (error) {
          if (error.code === grpc.status.NOT_FOUND) {
            reject(new SandboxNotFoundError('Session not found'));
          } else {
            reject(new FileOperationError(`gRPC error: ${error.message}`));
          }
          return;
        }
        if (!response.success) {
          const msgs = response.errors.map((e: GrpcFileError) => `${e.path}: ${e.error}`).join('; ');
          reject(new FileOperationError(`Failed to write files: ${msgs}`));
          return;
        }
        resolve();
      });
    });
  }

  private async _writeFilesViaHttp(files: Array<{ path: string; content: Buffer }>): Promise<void> {
    const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/files/write-bulk`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        files: files.map((f) => ({
          path: f.path,
          content: f.content.toString('base64'),
        })),
      }),
    });

    if (!response.ok) {
      if (response.status === 404) {
        throw new SandboxNotFoundError('Session not found');
      }
      const text = await response.text();
      throw new FileOperationError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
    }

    const result = await response.json() as { success: boolean; errors: Array<{ path: string; error: string }> };
    if (!result.success) {
      const msgs = result.errors.map((e) => `${e.path}: ${e.error}`).join('; ');
      throw new FileOperationError(`Failed to write files: ${msgs}`);
    }
  }

  /**
   * Read a file from the sandbox.
   *
   * @param path - Absolute path in the sandbox filesystem
   * @returns File contents as a Buffer
   * @throws {SandboxNotFoundError} If the session no longer exists
   * @throws {FileOperationError} If the file cannot be read
   */
  async readFile(path: string): Promise<Buffer> {
    if (this._destroyed) {
      throw new SandboxNotFoundError('Sandbox has been destroyed');
    }

    // Try gRPC first if available
    if (this._grpcStub) {
      return this._readFileViaGrpc(path);
    }

    // Fall back to HTTP
    return this._readFileViaHttp(path);
  }

  private async _readFileViaGrpc(path: string): Promise<Buffer> {
    const request: ReadFileRequest = {
      session_id: this.sessionId,
      path: path,
    };

    return new Promise((resolve, reject) => {
      this._grpcStub!.readFile(request, (error, response) => {
        if (error) {
          if (error.code === grpc.status.NOT_FOUND) {
            reject(new SandboxNotFoundError('Session not found'));
          } else {
            reject(new FileOperationError(`gRPC error: ${error.message}`));
          }
          return;
        }
        if (response.error) {
          reject(new FileOperationError(`Failed to read file: ${response.error}`));
          return;
        }
        resolve(Buffer.from(response.content));
      });
    });
  }

  private async _readFileViaHttp(path: string): Promise<Buffer> {
    const url = new URL(`${this._httpBaseUrl}/sessions/${this.sessionId}/files/read`);
    url.searchParams.set('path', path);

    const response = await fetch(url.toString());

    if (!response.ok) {
      if (response.status === 404) {
        throw new SandboxNotFoundError('Session not found');
      }
      const text = await response.text();
      throw new FileOperationError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
    }

    const result = await response.json() as { content: string };
    return Buffer.from(result.content, 'base64');
  }

  /**
   * Read a text file from the sandbox.
   *
   * @param path - Absolute path in the sandbox filesystem
   * @param encoding - Text encoding (default: utf-8)
   * @returns File contents as a string
   */
  async readFileText(path: string, encoding: BufferEncoding = 'utf-8'): Promise<string> {
    const content = await this.readFile(path);
    return content.toString(encoding);
  }

  /**
   * List files in a directory within the sandbox.
   *
   * @param path - Directory path in the sandbox filesystem
   * @returns Array of file entries
   * @throws {SandboxNotFoundError} If the session no longer exists
   * @throws {FileOperationError} If the directory cannot be listed
   */
  async listFiles(path: string): Promise<FileEntry[]> {
    if (this._destroyed) {
      throw new SandboxNotFoundError('Sandbox has been destroyed');
    }

    const url = new URL(`${this._httpBaseUrl}/sessions/${this.sessionId}/files/list`);
    url.searchParams.set('path', path);

    const response = await fetch(url.toString());

    if (!response.ok) {
      if (response.status === 404) {
        throw new SandboxNotFoundError('Session not found');
      }
      const text = await response.text();
      throw new FileOperationError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
    }

    const result = await response.json() as {
      files: Array<{
        name: string;
        path: string;
        is_directory: boolean;
        size: number;
      }>;
    };

    return result.files.map((f) => ({
      name: f.name,
      path: f.path,
      isDirectory: f.is_directory,
      size: f.size,
    }));
  }

  /**
   * Set environment variables for subsequent commands.
   *
   * @param env - Dictionary of environment variables to set
   * @throws {SandboxNotFoundError} If the session no longer exists
   */
  async setEnv(env: Record<string, string>): Promise<void> {
    if (this._destroyed) {
      throw new SandboxNotFoundError('Sandbox has been destroyed');
    }

    // Try gRPC first if available
    if (this._grpcStub) {
      return this._setEnvViaGrpc(env);
    }

    // Fall back to HTTP
    return this._setEnvViaHttp(env);
  }

  private async _setEnvViaGrpc(env: Record<string, string>): Promise<void> {
    const request: SetEnvRequest = {
      session_id: this.sessionId,
      env: env,
    };

    return new Promise((resolve, reject) => {
      this._grpcStub!.setEnv(request, (error) => {
        if (error) {
          if (error.code === grpc.status.NOT_FOUND) {
            reject(new SandboxNotFoundError('Session not found'));
          } else {
            reject(error);
          }
          return;
        }
        resolve();
      });
    });
  }

  private async _setEnvViaHttp(env: Record<string, string>): Promise<void> {
    const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/env`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ env }),
    });

    if (!response.ok) {
      if (response.status === 404) {
        throw new SandboxNotFoundError('Session not found');
      }
      throw new Error(`HTTP error: ${response.status} ${response.statusText}`);
    }
  }

  /**
   * Set the working directory for subsequent commands.
   *
   * @param cwd - The new working directory path
   * @throws {SandboxNotFoundError} If the session no longer exists
   */
  async setCwd(cwd: string): Promise<void> {
    if (this._destroyed) {
      throw new SandboxNotFoundError('Sandbox has been destroyed');
    }

    // Try gRPC first if available
    if (this._grpcStub) {
      return this._setCwdViaGrpc(cwd);
    }

    // Fall back to HTTP
    return this._setCwdViaHttp(cwd);
  }

  private async _setCwdViaGrpc(cwd: string): Promise<void> {
    const request: SetCwdRequest = {
      session_id: this.sessionId,
      cwd: cwd,
    };

    return new Promise((resolve, reject) => {
      this._grpcStub!.setCwd(request, (error) => {
        if (error) {
          if (error.code === grpc.status.NOT_FOUND) {
            reject(new SandboxNotFoundError('Session not found'));
          } else {
            reject(error);
          }
          return;
        }
        resolve();
      });
    });
  }

  private async _setCwdViaHttp(cwd: string): Promise<void> {
    const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/cwd`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ cwd }),
    });

    if (!response.ok) {
      if (response.status === 404) {
        throw new SandboxNotFoundError('Session not found');
      }
      throw new Error(`HTTP error: ${response.status} ${response.statusText}`);
    }
  }

  /**
   * Start a long-running background process in the sandbox (e.g., a dev server).
   * Unlike `run()`, the process survives after the call returns.
   *
   * @param command - The shell command to execute (e.g., "npm run dev")
   * @param options - Background run options (port, env, cwd)
   * @returns Background process result with PID, port, and preview URL
   * @throws {SandboxNotFoundError} If the session no longer exists
   * @throws {CommandExecutionError} If the process couldn't be started
   */
  async startBackground(command: string, options: BackgroundRunOptions = {}): Promise<BackgroundRunResult> {
    if (this._destroyed) {
      throw new SandboxNotFoundError('Sandbox has been destroyed');
    }

    const { port = 5173, env = {}, cwd = '' } = options;

    const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/background`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        command: ['/bin/sh', '-c', command],
        port,
        env,
        cwd: cwd || '/',
      }),
    });

    if (!response.ok) {
      if (response.status === 404) {
        throw new SandboxNotFoundError('Session not found');
      }
      const text = await response.text();
      throw new CommandExecutionError(`Failed to start background process: ${response.status} - ${text}`);
    }

    const result = await response.json() as {
      pid: number;
      port: number;
      preview_url: string | null;
    };

    return {
      pid: result.pid,
      port: result.port,
      previewUrl: result.preview_url,
    };
  }

  /**
   * Kill all background processes in this sandbox.
   *
   * Use this before starting a new background process to avoid port conflicts.
   */
  async killBackground(): Promise<{ killed: number[]; total: number }> {
    if (this._destroyed) {
      throw new SandboxNotFoundError('Sandbox has been destroyed');
    }

    const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/background`, {
      method: 'DELETE',
    });

    if (!response.ok) {
      if (response.status === 404) {
        throw new SandboxNotFoundError('Session not found');
      }
      const text = await response.text();
      throw new CommandExecutionError(`Failed to kill background processes: ${response.status} - ${text}`);
    }

    return await response.json() as { killed: number[]; total: number };
  }

  /**
   * Destroy this sandbox session.
   *
   * After calling this method, the sandbox cannot be used anymore.
   */
  async destroy(): Promise<void> {
    if (this._destroyed) {
      return;
    }

    try {
      const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}`, {
        method: 'DELETE',
      });
      // Best effort cleanup - ignore errors
      void response;
    } catch {
      // Ignore errors during cleanup
    } finally {
      this._destroyed = true;
    }
  }
}
