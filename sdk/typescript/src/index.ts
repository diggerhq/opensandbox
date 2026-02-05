/**
 * OpenSandbox TypeScript SDK
 *
 * A TypeScript SDK for interacting with OpenSandbox - secure code execution environments.
 *
 * @example
 * ```typescript
 * import { OpenSandbox } from '@opensandbox/sdk';
 *
 * const client = new OpenSandbox('http://localhost:8080');
 * const sandbox = await client.create();
 *
 * // Run commands
 * const result = await sandbox.run('echo "Hello, World!"');
 * console.log(result.stdout); // "Hello, World!\n"
 *
 * // File operations
 * await sandbox.writeFile('/home/test.txt', 'Hello from SDK!');
 * const content = await sandbox.readFileText('/home/test.txt');
 * console.log(content); // "Hello from SDK!"
 *
 * // List files
 * const files = await sandbox.listFiles('/home');
 * console.log(files);
 *
 * // Cleanup
 * await sandbox.destroy();
 * await client.close();
 * ```
 */

// Main exports
export { OpenSandbox } from './client';
export { Sandbox } from './sandbox';

// Type exports
export type {
  CommandResult,
  RunOptions,
  CreateOptions,
  ClientOptions,
  FileEntry,
  SessionInfo,
  CreateSessionResponse,
  BackgroundRunOptions,
  BackgroundRunResult,
} from './types';

// Error exports
export {
  OpenSandboxError,
  SandboxNotFoundError,
  ConnectionError,
  CommandExecutionError,
  FileOperationError,
} from './errors';
