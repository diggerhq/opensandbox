# @opencomputer/sdk

TypeScript SDK for [OpenComputer](https://github.com/diggerhq/opensandbox) — cloud sandbox platform.

## Install

```bash
npm install @opencomputer/sdk
```

## Quick Start

```typescript
import { Sandbox } from "@opencomputer/sdk";

const sandbox = await Sandbox.create({ template: "base" });

// Execute commands
const result = await sandbox.commands.run("echo hello");
console.log(result.stdout); // "hello\n"

// Read and write files
await sandbox.files.write("/tmp/test.txt", "Hello, world!");
const content = await sandbox.files.read("/tmp/test.txt");

// Clean up
await sandbox.kill();
```

## Configuration

| Option    | Env Variable            | Default                  |
|-----------|------------------------|--------------------------|
| `apiUrl`  | `OPENSANDBOX_API_URL`  | `https://app.opencomputer.dev`  |
| `apiKey`  | `OPENSANDBOX_API_KEY`  | (none)                   |

## License

MIT
