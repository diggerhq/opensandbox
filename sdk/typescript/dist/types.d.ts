/**
 * TypeScript type definitions for OpenSandbox SDK.
 */
/**
 * Result of a command execution in the sandbox.
 */
export interface CommandResult {
    /** Standard output from the command */
    stdout: string;
    /** Standard error from the command */
    stderr: string;
    /** Exit code (0 for success) */
    exitCode: number;
    /** Signal number if process was killed by a signal */
    signal: number;
}
/**
 * Options for running a command in the sandbox.
 */
export interface RunOptions {
    /** CPU time limit in milliseconds (default: 300000) */
    timeoutMs?: number;
    /** Memory limit in KB (default: 2097152) */
    memKb?: number;
    /** Maximum file size in KB (default: 1048576) */
    fsizeKb?: number;
    /** Maximum number of open files (default: 256) */
    nofile?: number;
    /** Additional environment variables */
    env?: Record<string, string>;
    /** Working directory for the command */
    cwd?: string;
}
/**
 * Options for creating a new sandbox session.
 */
export interface CreateOptions {
    /** Initial environment variables */
    env?: Record<string, string>;
    /** Session timeout in seconds (default: 300) */
    timeout?: number;
}
/**
 * Options for the OpenSandbox client.
 */
export interface ClientOptions {
    /** gRPC port (default: 50051) */
    grpcPort?: number;
    /** Use insecure gRPC connection even for HTTPS base URL */
    grpcInsecure?: boolean;
    /** HTTP request timeout in milliseconds (default: 30000) */
    timeout?: number;
}
/**
 * File entry from directory listing.
 */
export interface FileEntry {
    /** File or directory name */
    name: string;
    /** Full path within the sandbox */
    path: string;
    /** Whether this entry is a directory */
    isDirectory: boolean;
    /** File size in bytes (0 for directories) */
    size: number;
}
/**
 * Session information returned by the server.
 */
export interface SessionInfo {
    /** Unique session ID */
    id: string;
    /** Environment variables */
    env: Record<string, string>;
    /** Current working directory */
    cwd: string;
    /** Session age in seconds */
    ageSecs: number;
    /** Idle time in seconds */
    idleSecs: number;
    /** Preview URL for web servers (if configured) */
    previewUrl: string | null;
    /** Exposed ports */
    ports: number[];
    /** Session status */
    status: 'running' | 'idle' | 'terminating';
}
/**
 * Options for starting a background process (e.g., a dev server).
 */
export interface BackgroundRunOptions {
    /** Port the process will listen on (default: 5173) */
    port?: number;
    /** Additional environment variables */
    env?: Record<string, string>;
    /** Working directory for the command */
    cwd?: string;
}
/**
 * Result of starting a background process.
 */
export interface BackgroundRunResult {
    /** PID of the background process */
    pid: number;
    /** Port the process is listening on */
    port: number;
    /** Preview URL to access the web server */
    previewUrl: string | null;
}
/**
 * Response from creating a session.
 */
export interface CreateSessionResponse {
    /** Unique session ID */
    sessionId: string;
    /** Preview URL for web servers (if configured) */
    previewUrl: string | null;
}
//# sourceMappingURL=types.d.ts.map