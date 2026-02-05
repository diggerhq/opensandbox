/**
 * Custom error classes for the OpenSandbox SDK.
 */

/**
 * Base error class for OpenSandbox errors.
 */
export class OpenSandboxError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'OpenSandboxError';
  }
}

/**
 * Error thrown when a sandbox session is not found.
 */
export class SandboxNotFoundError extends OpenSandboxError {
  constructor(message = 'Sandbox session not found') {
    super(message);
    this.name = 'SandboxNotFoundError';
  }
}

/**
 * Error thrown when connection to the sandbox server fails.
 */
export class ConnectionError extends OpenSandboxError {
  constructor(message: string) {
    super(message);
    this.name = 'ConnectionError';
  }
}

/**
 * Error thrown when a command execution fails.
 */
export class CommandExecutionError extends OpenSandboxError {
  /** Exit code of the failed command */
  exitCode: number;
  /** Standard output from the command */
  stdout: string;
  /** Standard error from the command */
  stderr: string;

  constructor(message: string, exitCode = 1, stdout = '', stderr = '') {
    super(message);
    this.name = 'CommandExecutionError';
    this.exitCode = exitCode;
    this.stdout = stdout;
    this.stderr = stderr;
  }
}

/**
 * Error thrown when a file operation fails.
 */
export class FileOperationError extends OpenSandboxError {
  constructor(message: string) {
    super(message);
    this.name = 'FileOperationError';
  }
}
