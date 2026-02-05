/**
 * Custom error classes for the OpenSandbox SDK.
 */
/**
 * Base error class for OpenSandbox errors.
 */
export declare class OpenSandboxError extends Error {
    constructor(message: string);
}
/**
 * Error thrown when a sandbox session is not found.
 */
export declare class SandboxNotFoundError extends OpenSandboxError {
    constructor(message?: string);
}
/**
 * Error thrown when connection to the sandbox server fails.
 */
export declare class ConnectionError extends OpenSandboxError {
    constructor(message: string);
}
/**
 * Error thrown when a command execution fails.
 */
export declare class CommandExecutionError extends OpenSandboxError {
    /** Exit code of the failed command */
    exitCode: number;
    /** Standard output from the command */
    stdout: string;
    /** Standard error from the command */
    stderr: string;
    constructor(message: string, exitCode?: number, stdout?: string, stderr?: string);
}
/**
 * Error thrown when a file operation fails.
 */
export declare class FileOperationError extends OpenSandboxError {
    constructor(message: string);
}
//# sourceMappingURL=errors.d.ts.map