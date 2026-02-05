"use strict";
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
Object.defineProperty(exports, "__esModule", { value: true });
exports.FileOperationError = exports.CommandExecutionError = exports.ConnectionError = exports.SandboxNotFoundError = exports.OpenSandboxError = exports.Sandbox = exports.OpenSandbox = void 0;
// Main exports
var client_1 = require("./client");
Object.defineProperty(exports, "OpenSandbox", { enumerable: true, get: function () { return client_1.OpenSandbox; } });
var sandbox_1 = require("./sandbox");
Object.defineProperty(exports, "Sandbox", { enumerable: true, get: function () { return sandbox_1.Sandbox; } });
// Error exports
var errors_1 = require("./errors");
Object.defineProperty(exports, "OpenSandboxError", { enumerable: true, get: function () { return errors_1.OpenSandboxError; } });
Object.defineProperty(exports, "SandboxNotFoundError", { enumerable: true, get: function () { return errors_1.SandboxNotFoundError; } });
Object.defineProperty(exports, "ConnectionError", { enumerable: true, get: function () { return errors_1.ConnectionError; } });
Object.defineProperty(exports, "CommandExecutionError", { enumerable: true, get: function () { return errors_1.CommandExecutionError; } });
Object.defineProperty(exports, "FileOperationError", { enumerable: true, get: function () { return errors_1.FileOperationError; } });
//# sourceMappingURL=index.js.map