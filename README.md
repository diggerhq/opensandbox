# OpenSandbox

Podman-based sandbox platform with E2B-compatible APIs for secure, isolated code execution.

## Quick Start

### Prerequisites

- Go 1.22+
- [Podman](https://podman.io/) installed and configured (rootless)

### Build & Run

```bash
# Build
make build

# Run in combined mode (server + worker in one process)
make run-dev

# Server starts on http://localhost:8080
```

### Create a Sandbox

```bash
# Create a sandbox
curl -X POST http://localhost:8080/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"templateID": "base", "timeout": 300}'

# Run a command
curl -X POST http://localhost:8080/sandboxes/{id}/commands \
  -H "Content-Type: application/json" \
  -d '{"cmd": "echo hello world"}'

# Write a file
curl -X PUT "http://localhost:8080/sandboxes/{id}/files?path=/tmp/hello.txt" \
  -d "Hello from OpenSandbox!"

# Read a file
curl "http://localhost:8080/sandboxes/{id}/files?path=/tmp/hello.txt"

# Kill the sandbox
curl -X DELETE http://localhost:8080/sandboxes/{id}
```

### Python SDK

```bash
pip install opensandbox-sdk
```

```python
import asyncio
from opensandbox import Sandbox

async def main():
    async with await Sandbox.create(template="python") as sb:
        result = await sb.commands.run("python3 -c 'print(1+1)'")
        print(result.stdout)  # "2\n"

        await sb.files.write("/tmp/hello.py", "print('hello')")
        result = await sb.commands.run("python3 /tmp/hello.py")
        print(result.stdout)  # "hello\n"

asyncio.run(main())
```

### TypeScript SDK

```bash
npm install opensandbox
```

```typescript
import { Sandbox } from "opensandbox";

const sb = await Sandbox.create({ template: "node" });

const result = await sb.commands.run("node -e 'console.log(1+1)'");
console.log(result.stdout); // "2\n"

await sb.files.write("/tmp/hello.js", "console.log('hello')");
const r2 = await sb.commands.run("node /tmp/hello.js");
console.log(r2.stdout); // "hello\n"

await sb.kill();
```

## Architecture

```
Client SDKs (Python / TypeScript)
        │ REST API + WebSocket
        ▼
Control Plane (Go/Echo)
  ├── API Server
  ├── Template Registry
  ├── Sandbox Router
  └── Compute Pool
        ├── Local (development)
        └── EC2 (production — bare-metal Graviton)
        │ gRPC (internal)
        ▼
Sandbox Worker (Go)
  ├── Sandbox Manager
  ├── Filesystem Service
  └── PTY Manager
        ▼ Podman (rootless, crun)
  [sandbox containers]
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| POST | `/sandboxes` | Create sandbox |
| GET | `/sandboxes` | List sandboxes |
| GET | `/sandboxes/:id` | Get sandbox info |
| DELETE | `/sandboxes/:id` | Kill sandbox |
| POST | `/sandboxes/:id/timeout` | Update timeout |
| POST | `/sandboxes/:id/commands` | Run command |
| GET | `/sandboxes/:id/files?path=` | Read file |
| PUT | `/sandboxes/:id/files?path=` | Write file |
| GET | `/sandboxes/:id/files/list?path=` | List directory |
| POST | `/sandboxes/:id/files/mkdir?path=` | Create directory |
| DELETE | `/sandboxes/:id/files?path=` | Remove file/dir |
| POST | `/sandboxes/:id/pty` | Create PTY session |
| WS | `/sandboxes/:id/pty/:sessionID` | PTY WebSocket |
| DELETE | `/sandboxes/:id/pty/:sessionID` | Kill PTY |
| POST | `/templates` | Build template |
| GET | `/templates` | List templates |
| GET | `/templates/:name` | Get template |
| DELETE | `/templates/:name` | Delete template |

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `OPENSANDBOX_PORT` | `8080` | Server port |
| `OPENSANDBOX_API_KEY` | (empty) | API key (empty = no auth) |
| `OPENSANDBOX_WORKER_ADDR` | `localhost:9090` | Worker address |
| `OPENSANDBOX_MODE` | `combined` | `server`, `worker`, or `combined` |

## Security

Every sandbox container runs with:
- **Rootless Podman** — no root privileges
- `--cap-drop=ALL` — all capabilities dropped
- `--read-only` — read-only root filesystem
- `--security-opt=no-new-privileges`
- `--userns=auto` — user namespace isolation
- `--pids-limit=256` — fork bomb protection
- `--memory=512m` — memory limit
- `--cpus=1` — CPU limit
- `--network=none` — network isolation (opt-in)
- Default Podman seccomp profile

## License

MIT
