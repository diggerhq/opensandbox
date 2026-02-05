"use strict";
/**
 * Custom error classes for the OpenSandbox SDK.
 */
Object.defineProperty(exports, "__esModule", { value: true });
exports.FileOperationError = exports.CommandExecutionError = exports.ConnectionError = exports.SandboxNotFoundError = exports.OpenSandboxError = void 0;
/**
 * Base error class for OpenSandbox errors.
 */
class OpenSandboxError extends Error {
    constructor(message) {
        super(message);
        this.name = 'OpenSandboxError';
    }
}
exports.OpenSandboxError = OpenSandboxError;
/**
 * Error thrown when a sandbox session is not found.
 */
class SandboxNotFoundError extends OpenSandboxError {
    constructor(message = 'Sandbox session not found') {
        super(message);
        this.name = 'SandboxNotFoundError';
    }
}
exports.SandboxNotFoundError = SandboxNotFoundError;
/**
 * Error thrown when connection to the sandbox server fails.
 */
class ConnectionError extends OpenSandboxError {
    constructor(message) {
        super(message);
        this.name = 'ConnectionError';
    }
}
exports.ConnectionError = ConnectionError;
/**
 * Error thrown when a command execution fails.
 */
class CommandExecutionError extends OpenSandboxError {
    /** Exit code of the failed command */
    exitCode;
    /** Standard output from the command */
    stdout;
    /** Standard error from the command */
    stderr;
    constructor(message, exitCode = 1, stdout = '', stderr = '') {
        super(message);
        this.name = 'CommandExecutionError';
        this.exitCode = exitCode;
        this.stdout = stdout;
        this.stderr = stderr;
    }
}
exports.CommandExecutionError = CommandExecutionError;
/**
 * Error thrown when a file operation fails.
 */
class FileOperationError extends OpenSandboxError {
    constructor(message) {
        super(message);
        this.name = 'FileOperationError';
    }
}
exports.FileOperationError = FileOperationError;
//# sourceMappingURL=errors.js.map