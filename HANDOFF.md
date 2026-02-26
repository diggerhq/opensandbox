# OpenSandbox — Implementation Handoff

## What Was Built

OpenSandbox was evolved from a single-process Go server into a globally distributed sandbox platform. The architecture is inspired by Cloudflare Durable Objects: the control plane is removed from the data path. SDKs connect directly to workers after initial sandbox creation.

```
CREATION (one-time):
  SDK ──POST /sandboxes──> Control Plane ──gRPC──> Worker
  SDK <── { sandboxID, connectURL, token }

ALL SUBSEQUENT OPS (direct):
  SDK ──HTTP/WS──> Worker (no control plane involved)

ASYNC SYNC (background):
  Worker ──NATS──> Consumer ──batch insert──> PostgreSQL
```

---

## What's Done

### Phase 1: Database + Auth + Session Tracking

| File | What it does |
|------|-------------|
| `internal/db/store.go` | pgx-based data access layer — 15+ methods for orgs, users, API keys, sessions, workers |
| `internal/db/migrations/001_initial.up.sql` | Full PG schema: orgs, users, api_keys, sandbox_sessions, command_logs, pty_session_logs, workers, templates |
| `internal/auth/jwt.go` | Sandbox-scoped JWT issuing/validation (HS256). Claims: org_id, sandbox_id, worker_id |
| `internal/auth/middleware.go` | `PGAPIKeyMiddleware` (API keys vs PG, static fallback) + `SandboxJWTMiddleware` (Bearer token on workers) |
| `internal/sandbox/sqlite.go` | Per-sandbox SQLite with WAL mode — command_log, pty_sessions, events (with sync flag for NATS) |
| `internal/api/sandbox.go` | Org quota checks, SQLite init, JWT/connectURL (only in server mode), PG session records |
| `internal/api/commands.go` | SQLite command logging with timing |

### Phase 2: Worker HTTP Server + Direct SDK Access

| File | What it does |
|------|-------------|
| `internal/worker/http_server.go` | Echo HTTP server on worker — same REST API as control plane, but with JWT auth |
| `internal/worker/handlers.go` | All handlers: getSandbox, runCommand, readFile, writeFile, listDir, makeDir, removeFile, createPTY, ptyWebSocket, killPTY |
| `sdks/python/opensandbox/sandbox.py` | Uses `connectURL` + `token` from create response. `_data_client` for direct worker access |
| `sdks/typescript/src/sandbox.ts` | Same pattern. `connectUrl`/`token` fields, passed to Filesystem/Commands/Pty classes |
| `sdks/typescript/src/commands.ts`, `filesystem.ts`, `pty.ts` | Accept `token` param, use `Authorization: Bearer` when available |

### Phase 3: gRPC + NATS Sync

| File | What it does |
|------|-------------|
| `proto/worker/worker.pb.go`, `worker_grpc.pb.go` | Generated from `worker.proto` |
| `internal/worker/grpc_server.go` | gRPC server wrapping sandbox.Manager — CreateSandbox, DestroySandbox, GetSandbox, ExecCommand, ReadFile, WriteFile, etc. |
| `internal/worker/event_publisher.go` | NATS JetStream publisher — syncs SQLite events every 2s, heartbeats every 5s |
| `internal/db/sync_consumer.go` | NATS consumer on control plane side — writes worker events to PG in batches |

### Phase 4: Control Plane as Orchestrator

| File | What it does |
|------|-------------|
| `internal/controlplane/server.go` | Thin CP: create via gRPC, discover sandbox location, destroy, list sessions/workers |
| `internal/controlplane/worker_registry.go` | In-memory registry from NATS heartbeats. Stale detection (15s), region-based selection |
| `internal/controlplane/scaler.go` | Autoscaler: >70% util = scale up, <30% = scale down, 10s check interval |
| `internal/compute/pool.go` | Extended Pool interface: `SupportedRegions()`, `DrainMachine()`, Machine.HTTPAddr |
| `internal/compute/router.go` | `AssignInRegion()` for region-aware routing |

### Phase 5: Deploy + Observability

| File | What it does |
|------|-------------|
| `internal/metrics/metrics.go` | 14 Prometheus metrics (gauges, counters, histograms) + Echo middleware + standalone /metrics server |
| `deploy/docker-compose.yml` | PostgreSQL 16 + NATS 2 (JetStream) for local dev |
| `deploy/ec2/` | EC2 bare-metal deployment (setup, deploy, Caddy, systemd) |
| `deploy/prometheus/prometheus.yml` | Scrape configs for CP and workers |
| `deploy/grafana/dashboards/opensandbox-overview.json` | Dashboard: active sandboxes, create latency, utilization, sync lag |
| `deploy/Dockerfile.server`, `Dockerfile.worker` | Both use CGO_ENABLED=1 (required by go-sqlite3) |

### Phase 6: WorkOS (skeleton only)

| File | What it does |
|------|-------------|
| `internal/auth/workos.go` | Middleware structure, `ProvisionOrgAndUser()`, `GenerateAPIKey()`. **`validateSession()` is stubbed.** |

### Entry Points

| File | What it does |
|------|-------------|
| `cmd/server/main.go` | Wires up: Podman, sandbox/PTY managers, templates, PG + migrations, JWT, SQLite, NATS consumer |
| `cmd/worker/main.go` | Wires up: Podman, sandbox/PTY managers, SQLite, JWT, gRPC (:9090), HTTP (cfg.Port), NATS publisher, metrics (:9091), image pre-pulling |

---

## What's NOT Done

### 1. WorkOS Authentication (Phase 6)
`internal/auth/workos.go:97` — `validateSession()` returns `"not yet implemented"`.

**To finish:**
- Install WorkOS Go SDK
- Implement `validateSession()` to call WorkOS session validation API
- Add OAuth callback routes (`/auth/callback`, `/auth/login`, `/auth/logout`)
- Wire into `cmd/server/main.go`

### 2. Web Dashboard Frontend
No frontend exists. The plan calls for a web dashboard with:
- Session history viewer (data already available via `GET /sessions`)
- API key management (CRUD already in `store.go`)
- Org settings / billing (org table exists)
- SSO login via WorkOS

### 3. Prometheus Metrics Not Instrumented in Handlers
`internal/metrics/metrics.go` defines 14 metrics but they are not yet called from the actual API handlers. For example, `metrics.SandboxesActive.Inc()` is never called in `createSandbox()`.

**To finish:**
- Add `metrics.SandboxesActive.Inc()` / `.Dec()` in sandbox create/kill handlers
- Add `metrics.ExecDuration.Observe()` in command execution
- Add `metrics.SandboxCreateDuration.Observe()` in create flow
- Add `metrics.PTYSessionsActive` tracking in PTY handlers
- etc.

### 4. gRPC Streaming RPCs (intentionally deferred)
`internal/worker/grpc_server.go` — `ExecCommandStream` (line 190) and `PTYStream` (line 208) return "not implemented, use HTTP/WS directly". These are low priority since SDKs use HTTP/WebSocket for the data path.

### 5. Worker Heartbeat Stats
`cmd/worker/main.go` line ~108 — heartbeat reports hardcoded `Capacity: 50, Current: 0`. Should read actual sandbox count from the manager.

---

## How to Test Locally

### Prerequisites
- Go 1.23+
- Podman installed and running
- Docker (for infrastructure containers)

### Tier 1 — Simplest (no PG, no auth)
```bash
make run-dev
# In another terminal:
curl -s http://localhost:8080/health
curl -s -X POST http://localhost:8080/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"template":"base"}'
```

### Tier 2 — With PostgreSQL
```bash
make infra-up          # starts PG + NATS
make run-pg            # starts server in combined mode with PG
# In another terminal:
make seed              # creates test org + API key
curl -s -X POST http://localhost:8080/sandboxes \
  -H "Content-Type: application/json" \
  -H "X-API-Key: test-key" \
  -d '{"template":"base"}'
```

### Tier 3 — Separate Control Plane + Worker
```bash
make infra-up
# Terminal 1 — control plane on :8080
make run-full-server
# Terminal 2 — worker on :8081
make run-full-worker
# Terminal 3
make seed
# Create sandbox via CP → get connectURL pointing to worker
curl -s -X POST http://localhost:8080/sandboxes \
  -H "Content-Type: application/json" \
  -H "X-API-Key: test-key" \
  -d '{"template":"base"}'
# Response includes connectURL + token for direct worker access
```

### Stop Infrastructure
```bash
make infra-down        # stop containers
make infra-reset       # stop + delete volumes
```

---

## Key Design Decisions to Know

1. **connectURL/token only in server mode** — In combined mode (`make run-pg`), the SDK already talks to the right server, so no redirect. This was a bug fix: `internal/api/sandbox.go` gates on `s.mode == "server"`.

2. **CGO_ENABLED=1** — Required everywhere because of `go-sqlite3`. Both Dockerfiles and Makefile use it.

3. **Three auth contexts** — Dashboard (WorkOS, stubbed), SDK (API keys in PG), Worker direct access (sandbox-scoped JWT). All coexist.

4. **Fail, don't migrate** — When a worker dies, its sandboxes are marked as errored. No state migration attempted. Un-synced SQLite events are lost (acceptable — it's metadata).

5. **gRPC only for creation** — CP → Worker uses gRPC only for create/destroy. All data-path operations (commands, files, PTY) go directly from SDK to worker over HTTP/WS.

---

## File Map

```
cmd/
  server/main.go              — Control plane / combined mode entry point
  worker/main.go              — Worker entry point

internal/
  api/
    router.go                 — Echo server, routes, Server struct
    sandbox.go                — Create/get/kill/list/setTimeout/listSessions
    commands.go               — Execute commands (with SQLite logging)
    filesystem.go             — File read/write/list/mkdir/remove
    pty.go                    — WebSocket PTY
    template.go               — Template CRUD

  auth/
    jwt.go                    — Sandbox-scoped JWT (issue + validate)
    middleware.go             — PGAPIKeyMiddleware + SandboxJWTMiddleware
    workos.go                 — WorkOS skeleton (stubbed)

  controlplane/
    server.go                 — Thin CP server (create/discover/destroy)
    worker_registry.go        — NATS heartbeat → in-memory registry
    scaler.go                 — Autoscaler (Pool interface)

  db/
    store.go                  — pgx data access layer
    sync_consumer.go          — NATS → PG batch writer
    migrations/               — SQL up/down files

  worker/
    http_server.go            — Worker HTTP server (JWT-authed)
    handlers.go               — All worker handlers
    grpc_server.go            — gRPC server (wraps sandbox.Manager)
    event_publisher.go        — NATS publisher (events + heartbeats)

  sandbox/
    manager.go                — Container lifecycle (Podman)
    pty.go                    — PTY session management
    sqlite.go                 — Per-sandbox SQLite

  compute/
    pool.go                   — Pool interface
    ec2.go                    — AWS EC2 bare-metal pool
    local.go                  — Local/dev pool

  config/config.go            — All env var config
  metrics/metrics.go          — Prometheus metrics + Echo middleware

sdks/
  python/opensandbox/sandbox.py   — Uses connectURL for direct worker access
  typescript/src/
    sandbox.ts                — Uses connectURL for direct worker access
    commands.ts               — Bearer token support
    filesystem.ts             — Bearer token support
    pty.ts                    — Bearer token support

deploy/
  docker-compose.yml          — PG + NATS for local dev
  ec2/                        — EC2 bare-metal deployment
  Dockerfile.server           — CP Docker image
  Dockerfile.worker           — Worker Docker image
  prometheus/prometheus.yml   — Prometheus scrape config
  grafana/dashboards/         — Grafana dashboard JSON

proto/worker/
  worker.proto                — gRPC service definition
  worker.pb.go               — Generated protobuf
  worker_grpc.pb.go           — Generated gRPC stubs
```

---

## Suggested Next Steps (in priority order)

1. **Test Tier 2 end-to-end** — `make infra-up && make run-pg`, then `make seed`, then create/exec/kill a sandbox with curl. Verify PG records are written.
2. **Wire Prometheus metrics into handlers** — Low effort, high value. Add Inc/Dec/Observe calls in sandbox.go, commands.go, pty.go.
3. **Fix worker heartbeat stats** — Read actual sandbox count from manager instead of hardcoded 50/0.
4. **Implement WorkOS auth** — If you want a dashboard, this is the prerequisite.
5. **Build web dashboard** — React/Next.js app, session history, API key management.
6. **Test Tier 3 flow** — CP + Worker in separate processes, verify gRPC creation + direct SDK access.
