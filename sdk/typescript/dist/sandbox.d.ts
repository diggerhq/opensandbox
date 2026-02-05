/**
 * Sandbox session class for interacting with a running sandbox.
 */
import * as grpc from '@grpc/grpc-js';
import type { CommandResult, RunOptions, FileEntry, BackgroundRunOptions, BackgroundRunResult } from './types';
export interface SandboxServiceClient {
    runCommand(request: RunCommandRequest, callback: (error: grpc.ServiceError | null, response: RunCommandResponse) => void): void;
    writeFile(request: WriteFileRequest, callback: (error: grpc.ServiceError | null, response: WriteFileResponse) => void): void;
    writeFiles(request: GrpcWriteFilesRequest, callback: (error: grpc.ServiceError | null, response: GrpcWriteFilesResponse) => void): void;
    readFile(request: ReadFileRequest, callback: (error: grpc.ServiceError | null, response: ReadFileResponse) => void): void;
    setEnv(request: SetEnvRequest, callback: (error: grpc.ServiceError | null, response: SetEnvResponse) => void): void;
    setCwd(request: SetCwdRequest, callback: (error: grpc.ServiceError | null, response: SetCwdResponse) => void): void;
}
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
export declare class Sandbox {
    /** Unique session identifier */
    readonly sessionId: string;
    /** Preview URL for accessing web servers in the sandbox */
    readonly previewUrl: string | null;
    private _grpcStub;
    private _httpBaseUrl;
    private _destroyed;
    constructor(sessionId: string, grpcStub: SandboxServiceClient | null, httpBaseUrl: string, previewUrl?: string | null);
    /**
     * Execute a shell command in the sandbox.
     *
     * @param command - The shell command to execute
     * @param options - Execution options (timeout, memory, etc.)
     * @returns Command result with stdout, stderr, exitCode, and signal
     * @throws {SandboxNotFoundError} If the session no longer exists
     * @throws {CommandExecutionError} If there was an error executing the command
     */
    run(command: string, options?: RunOptions): Promise<CommandResult>;
    private _runViaGrpc;
    private _runViaHttp;
    /**
     * Write content to a file in the sandbox.
     *
     * @param path - Absolute path in the sandbox filesystem
     * @param content - File content (string or Buffer)
     * @throws {SandboxNotFoundError} If the session no longer exists
     * @throws {FileOperationError} If the file cannot be written
     */
    writeFile(path: string, content: string | Buffer): Promise<void>;
    private _writeFileViaGrpc;
    private _writeFileViaHttp;
    /**
     * Write multiple files to the sandbox in a single request.
     *
     * @param files - Array of files with path and content
     * @throws {SandboxNotFoundError} If the session no longer exists
     * @throws {FileOperationError} If any file cannot be written (partial success possible)
     */
    writeFiles(files: Array<{
        path: string;
        content: string | Buffer;
    }>): Promise<void>;
    private _writeFilesViaGrpc;
    private _writeFilesViaHttp;
    /**
     * Read a file from the sandbox.
     *
     * @param path - Absolute path in the sandbox filesystem
     * @returns File contents as a Buffer
     * @throws {SandboxNotFoundError} If the session no longer exists
     * @throws {FileOperationError} If the file cannot be read
     */
    readFile(path: string): Promise<Buffer>;
    private _readFileViaGrpc;
    private _readFileViaHttp;
    /**
     * Read a text file from the sandbox.
     *
     * @param path - Absolute path in the sandbox filesystem
     * @param encoding - Text encoding (default: utf-8)
     * @returns File contents as a string
     */
    readFileText(path: string, encoding?: BufferEncoding): Promise<string>;
    /**
     * List files in a directory within the sandbox.
     *
     * @param path - Directory path in the sandbox filesystem
     * @returns Array of file entries
     * @throws {SandboxNotFoundError} If the session no longer exists
     * @throws {FileOperationError} If the directory cannot be listed
     */
    listFiles(path: string): Promise<FileEntry[]>;
    /**
     * Set environment variables for subsequent commands.
     *
     * @param env - Dictionary of environment variables to set
     * @throws {SandboxNotFoundError} If the session no longer exists
     */
    setEnv(env: Record<string, string>): Promise<void>;
    private _setEnvViaGrpc;
    private _setEnvViaHttp;
    /**
     * Set the working directory for subsequent commands.
     *
     * @param cwd - The new working directory path
     * @throws {SandboxNotFoundError} If the session no longer exists
     */
    setCwd(cwd: string): Promise<void>;
    private _setCwdViaGrpc;
    private _setCwdViaHttp;
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
    startBackground(command: string, options?: BackgroundRunOptions): Promise<BackgroundRunResult>;
    /**
     * Kill all background processes in this sandbox.
     *
     * Use this before starting a new background process to avoid port conflicts.
     */
    killBackground(): Promise<{
        killed: number[];
        total: number;
    }>;
    /**
     * Destroy this sandbox session.
     *
     * After calling this method, the sandbox cannot be used anymore.
     */
    destroy(): Promise<void>;
}
export {};
//# sourceMappingURL=sandbox.d.ts.map