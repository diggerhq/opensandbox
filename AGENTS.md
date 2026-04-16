# OpenComputer

Cloud sandboxes for running AI agents. Persistent VMs with checkpoints,
hibernation, elasticity, and preview URLs.

**Naming:** The product is **OpenComputer**, but the Go module is
`github.com/opensandbox/opensandbox`, binaries are `opensandbox-server` /
`opensandbox-worker` / `osb-agent`, env vars are `OPENSANDBOX_*`, and
API key prefixes are `osb_`. Don't be confused by the mismatch — it's
historical. Use "OpenComputer" in docs and UI, `opensandbox` in code.

## Hard rules

**NEVER force push.** `git push --force`, `git push -f`, and
`git push --force-with-lease` are forbidden. No exceptions, all branches.
Make a new commit instead.

## Architecture

Three-tier distributed system: control plane, data plane, in-VM agent.

```
Client → Control Plane (server) → Data Plane (worker) → VM Agent
            REST API                  gRPC                gRPC over
            PostgreSQL                QEMU/Firecracker    vsock/virtio-serial
            billing, auth             sandbox lifecycle
            orchestration             checkpoints
```

| Tier | Binary | Owns | Key files |
|------|--------|------|-----------|
| Control plane | `cmd/server` | API, routing, orchestration, billing, autoscaling | `internal/api/router.go`, `internal/controlplane/` |
| Data plane | `cmd/worker` | VM lifecycle, snapshots, hibernation, storage | `internal/qemu/manager.go`, `internal/worker/` |
| In-VM agent | `cmd/agent` | Exec, files, PTY, stats inside the sandbox | `internal/agent/`, `proto/agent/agent.proto` |
| CLI | `cmd/oc` | User-facing commands | `cmd/oc/internal/commands/` |

Communication between tiers:
- Client ↔ Server: HTTP/REST + WebSocket (port 8080)
- Server ↔ Worker: gRPC (port 9090)
- Worker ↔ Agent: gRPC over vsock (Firecracker) or virtio-serial (QEMU)

## Source layout

| Path | What it is |
|------|-----------|
| `cmd/server/` | Control plane entry point |
| `cmd/worker/` | Data plane entry point |
| `cmd/agent/` | In-VM agent entry point |
| `cmd/oc/` | CLI tool (Cobra framework) |
| `internal/api/` | REST API handlers — sandbox, exec, files, PTY, checkpoints, auth |
| `internal/auth/` | JWT, WorkOS OAuth, API key validation, middleware |
| `internal/sandbox/` | Sandbox state machine, routing |
| `internal/qemu/` | QEMU VM manager — snapshots, hibernation, migration (large: ~108KB) |
| `internal/compute/` | Cloud provider pools (EC2, Azure) |
| `internal/controlplane/` | Server scaling, worker registry (Redis), gRPC leader |
| `internal/db/` | PostgreSQL schema, migrations (23 pairs in `migrations/`) |
| `internal/billing/` | Stripe integration, usage tracking, scale events |
| `internal/proxy/` | Subdomain routing for preview URLs |
| `internal/secretsproxy/` | MITM proxy — substitutes secret placeholders in outbound HTTPS |
| `internal/config/` | Environment-based configuration (`config.go` is the reference) |
| `internal/analytics/` | Segment event tracking |
| `internal/grpctls/` | TLS helpers for gRPC connections |
| `proto/` | Protocol Buffer definitions — agent and worker gRPC services |
| `sdks/typescript/` | `@opencomputer/sdk` npm package |
| `sdks/python/` | `opencomputer-sdk` PyPI package |
| `web/` | React + Vite dashboard (TanStack Query, xterm.js) |
| `docs/` | Mintlify documentation site |
| `deploy/` | Dockerfiles, Terraform, cloud deployment scripts |
| `scripts/` | Integration tests, QEMU tests, benchmarks |
| `examples/` | SDK usage examples |
| `archive/` | Dead code (microvm-tests, process-level). Ignore. |

## Dev loop

**Language:** Go 1.24 (CGO for server/worker). Static binaries for agent.

**Prerequisites:** Go 1.24+, Docker (for infra), PostgreSQL + NATS (via compose).

```bash
# Start local infra (Postgres + NATS)
make infra-up

# Seed test org + API key
make seed

# Run combined server+worker (no auth, simplest path)
make run-dev

# Run with PostgreSQL
make run-pg

# Run with full auth (WorkOS) + Vite dashboard
make run-pg-workos

# Build everything
make build

# Build + install CLI
make install-oc

# Run tests
make test          # all tests
make test-unit     # unit only

# Lint + format
make fmt           # gofmt
make lint          # golangci-lint (if installed)
make tidy          # go mod tidy

# Web dashboard dev
make web-dev       # Vite dev server, proxies to :8080
make web-build     # production build
```

Note: `make run-dev` and other run targets build the server binary first
(CGO_ENABLED=1, so you need a C compiler / Xcode CLT on macOS).

Three dev tiers, pick the simplest one that covers your change:
1. **Tier 1** (`make run-dev`): combined mode, no DB, in-memory only
2. **Tier 2** (`make run-pg`): combined mode, real PostgreSQL
3. **Tier 3** (`make run-full-server` + `make run-full-worker`): distributed

## Key environment variables

Reference: `internal/config/config.go` and `deploy/*.env.example`.

| Var | What it is |
|-----|-----------|
| `OPENSANDBOX_MODE` | `combined`, `server`, or `worker` |
| `OPENSANDBOX_PORT` | HTTP port (default 8080) |
| `OPENSANDBOX_DATABASE_URL` | PostgreSQL connection string |
| `OPENSANDBOX_JWT_SECRET` | Signs sandbox-scoped JWTs |
| `OPENSANDBOX_API_KEY` | Static API key (dev/combined mode) |
| `OPENSANDBOX_REDIS_URL` | Worker registry (distributed mode) |
| `OPENSANDBOX_VM_BACKEND` | `qemu` or `firecracker` |
| `OPENSANDBOX_REGION` | Worker region identifier |
| `OPENSANDBOX_S3_BUCKET` | Checkpoint storage bucket |
| `OPENSANDBOX_SECRET_ENCRYPTION_KEY` | AES-256 key for secret store at rest |

## Architecture boundaries

**API handlers** (`internal/api/`) own HTTP concerns: request parsing,
response formatting, auth middleware. They call into domain packages
(`sandbox`, `qemu`, `compute`) for business logic. Don't put domain
logic in handlers.

**Sandbox state machine** (`internal/sandbox/`) owns state transitions.
Don't change sandbox state from API handlers directly — go through the
state machine.

**Proto definitions** (`proto/`) are the contract between tiers. Changes
to `.proto` files are contract changes — regenerate Go code (`make proto`)
and verify both sides.

**SDKs** (`sdks/`) are published packages with external consumers. Breaking
changes need version bumps and migration guidance. The TypeScript SDK is
the primary SDK — keep it working first.

**Docs** (`docs/`) are a Mintlify site. Navigation lives in `docs/mint.json`.
When adding a new page, add it to the `navigation` array or it won't appear
in the sidebar.

## CLI development

The `oc` CLI lives in `cmd/oc/`. Cobra framework, config at `~/.oc/config.yaml`.

```bash
make build-oc        # build
make install-oc      # build + install to $GOPATH/bin
```

The CLI has two kinds of commands:
- **Sandbox commands** (`sandbox`, `exec`, `shell`, `checkpoint`, `patch`,
  `preview`, `secret`) — talk directly to the OpenComputer API.
- **Agent commands** (`agent create/connect/install/get/delete`) — thin
  wrappers that talk to sessions-api (separate service). Require
  `SESSIONS_API_URL` or `--sessions-api-url` flag.

CLI releases are automated via `.github/workflows/release-cli.yml` on merge
to main. The release workflow builds cross-platform binaries.

## Deploying

**Server:** Docker image from `deploy/Dockerfile.server`. Deployed to cloud
VMs or container services.

**Worker:** Docker image from `deploy/Dockerfile.worker`. Complex build
(CRIU from source + crun + Podman + QEMU). Deployed to bare-metal or
large VMs with nested virtualization.

**Docs:** Mintlify auto-deploys from the `docs/` directory on the default
branch.

CI/CD workflows in `.github/workflows/`:
- `deploy-server.yml` — control plane
- `deploy-worker.yml` — data plane
- `build-worker-ami.yml` — Packer AMI for worker instances
- `publish-ts-sdk.yml` — npm package
- `publish-python-sdk.yml` — PyPI package
- `release-cli.yml` — CLI binaries

## Database

PostgreSQL with raw SQL migrations in `internal/db/migrations/` (23 pairs,
`.up.sql` / `.down.sql`). No ORM — direct queries via `pgx`.

Core tables: `orgs`, `users`, `api_keys`, `sandbox_sessions`, `workers`,
`checkpoints`, `templates`, `preview_urls`, `scale_events`.

Secrets are encrypted at rest with AES-256 (`internal/crypto/`), with key
rotation support.

## Testing

```bash
make test            # all tests (unit + integration)
make test-unit       # unit only
```

Integration tests in `scripts/integration-tests/` (TypeScript, run against
a live server). QEMU-specific tests in `scripts/qemu-tests/`. Benchmarks
in `scripts/bench-*.sh`.

## Managed agents context

The managed agent platform (`oc agent create/connect/install`) is built in
[sessions-api](https://github.com/diggerhq/sessions-api), a separate
service deployed on Fly. It uses the OpenComputer SDK to create sandboxes
from checkpoints and exec into them. The managed agent code does not live
in this repo — this repo is the sandbox infrastructure.

What lives here that managed agents depend on:
- **Checkpoint API** — `POST /api/sandboxes/:id/checkpoints`, restore, fork
- **Secret store** — `POST /api/secret-stores`, secrets injected at sandbox boot
- **Exec API** — `POST /api/sandboxes/:id/exec` (how the adapter configures cores)
- **SDK** (`sdks/typescript/`) — `@opencomputer/sdk` used by sessions-api
- **CLI** (`cmd/oc/`) — `oc agent` commands (thin wrapper over sessions-api)
- **Docs** (`docs/`) — managed agent guides and API reference
