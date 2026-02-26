# opensandbox

TypeScript SDK for [OpenSandbox](https://github.com/diggerhq/opensandbox) — an open-source, E2B-compatible sandbox platform.

## Install

```bash
npm install opensandbox
```

## Quick Start

```typescript
import { Sandbox } from "opensandbox";

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
