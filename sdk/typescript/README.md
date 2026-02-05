# OpenSandbox TypeScript SDK

TypeScript/JavaScript SDK for [OpenSandbox](https://github.com/diggerhq/opensandbox) - secure code execution environments.

## Installation

```bash
npm install @opensandbox/sdk
```

## Quick Start

```typescript
import { OpenSandbox } from '@opensandbox/sdk';

// Create client
const client = new OpenSandbox('http://localhost:8080');

// Create a sandbox session
const sandbox = await client.create();

// Run commands
const result = await sandbox.run('echo "Hello, World!"');
console.log(result.stdout); // "Hello, World!\n"
console.log(result.exitCode); // 0

// Clean up
await sandbox.destroy();
await client.close();
```

## Features

### Command Execution

```typescript
// Basic command
const result = await sandbox.run('ls -la');

// With options
const result = await sandbox.run('node script.js', {
  timeoutMs: 60000,      // 60 second timeout
  memKb: 512000,         // 512MB memory limit
  env: { NODE_ENV: 'production' },
  cwd: '/home/app',
});

// Check result
if (result.exitCode === 0) {
  console.log('Success:', result.stdout);
} else {
  console.error('Failed:', result.stderr);
}
```

### File Operations

```typescript
// Write a file
await sandbox.writeFile('/home/test.txt', 'Hello, World!');

// Write binary data
const buffer = Buffer.from([0x48, 0x65, 0x6c, 0x6c, 0x6f]);
await sandbox.writeFile('/home/binary.dat', buffer);

// Read a file as text
const text = await sandbox.readFileText('/home/test.txt');

// Read a file as binary
const data = await sandbox.readFile('/home/binary.dat');

// List directory contents
const files = await sandbox.listFiles('/home');
for (const file of files) {
  console.log(`${file.name} - ${file.isDirectory ? 'dir' : 'file'} - ${file.size} bytes`);
}
```

### Environment & Working Directory

```typescript
// Set environment variables
await sandbox.setEnv({
  API_KEY: 'secret',
  DEBUG: 'true',
});

// Set working directory
await sandbox.setCwd('/home/app');

// Now commands run with these settings
const result = await sandbox.run('pwd'); // /home/app
```

### Preview URLs

When the server is configured with a preview domain, sandboxes can expose web servers:

```typescript
const sandbox = await client.create();

// Start a web server
await sandbox.run('python3 -m http.server 5173 &');

// Access via preview URL
if (sandbox.previewUrl) {
  console.log(`Web server available at: ${sandbox.previewUrl}`);
  // e.g., https://abc-123.preview.opensandbox.fly.dev
}
```

## API Reference

### `OpenSandbox`

Main client for connecting to an OpenSandbox server.

```typescript
const client = new OpenSandbox(baseUrl, options?);
```

**Options:**
- `grpcPort?: number` - gRPC port (default: 50051)
- `grpcInsecure?: boolean` - Use insecure gRPC even for HTTPS
- `timeout?: number` - HTTP timeout in milliseconds (default: 30000)

**Methods:**
- `create(options?)` - Create a new sandbox session
- `close()` - Close all connections

### `Sandbox`

Represents a sandbox session.

**Properties:**
- `sessionId: string` - Unique session identifier
- `previewUrl: string | null` - Preview URL for web servers

**Methods:**
- `run(command, options?)` - Execute a shell command
- `writeFile(path, content)` - Write a file
- `readFile(path)` - Read a file as Buffer
- `readFileText(path, encoding?)` - Read a file as string
- `listFiles(path)` - List directory contents
- `setEnv(env)` - Set environment variables
- `setCwd(cwd)` - Set working directory
- `destroy()` - Destroy the sandbox

### Error Classes

- `OpenSandboxError` - Base error class
- `SandboxNotFoundError` - Session not found
- `ConnectionError` - Connection to server failed
- `CommandExecutionError` - Command execution failed
- `FileOperationError` - File operation failed

## Types

```typescript
interface CommandResult {
  stdout: string;
  stderr: string;
  exitCode: number;
  signal: number;
}

interface RunOptions {
  timeoutMs?: number;
  memKb?: number;
  fsizeKb?: number;
  nofile?: number;
  env?: Record<string, string>;
  cwd?: string;
}

interface FileEntry {
  name: string;
  path: string;
  isDirectory: boolean;
  size: number;
}
```

## License

MIT
