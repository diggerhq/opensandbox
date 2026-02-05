"use strict";
/**
 * Sandbox session class for interacting with a running sandbox.
 */
var __createBinding = (this && this.__createBinding) || (Object.create ? (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    var desc = Object.getOwnPropertyDescriptor(m, k);
    if (!desc || ("get" in desc ? !m.__esModule : desc.writable || desc.configurable)) {
      desc = { enumerable: true, get: function() { return m[k]; } };
    }
    Object.defineProperty(o, k2, desc);
}) : (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    o[k2] = m[k];
}));
var __setModuleDefault = (this && this.__setModuleDefault) || (Object.create ? (function(o, v) {
    Object.defineProperty(o, "default", { enumerable: true, value: v });
}) : function(o, v) {
    o["default"] = v;
});
var __importStar = (this && this.__importStar) || (function () {
    var ownKeys = function(o) {
        ownKeys = Object.getOwnPropertyNames || function (o) {
            var ar = [];
            for (var k in o) if (Object.prototype.hasOwnProperty.call(o, k)) ar[ar.length] = k;
            return ar;
        };
        return ownKeys(o);
    };
    return function (mod) {
        if (mod && mod.__esModule) return mod;
        var result = {};
        if (mod != null) for (var k = ownKeys(mod), i = 0; i < k.length; i++) if (k[i] !== "default") __createBinding(result, mod, k[i]);
        __setModuleDefault(result, mod);
        return result;
    };
})();
Object.defineProperty(exports, "__esModule", { value: true });
exports.Sandbox = void 0;
const grpc = __importStar(require("@grpc/grpc-js"));
const errors_1 = require("./errors");
/**
 * A sandbox session that can execute commands and manage files.
 *
 * This class uses gRPC for fast command execution and file operations,
 * with HTTP fallback for session lifecycle management.
 */
class Sandbox {
    /** Unique session identifier */
    sessionId;
    /** Preview URL for accessing web servers in the sandbox */
    previewUrl;
    _grpcStub;
    _httpBaseUrl;
    _destroyed = false;
    constructor(sessionId, grpcStub, httpBaseUrl, previewUrl = null) {
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
    async run(command, options = {}) {
        if (this._destroyed) {
            throw new errors_1.SandboxNotFoundError('Sandbox has been destroyed');
        }
        const { timeoutMs = 300000, memKb = 2097152, fsizeKb = 1048576, nofile = 256, env = {}, cwd = '', } = options;
        // Try gRPC first if available
        if (this._grpcStub) {
            return this._runViaGrpc(command, timeoutMs, memKb, fsizeKb, nofile, env, cwd);
        }
        // Fall back to HTTP
        return this._runViaHttp(command, timeoutMs, memKb, fsizeKb, nofile, env, cwd);
    }
    async _runViaGrpc(command, timeoutMs, memKb, fsizeKb, nofile, env, cwd) {
        const request = {
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
            this._grpcStub.runCommand(request, (error, response) => {
                if (error) {
                    if (error.code === grpc.status.NOT_FOUND) {
                        reject(new errors_1.SandboxNotFoundError('Session not found'));
                    }
                    else {
                        reject(new errors_1.CommandExecutionError(`gRPC error: ${error.message}`));
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
    async _runViaHttp(command, timeoutMs, memKb, fsizeKb, nofile, env, cwd) {
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
                throw new errors_1.SandboxNotFoundError('Session not found');
            }
            const text = await response.text();
            throw new errors_1.CommandExecutionError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
        }
        const result = await response.json();
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
    async writeFile(path, content) {
        if (this._destroyed) {
            throw new errors_1.SandboxNotFoundError('Sandbox has been destroyed');
        }
        const buffer = typeof content === 'string' ? Buffer.from(content, 'utf-8') : content;
        // Try gRPC first if available
        if (this._grpcStub) {
            return this._writeFileViaGrpc(path, buffer);
        }
        // Fall back to HTTP
        return this._writeFileViaHttp(path, buffer);
    }
    async _writeFileViaGrpc(path, content) {
        const request = {
            session_id: this.sessionId,
            path: path,
            content: content,
        };
        return new Promise((resolve, reject) => {
            this._grpcStub.writeFile(request, (error, response) => {
                if (error) {
                    if (error.code === grpc.status.NOT_FOUND) {
                        reject(new errors_1.SandboxNotFoundError('Session not found'));
                    }
                    else {
                        reject(new errors_1.FileOperationError(`gRPC error: ${error.message}`));
                    }
                    return;
                }
                if (!response.success) {
                    reject(new errors_1.FileOperationError(`Failed to write file: ${response.error}`));
                    return;
                }
                resolve();
            });
        });
    }
    async _writeFileViaHttp(path, content) {
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
                throw new errors_1.SandboxNotFoundError('Session not found');
            }
            const text = await response.text();
            throw new errors_1.FileOperationError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
        }
    }
    /**
     * Write multiple files to the sandbox in a single request.
     *
     * @param files - Array of files with path and content
     * @throws {SandboxNotFoundError} If the session no longer exists
     * @throws {FileOperationError} If any file cannot be written (partial success possible)
     */
    async writeFiles(files) {
        if (this._destroyed) {
            throw new errors_1.SandboxNotFoundError('Sandbox has been destroyed');
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
    async _writeFilesViaGrpc(files) {
        const request = {
            session_id: this.sessionId,
            files: files.map((f) => ({ path: f.path, content: f.content })),
        };
        return new Promise((resolve, reject) => {
            this._grpcStub.writeFiles(request, (error, response) => {
                if (error) {
                    if (error.code === grpc.status.NOT_FOUND) {
                        reject(new errors_1.SandboxNotFoundError('Session not found'));
                    }
                    else {
                        reject(new errors_1.FileOperationError(`gRPC error: ${error.message}`));
                    }
                    return;
                }
                if (!response.success) {
                    const msgs = response.errors.map((e) => `${e.path}: ${e.error}`).join('; ');
                    reject(new errors_1.FileOperationError(`Failed to write files: ${msgs}`));
                    return;
                }
                resolve();
            });
        });
    }
    async _writeFilesViaHttp(files) {
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
                throw new errors_1.SandboxNotFoundError('Session not found');
            }
            const text = await response.text();
            throw new errors_1.FileOperationError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
        }
        const result = await response.json();
        if (!result.success) {
            const msgs = result.errors.map((e) => `${e.path}: ${e.error}`).join('; ');
            throw new errors_1.FileOperationError(`Failed to write files: ${msgs}`);
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
    async readFile(path) {
        if (this._destroyed) {
            throw new errors_1.SandboxNotFoundError('Sandbox has been destroyed');
        }
        // Try gRPC first if available
        if (this._grpcStub) {
            return this._readFileViaGrpc(path);
        }
        // Fall back to HTTP
        return this._readFileViaHttp(path);
    }
    async _readFileViaGrpc(path) {
        const request = {
            session_id: this.sessionId,
            path: path,
        };
        return new Promise((resolve, reject) => {
            this._grpcStub.readFile(request, (error, response) => {
                if (error) {
                    if (error.code === grpc.status.NOT_FOUND) {
                        reject(new errors_1.SandboxNotFoundError('Session not found'));
                    }
                    else {
                        reject(new errors_1.FileOperationError(`gRPC error: ${error.message}`));
                    }
                    return;
                }
                if (response.error) {
                    reject(new errors_1.FileOperationError(`Failed to read file: ${response.error}`));
                    return;
                }
                resolve(Buffer.from(response.content));
            });
        });
    }
    async _readFileViaHttp(path) {
        const url = new URL(`${this._httpBaseUrl}/sessions/${this.sessionId}/files/read`);
        url.searchParams.set('path', path);
        const response = await fetch(url.toString());
        if (!response.ok) {
            if (response.status === 404) {
                throw new errors_1.SandboxNotFoundError('Session not found');
            }
            const text = await response.text();
            throw new errors_1.FileOperationError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
        }
        const result = await response.json();
        return Buffer.from(result.content, 'base64');
    }
    /**
     * Read a text file from the sandbox.
     *
     * @param path - Absolute path in the sandbox filesystem
     * @param encoding - Text encoding (default: utf-8)
     * @returns File contents as a string
     */
    async readFileText(path, encoding = 'utf-8') {
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
    async listFiles(path) {
        if (this._destroyed) {
            throw new errors_1.SandboxNotFoundError('Sandbox has been destroyed');
        }
        const url = new URL(`${this._httpBaseUrl}/sessions/${this.sessionId}/files/list`);
        url.searchParams.set('path', path);
        const response = await fetch(url.toString());
        if (!response.ok) {
            if (response.status === 404) {
                throw new errors_1.SandboxNotFoundError('Session not found');
            }
            const text = await response.text();
            throw new errors_1.FileOperationError(`HTTP error: ${response.status} ${response.statusText} - ${text}`);
        }
        const result = await response.json();
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
    async setEnv(env) {
        if (this._destroyed) {
            throw new errors_1.SandboxNotFoundError('Sandbox has been destroyed');
        }
        // Try gRPC first if available
        if (this._grpcStub) {
            return this._setEnvViaGrpc(env);
        }
        // Fall back to HTTP
        return this._setEnvViaHttp(env);
    }
    async _setEnvViaGrpc(env) {
        const request = {
            session_id: this.sessionId,
            env: env,
        };
        return new Promise((resolve, reject) => {
            this._grpcStub.setEnv(request, (error) => {
                if (error) {
                    if (error.code === grpc.status.NOT_FOUND) {
                        reject(new errors_1.SandboxNotFoundError('Session not found'));
                    }
                    else {
                        reject(error);
                    }
                    return;
                }
                resolve();
            });
        });
    }
    async _setEnvViaHttp(env) {
        const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/env`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ env }),
        });
        if (!response.ok) {
            if (response.status === 404) {
                throw new errors_1.SandboxNotFoundError('Session not found');
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
    async setCwd(cwd) {
        if (this._destroyed) {
            throw new errors_1.SandboxNotFoundError('Sandbox has been destroyed');
        }
        // Try gRPC first if available
        if (this._grpcStub) {
            return this._setCwdViaGrpc(cwd);
        }
        // Fall back to HTTP
        return this._setCwdViaHttp(cwd);
    }
    async _setCwdViaGrpc(cwd) {
        const request = {
            session_id: this.sessionId,
            cwd: cwd,
        };
        return new Promise((resolve, reject) => {
            this._grpcStub.setCwd(request, (error) => {
                if (error) {
                    if (error.code === grpc.status.NOT_FOUND) {
                        reject(new errors_1.SandboxNotFoundError('Session not found'));
                    }
                    else {
                        reject(error);
                    }
                    return;
                }
                resolve();
            });
        });
    }
    async _setCwdViaHttp(cwd) {
        const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/cwd`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ cwd }),
        });
        if (!response.ok) {
            if (response.status === 404) {
                throw new errors_1.SandboxNotFoundError('Session not found');
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
    async startBackground(command, options = {}) {
        if (this._destroyed) {
            throw new errors_1.SandboxNotFoundError('Sandbox has been destroyed');
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
                throw new errors_1.SandboxNotFoundError('Session not found');
            }
            const text = await response.text();
            throw new errors_1.CommandExecutionError(`Failed to start background process: ${response.status} - ${text}`);
        }
        const result = await response.json();
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
    async killBackground() {
        if (this._destroyed) {
            throw new errors_1.SandboxNotFoundError('Sandbox has been destroyed');
        }
        const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}/background`, {
            method: 'DELETE',
        });
        if (!response.ok) {
            if (response.status === 404) {
                throw new errors_1.SandboxNotFoundError('Session not found');
            }
            const text = await response.text();
            throw new errors_1.CommandExecutionError(`Failed to kill background processes: ${response.status} - ${text}`);
        }
        return await response.json();
    }
    /**
     * Destroy this sandbox session.
     *
     * After calling this method, the sandbox cannot be used anymore.
     */
    async destroy() {
        if (this._destroyed) {
            return;
        }
        try {
            const response = await fetch(`${this._httpBaseUrl}/sessions/${this.sessionId}`, {
                method: 'DELETE',
            });
            // Best effort cleanup - ignore errors
            void response;
        }
        catch {
            // Ignore errors during cleanup
        }
        finally {
            this._destroyed = true;
        }
    }
}
exports.Sandbox = Sandbox;
//# sourceMappingURL=sandbox.js.map