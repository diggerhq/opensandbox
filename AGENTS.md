# OpenComputer

Cloud sandboxes for running AI agents. Persistent VMs with checkpoints,
hibernation, elasticity, preview URLs, SDKs, and a CLI.

**Naming:** the product is **OpenComputer**, but the Go module is
`github.com/opensandbox/opensandbox`, binaries are `opensandbox-server` /
`opensandbox-worker` / `osb-agent`, env vars are `OPENSANDBOX_*`, and API key
prefixes are `osb_`. This is historical. Use "OpenComputer" in docs and UI,
`opensandbox` in code.

## Purpose

This file is the **durable starting point for agents** working in this repo.
It should stay stable and high-level:

- what this repo owns
- what it does not own
- where to look first
- which boundaries and contracts matter
- hard rules that should not drift

It should **not** become a runbook. Exact commands, env vars, workflows,
deployment steps, and current rollout details should live in the real source of
truth they come from.

## Start here

- `README.md` — product overview and human quick start
- `Makefile` — local dev, build, test, and common run targets
- `internal/config/config.go` — environment variables and runtime config
- `proto/` — inter-tier contracts
- `docs/mint.json` — docs navigation
- `cmd/oc/` — CLI entrypoint
- `sdks/typescript/` and `sdks/python/` — published SDKs

Managed-agent product behavior is mostly **not** implemented here:

- `sessions-api` — managed-agent control plane and orchestration
- `ws-gstack` — design workspace for managed agents, channels, packages, and
  product shape

## Hard rules

**NEVER force push.** `git push --force`, `git push -f`, and
`git push --force-with-lease` are forbidden. No exceptions. Make a new commit
instead.

**NEVER push to main.** Always create a feature branch and open a pull request.
Direct pushes to `main` are not allowed in this repo.

**Branch naming.** Prefix branches by intent: `feat/<kebab-name>` for new
functionality, `fix/<kebab-name>` for bug fixes, `docs/<kebab-name>` for
docs-only changes. These are the dominant patterns on `origin` and should be
preferred for any new branch. Don't use personal-initials prefixes (`ig/...`,
etc.) — they make it harder for others to find in-flight work by topic.

**Confirm pushes, with one exception.** Pushing to a branch the agent itself
just created in this session does not require a fresh confirmation each time
once the initial push is authorized. Pushing to any branch that existed before
the session — or that anyone else may have touched — needs explicit
confirmation every time. When in doubt, confirm.

## Repo scope

This repo owns the **sandbox platform**:

- control plane API
- worker / VM lifecycle
- in-VM agent
- checkpoints, hibernation, preview URLs
- auth, billing, secrets, autoscaling
- SDKs, CLI, dashboard, and docs for the platform itself

This repo does **not** own the managed-agent product layer:

- managed cores, channels, and packages
- agent-specific orchestration flows
- sessions-api control plane behavior

Those depend on this repo's APIs, SDKs, CLI, and sandbox primitives, but the
product logic lives elsewhere.

## Durable architecture

OpenComputer is a three-tier system:

```text
Client -> Control Plane -> Data Plane -> In-VM Agent
          HTTP / REST       gRPC         gRPC over vsock or virtio-serial
```

- **Control plane** (`cmd/server`, `internal/api/`, `internal/controlplane/`)
  owns API, auth, routing, orchestration, billing, and worker coordination.
- **Data plane** (`cmd/worker`, `internal/worker/`, `internal/qemu/`,
  `internal/compute/`) owns sandbox lifecycle, snapshots, hibernation, and
  machine-level execution.
- **In-VM agent** (`cmd/agent`, `internal/agent/`, `proto/agent/`) owns exec,
  files, PTY, and process interaction inside the sandbox.
- **CLI** (`cmd/oc/`) is the user-facing shell over platform APIs.

The stable mental model is:

- the server owns global coordination
- the worker owns VM reality
- the in-VM agent owns inside-the-sandbox operations

## Source map

Use this as a routing map, not a full index:

- `cmd/server/`, `cmd/worker/`, `cmd/agent/`, `cmd/oc/` — entrypoints
- `internal/api/` — HTTP handlers and API composition
- `internal/auth/` — auth, API keys, WorkOS, middleware
- `internal/sandbox/` — sandbox state machine and routing
- `internal/qemu/` — VM manager and checkpoint/hibernation logic
- `internal/worker/` — worker-side orchestration and sandbox operations
- `internal/controlplane/` — control-plane coordination and scaling
- `internal/db/` — schema and migrations
- `internal/proxy/` — preview URL and subdomain routing
- `internal/secretsproxy/` — outbound secret substitution proxy
- `sdks/` — published SDKs
- `web/` — dashboard
- `docs/` — docs site
- `deploy/` — deployment assets
- `archive/` — old code; ignore unless you are explicitly archaeology-mode

## External contracts

Be careful when changing these. They have consumers outside the immediate code
you are editing:

- `proto/` — contracts between tiers
- public HTTP API routes in `internal/api/`
- `sdks/` — published TypeScript and Python SDKs
- `cmd/oc/` — CLI behavior users script against
- `docs/` — user-facing product and API documentation

If a change crosses one of these boundaries, treat it as a contract change, not
just a refactor.

## Architecture boundaries

- **HTTP handlers own HTTP concerns.** Request parsing, auth, and response
  formatting belong in `internal/api/`. Do not bury domain logic there.
- **Sandbox state transitions go through the sandbox state machine.** Do not
  mutate sandbox state ad hoc from handlers.
- **Proto files are real contracts.** If they change, regenerate code and
  think through both sides of the boundary.
- **SDKs and CLI are public surfaces.** Prefer preserving behavior over
  rearranging internals for neatness.
- **Docs navigation is explicit.** Adding a page without wiring it into
  `docs/mint.json` leaves it effectively invisible.

## CLI boundary

`cmd/oc/` spans two kinds of user flows:

- direct OpenComputer platform commands: sandboxes, exec, files, checkpoints,
  previews, secrets
- managed-agent commands, which delegate to `sessions-api`

Do not assume all `oc` behavior is implemented in this repo just because the
command lives here.

## Change strategy

When making changes, prefer the narrowest layer that can correctly own the work:

- docs/content issue -> `docs/`
- CLI UX issue -> `cmd/oc/`
- SDK issue -> `sdks/`
- API shape/auth issue -> `internal/api/` or `internal/auth/`
- sandbox lifecycle issue -> `internal/sandbox/`, `internal/worker/`,
  `internal/qemu/`
- managed-agent orchestration issue -> probably not this repo

## Maintenance rule for this file

Update `AGENTS.md` when the repo's **shape, ownership boundaries, hard rules,
or source-of-truth map** changes.

Do **not** update it for:

- current feature status
- exact commands or flags
- env var inventories
- workflow filenames
- deployment playbooks
- temporary rollout details

Those belong in the concrete files they come from.
