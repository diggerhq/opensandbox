# OpenSandbox - Linux Sandbox with HTTP API

A Rust implementation of a Linux sandbox that runs commands in isolated environments with namespace separation, resource limits, and optional stateful sessions.

## Features

- **PID namespace isolation** - sandboxed processes can't see host processes
- **Mount namespace isolation** - sandboxed processes have their own filesystem view
- **Chroot jail** - processes are confined to a minimal filesystem
- **Resource limits** - CPU time, memory, file size, open files
- **Stateful sessions** - files and environment variables persist across requests
- **HTTP API** - easy integration with any language/tool

## Quick Start

```bash
# Build and run with Docker
docker compose up --build

# Test it
curl -X POST http://localhost:8080/run \
  -H "Content-Type: application/json" \
  -d '{"command": ["/bin/echo", "Hello from sandbox!"]}'
```

## API Endpoints

### Stateless Execution

**POST /run** - Run a command in a fresh sandbox (cleaned up after)

```bash
curl -X POST http://localhost:8080/run \
  -H "Content-Type: application/json" \
  -d '{
    "command": ["git", "--version"],
    "time": 5000,
    "mem": 2097152,
    "fsize": 10240,
    "nofile": 64
  }'
```

Response:
```json
{
  "stdout": "git version 2.39.2\n",
  "stderr": "",
  "exit_code": 0,
  "signal": null
}
```

### Stateful Sessions

Sessions preserve files and environment variables across multiple requests.

**POST /sessions** - Create a new session
```bash
curl -X POST http://localhost:8080/sessions \
  -H "Content-Type: application/json" \
  -d '{"env": {"MY_VAR": "hello"}}'
# Returns: {"session_id": "uuid..."}
```

**POST /sessions/:id/run** - Run command in session
```bash
# Write a file
curl -X POST http://localhost:8080/sessions/{id}/run \
  -H "Content-Type: application/json" \
  -d '{"command": ["/bin/sh", "-c", "echo hello > /tmp/test.txt"]}'

# Read it back (file persists!)
curl -X POST http://localhost:8080/sessions/{id}/run \
  -H "Content-Type: application/json" \
  -d '{"command": ["/bin/cat", "/tmp/test.txt"]}'
```

**POST /sessions/:id/env** - Set environment variables
```bash
curl -X POST http://localhost:8080/sessions/{id}/env \
  -H "Content-Type: application/json" \
  -d '{"env": {"GH_TOKEN": "..."}}'
```

**POST /sessions/:id/cwd** - Set working directory
```bash
curl -X POST http://localhost:8080/sessions/{id}/cwd \
  -H "Content-Type: application/json" \
  -d '{"cwd": "/tmp"}'
```

**GET /sessions** - List all sessions

**GET /sessions/:id** - Get session info

**DELETE /sessions/:id** - Delete session and cleanup

### Health Check

**GET /health** - Returns "OK"

## Configuration Options

| Parameter | Default | Description |
|-----------|---------|-------------|
| `time` | 5000 | CPU time limit in milliseconds |
| `mem` | 2097152 | Memory limit in KB (2GB default for Go programs) |
| `fsize` | 10240 | Max file size in KB |
| `nofile` | 64 | Max open files |
| `env` | {} | Environment variables |
| `cwd` | "/" | Working directory |

## CLI Mode

The binary also supports direct CLI execution:

```bash
sudo ./opensandbox --run --time 1000 --mem 262144 -- /bin/echo "hello"
```

## Available Tools

The sandbox includes:
- Standard Unix utilities (`/bin`, `/usr/bin`)
- `git`
- `gh` (GitHub CLI)

## Architecture

```
HTTP Request
    │
    ▼
┌─────────────────┐
│   Axum Server   │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ spawn_blocking  │  (tokio blocking task)
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  clone() with   │  CLONE_NEWPID | CLONE_NEWNS
│  namespaces     │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  Child Process  │
│  - chroot       │
│  - setrlimit    │
│  - execvpe      │
└─────────────────┘
```

## Security Notes

- Requires `--privileged` Docker flag for namespace operations
- Processes run as root inside sandbox (privilege dropping disabled due to multi-threading issues)
- Sandbox isolation via: PID namespace, mount namespace, chroot, resource limits
- No network namespace isolation (processes can access network)

## Building from Source

```bash
# Requires Linux
cargo build --release

# Run (requires root)
sudo ./target/release/opensandbox serve --port 8080
```

## Session Lifecycle

- Sessions auto-expire after 5 minutes of inactivity
- Expired sessions are cleaned up automatically
- Each session has its own sandbox directory at `/tmp/sandbox-{id}`

## Deploying to Fly.io

Fly.io runs apps in Firecracker VMs, which provides the necessary privileges for namespace operations.

1. Install the Fly CLI and login:
```bash
curl -L https://fly.io/install.sh | sh
fly auth login
```

2. Create `fly.toml` in the project root:
```toml
app = "opensandbox"
primary_region = "ord"

[build]
  dockerfile = "Dockerfile"

[http_service]
  internal_port = 8080
  force_https = true
  auto_stop_machines = false
  auto_start_machines = true
  min_machines_running = 1

[checks]
  [checks.health]
    type = "http"
    port = 8080
    path = "/health"
    interval = "10s"
    timeout = "2s"

[[vm]]
  memory = "2gb"
  cpu_kind = "shared"
  cpus = 2
```

3. Deploy:
```bash
fly launch
```

4. Allocate a public IP (if not automatically assigned):
```bash
fly ips allocate-v4 --shared
```

5. Test:
```bash
curl https://your-app-name.fly.dev/health
```

## Similar Projects & Inspiration

- [isolate](https://github.com/ioi/isolate) - Sandbox used by the International Olympiad in Informatics (IOI)
