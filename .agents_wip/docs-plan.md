# OpenComputer Docs Rewrite Plan

## Guiding Principles

1. **Single sidebar, no tabs.** Every page lives in one navigation tree.
2. **Entity-first.** Pages organized around things the user encounters (sandboxes, agents, checkpoints, templates), not by SDK or abstract category. Each entity page is self-contained: what it is, how to use it, full API reference.
3. **Three-tab examples.** Every code example wraps in tabs: TypeScript / Python / HTTP API (where applicable). The user picks their preferred surface once and sees it everywhere. Some examples are SDK-only (no HTTP equivalent for streaming); some are HTTP-only (auth headers). Use judgement — include a tab only when it adds value.
4. **Quality over quantity.** If it can be said in fewer words, it should be. No filler sections. Every page earns its place.
5. **Entity → Example → Reference** flow on each page. Open with what the entity *is* (2-3 sentences), show a working code example, then provide the full API reference below.
6. **Code-forward.** The first thing on every entity page (after the short explanation) should be a working code example. Parameters and types come after.
7. **Reference section is exhaustive.** The Agents/Sandboxes pages teach with curated examples. The Reference pages document every endpoint, method, type, and parameter — the source of truth when the entity pages aren't enough.
8. **Honest about gaps.** Don't document features that don't exist yet. Mark experimental/beta features clearly.

---

## Current State Assessment

### What exists (30 .mdx files)
- Introduction + Quickstart (solid onboarding)
- 3 feature pages: Agents, Running Commands, Working with Files
- 8 TypeScript SDK pages (separate tab)
- 8 Python SDK pages (separate tab, mirrors TS)
- 7 CLI reference pages (separate tab)
- 2 guides (Lovable clone, Agent Skill)

### Key problems
1. **Tab separation creates duplication.** "Running Commands" exists as a feature page, a TS SDK page, a Python SDK page, and a CLI page. Four places for one concept.
2. **No conceptual foundation.** Docs jump straight to API calls without explaining what a sandbox *is*, its lifecycle, resource model, or how persistence works.
3. **Missing critical content.** No sandbox specs (OS, storage, network), no error reference, no troubleshooting, no architecture overview.
4. **SDK/code gaps.** `resume` in Agent sessions, `maxRunAfterDisconnect` in exec, hibernation semantics, preview URL domain verification — all in code but undocumented.
5. **Inconsistent API naming between SDKs.** `sandbox.exec` vs `sandbox.commands` (deprecated alias still used in Python quickstart examples).

---

## Proposed Structure

The two top-level entities are **Agents** (the primary use case — Claude running inside sandboxes) and **Sandboxes** (the compute primitive). Agents come first because that's why most users are here. Everything else — files, checkpoints, templates, etc. — are sub-entities scoped under their parent. Directory structure mirrors navigation groups.

```
docs/
├── mint.json
├── images/
│   ├── favicon.svg
│   ├── logo-light.svg
│   └── logo-dark.svg
│
│── introduction.mdx               ← REWRITE
│── quickstart.mdx                 ← REWRITE
│── how-it-works.mdx               ← NEW (architecture + key technical decisions)
│
├── agents/                         ← NEW directory
│   ├── overview.mdx               ← REWRITE (entity: what agents are, how they work)
│   ├── events.mdx                 ← NEW (understanding the event stream)
│   ├── tools.mdx                  ← NEW (configuring tools & MCP servers)
│   └── multi-turn.mdx             ← NEW (follow-ups, resume, session management)
│
├── sandboxes/                      ← NEW directory
│   ├── overview.mdx               ← NEW (entity: what sandboxes are + lifecycle + create/kill/hibernate)
│   ├── running-commands.mdx       ← REWRITE (merge SDK exec pages)
│   ├── working-with-files.mdx     ← REWRITE (merge SDK filesystem pages)
│   ├── interactive-terminals.mdx  ← NEW (promote from SDK-only)
│   ├── checkpoints.mdx            ← REWRITE (entity: what checkpoints are + API)
│   ├── templates.mdx              ← REWRITE (entity: what templates are + API)
│   ├── patches.mdx                ← REWRITE (entity: what patches are + API)
│   └── preview-urls.mdx           ← NEW (entity: what preview URLs are + API)
│
├── reference/                      ← NEW directory
│   ├── api.mdx                    ← NEW (HTTP API — every endpoint)
│   ├── typescript-sdk.mdx         ← NEW (every class, method, type)
│   ├── python-sdk.mdx             ← NEW (every class, method, type)
│   └── cli.mdx                    ← NEW (every CLI command, flag, subcommand)
│
├── cli/                            ← KEEP (guide-like pages in CLI nav group)
│   ├── overview.mdx               ← REWRITE (install, config, key workflows)
│   ├── sandbox.mdx                ← REWRITE (guide: sandbox management patterns)
│   ├── exec.mdx                   ← REWRITE from commands.mdx (guide: running commands)
│   ├── shell.mdx                  ← REWRITE (guide: interactive terminal tips)
│   ├── checkpoint.mdx             ← REWRITE from checkpoints.mdx (guide: checkpoint workflows)
│   ├── patch.mdx                  ← REWRITE from patches.mdx (guide: patch patterns)
│   └── preview.mdx                ← REWRITE from previews.mdx (guide: preview URL workflows)
│
├── guides/                         ← KEEP
│   ├── build-a-lovable-clone.mdx
│   └── agent-skill.mdx
│
│── troubleshooting.mdx            ← NEW
│
├── sdks/                           ← DELETE entire directory
│   ├── typescript/                  (content merged into entity pages)
│   └── python/                     (content merged into entity pages)
```

### mint.json Navigation

```json
{
  "tabs": [],
  "navigation": [
    {
      "group": "Getting Started",
      "pages": [
        "introduction",
        "quickstart",
        "how-it-works"
      ]
    },
    {
      "group": "Agents",
      "pages": [
        "agents/overview",
        "agents/events",
        "agents/tools",
        "agents/multi-turn"
      ]
    },
    {
      "group": "Sandboxes",
      "pages": [
        "sandboxes/overview",
        "sandboxes/running-commands",
        "sandboxes/working-with-files",
        "sandboxes/interactive-terminals",
        "sandboxes/checkpoints",
        "sandboxes/templates",
        "sandboxes/patches",
        "sandboxes/preview-urls"
      ]
    },
    {
      "group": "CLI",
      "pages": [
        "cli/overview",
        "cli/sandbox",
        "cli/exec",
        "cli/shell",
        "cli/checkpoint",
        "cli/patch",
        "cli/preview"
      ]
    },
    {
      "group": "Guides",
      "pages": [
        "guides/build-a-lovable-clone",
        "guides/agent-skill"
      ]
    },
    {
      "group": "Reference",
      "pages": [
        "reference/api",
        "reference/typescript-sdk",
        "reference/python-sdk",
        "reference/cli"
      ]
    },
    {
      "group": "Resources",
      "pages": [
        "troubleshooting"
      ]
    }
  ]
}
```

### Why this structure

- **Agents first.** This is the headline feature — most users land here to run Claude inside sandboxes. Putting Agents right after Getting Started matches the reader's intent.
- **Agents get depth.** Four pages mirror the Sandboxes pattern: an overview page explaining what agents are, then dedicated pages for events (the output you consume), tools (configuring what agents can do), and multi-turn (conversations that span sessions). Each page earns its place by covering a distinct concern.
- **Sandboxes group** contains the sandbox entity page plus everything you do with a sandbox (run commands, work with files, open terminals, create checkpoints, build templates, expose URLs).
- **CLI group** has guide-like pages (install, config, key workflows per topic). Each CLI page teaches patterns and concepts; the exhaustive flag-by-flag reference lives in `reference/cli.mdx`.
- **Reference is second-to-last.** It's lookup-oriented (every endpoint, every flag, every command) — most users read the entity/CLI pages first and only reach for Reference when they need exact signatures.
- **Directory = group.** `agents/`, `sandboxes/`, `cli/`, `guides/` — folder structure mirrors navigation. No orphan files at the root (except Getting Started and Resources).
- **No "Concepts" section.** Each entity page opens with what it is and why it exists, then shows the API.
- **No "Features" section.** The sidebar groups tell you what things *are*.

---

## Page-by-Page Specification

### Getting Started

#### `introduction.mdx` — REWRITE

**Goal:** Explain what OpenComputer is, who it's for, and why it exists. In 60 seconds a reader should know if this product is relevant to them.

**Structure:**
1. One-sentence tagline (cloud sandboxes for AI agents)
2. 3-4 sentence explanation: what a sandbox is, what makes it different from containers/serverless (persistent, full Linux VM, hibernates, checkpoints)
3. Feature cards (keep existing 4, tighten copy):
   - Claude Agent SDK built in
   - Long-running (hours/days, not minutes)
   - Checkpoint/fork like git branches
   - Full Linux VM (not a container)
4. Install (CodeGroup: npm + pip + CLI) + set `OPENCOMPUTER_API_KEY` env var
5. Minimal example: create sandbox → run agent → get result (TS + Python side by side)
6. Next steps cards → Quickstart, How it works, Agents, Sandboxes

**Changes from current:** Add install for CLI. Pin env var name (`OPENCOMPUTER_API_KEY`). Tighten copy. Add "Full Linux VM" card. Remove "Run agent tasks easily" card (redundant with Agent SDK card). Add how-it-works link.

#### `quickstart.mdx` — REWRITE

**Goal:** Working agent in 2 minutes. One example, zero detours.

**Structure:**
1. Prerequisites: API key + SDK installed (link to intro for install)
2. Set API key: `export OPENCOMPUTER_API_KEY=your-key`
3. **One example: Run an agent** — Create sandbox, `agent.start()` with a real task, stream events, print result. (TS + Python CodeGroup)
4. **What just happened** — 2-3 sentences: you created a cloud VM, Claude ran inside it, you got structured output back.
5. **Next steps** — cards linking to:
   - How it works (architecture)
   - Agents (deep dive on events, tools, multi-turn)
   - Sandboxes (the compute primitive, lifecycle, running commands)
   - Checkpoints (snapshot and fork)

**Changes from current:** Single focused example instead of three. Fix Python quickstart (`sandbox.commands.run` → agent example). Don't overload — link out to entity pages for depth.

#### `how-it-works.mdx` — NEW

**Goal:** Give technically curious users a clear mental model of the system in under 3 minutes. Cover architecture and the key technical decisions users would have opinions about.

**Structure:**
1. **One-paragraph overview** — Your code talks to the OpenComputer API. The API provisions a Firecracker microVM. Your agent (or commands) run inside that VM. Events stream back to your code.
2. **Firecracker microVMs** — Why not containers? Each sandbox is a real VM with its own kernel, memory, and disk. Hardware-level isolation (KVM). Boot in ~150ms. This is the same technology AWS Lambda uses under the hood.
3. **Hibernation** — The VM's full state (memory + disk) is snapshotted to S3. Resume is fast because the kernel doesn't reboot — it picks up exactly where it left off. Tradeoff: snapshot size scales with memory allocation.
4. **Checkpoints & forking** — Checkpoints are named snapshots. Forking creates a new VM from a checkpoint with copy-on-write semantics. This enables parallel exploration (try 5 approaches from the same starting point) and reproducibility.
5. **Templates** — Built from Dockerfiles but converted to VM root filesystems (not run as containers). The build process uses Docker to produce a filesystem image, then Firecracker boots from that image directly.
6. **Networking** — Each VM gets a TAP device with NAT. Full outbound internet. Inbound access via preview URLs (reverse-proxied through the host).
7. **Agent sessions** — Claude Agent SDK runs inside the VM as a regular process. The SDK spawns Claude, gives it bash + file tools, and manages the think→act→observe loop. Events are parsed from stdout and streamed to your code over WebSocket.

**Tone:** Concise, technical, opinionated. No marketing. A senior engineer should read this and think "these are reasonable choices, I understand the tradeoffs."

---

### Entity Pages

Each entity page follows this template:
1. **What is this** — 2-3 sentences explaining the entity for someone who's never seen it
2. **Primary code example** in tabs (TypeScript / Python / HTTP API where applicable)
3. **API reference** with `<ParamField>` for each method
4. **Additional examples** (also tabbed where applicable)
5. **Cross-links** — `<Tip>` at the bottom with: CLI equivalent (if one exists), links to Reference pages (TS SDK, Python SDK, HTTP API)

**Cross-linking pattern:** Entity pages link down to CLI guide + Reference. CLI guide pages link to the SDK entity page + `reference/cli.mdx`. This creates a navigable triangle: entity page ↔ CLI guide ↔ reference. A reader never hits a dead end.

### Agents (`agents/`)

#### `agents/overview.mdx` — REWRITE (entity page)

**Goal:** Explain what agent sessions are, how they work, and get the reader to a working agent in 60 seconds.

**Structure:**
1. **What is an agent session** — A Claude Agent SDK instance running inside a [sandbox](/sandboxes/overview) (a cloud VM with its own filesystem and shell). You send it a prompt, it works autonomously — writing files, running commands, iterating on errors — and streams events back as it goes.
2. **Quick example:** `sandbox.agent.start()` with event handling (TS + Python CodeGroup) — the simplest working agent
3. **How it works** — brief (3-4 sentences): the SDK spawns Claude inside the sandbox VM, Claude gets bash/file tools, it works in a loop (think → act → observe), events stream back to your code via WebSocket.
4. **`sandbox.agent.start(opts)`** — full param reference:
   - prompt, model, systemPrompt, allowedTools, permissionMode, maxTurns, cwd, mcpServers, onEvent, onError
   - **TS-only:** resume, onExit, onScrollbackEnd (Python SDK does not support these yet)
5. **AgentSession** — properties and methods table:
   - sessionId, done, sendPrompt, interrupt, configure, kill, close (both SDKs)
   - **Python-specific:** `collect_events()`, `wait()` (convenient alternatives to callbacks)
   - **TS-specific:** `done` returns Promise<number>; Python uses `await session.wait()` instead
6. **Quick links** to sub-pages: Events, Tools, Multi-turn

**Absorbs content from:** current `agents.mdx`, `sdks/typescript/agent.mdx` (overview parts), `sdks/python/agent.mdx` (overview parts).

#### `agents/events.mdx` — NEW

**Goal:** Complete reference for understanding what comes back from an agent session. A developer should be able to look at any event and know exactly what it means and how to handle it.

**Structure:**
1. Brief intro: agent sessions emit a stream of typed events via the `onEvent` callback. Each event has a `type` field.
2. **Event lifecycle** — typical order of events for a successful session:
   `ready → configured → assistant → tool_use_summary → assistant → ... → result → turn_complete`
3. **Event reference** — one subsection per event type, each with:
   - What it means
   - When it fires
   - Fields/payload
   - Code example showing how to handle it
   - Event types: `ready`, `configured`, `assistant`, `tool_use_summary`, `system`, `result`, `turn_complete`, `interrupted`, `error`
4. **Filtering events** — practical patterns: logging only assistant messages, capturing the final result, progress indicators
5. **Error handling** — `error` events vs `onError` (stderr) vs rejected promises. When each fires and how to respond.

#### `agents/tools.mdx` — NEW

**Goal:** Explain how to configure what tools an agent can use, including MCP servers for custom capabilities.

**Structure:**
1. **Default tools** — what the agent can do out of the box (bash, file read/write, Python). Brief explanation of `allowedTools` param.
2. **MCP servers** — the main event on this page. What MCP is (one sentence), then how to configure it:
   - `mcpServers` param structure: `Record<string, { command, args?, env? }>`
   - Example: SQLite database tool
   - Example: custom API tool
   - How MCP servers run (spawned inside the sandbox, agent discovers tools via protocol)
3. **`systemPrompt`** — how to steer agent behavior with custom instructions
4. **`permissionMode`** — what it controls, available values
5. **`maxTurns`** — limiting agent iterations

#### `agents/multi-turn.mdx` — NEW

**Goal:** Everything about conversations that go beyond a single prompt — follow-ups, resuming across sessions, and managing running sessions.

**Structure:**
1. **Follow-up prompts** — `session.sendPrompt(text)` to continue a conversation within the same session
   - Example: start a task, wait for completion, send follow-up
2. **Resuming across sessions** (TypeScript only) — `resume` parameter in `agent.start()`
   - How it works: capture `claude_session_id` from `turn_complete` event, pass it as `resume` in a new `agent.start()` call
   - Example: save session ID, create new sandbox from checkpoint, resume conversation
   - **Note:** Python SDK does not yet support `resume`. Show HTTP API workaround or mark as TS-only.
3. **Interrupting** — `session.interrupt()` to stop the current turn
4. **Reconfiguring mid-session** — `session.configure()` to change model, tools, etc.
5. **Managing sessions:**
   - `sandbox.agent.list()` — list active sessions
   - `sandbox.agent.attach(sessionId)` — reconnect to a running session (both SDKs)
   - When to use attach vs resume (attach = same session still running; resume = new session continuing old conversation — TS only)
6. **Note:** Agent sessions are SDK-only (no CLI command yet)

---

### Sandboxes (`sandboxes/`)

#### `sandboxes/overview.mdx` — NEW (entity page)

**Goal:** The definitive page for understanding and working with the sandbox primitive. Concept + lifecycle + full SDK API for sandbox management.

**Structure:**
1. **What is a sandbox** — A full Linux virtual machine in the cloud. Each sandbox is an isolated environment with its own filesystem, network, and processes. Think of it as a laptop that sleeps when idle and wakes instantly when you need it.
2. **Quick example:** create → run command → kill (TS + Python CodeGroup)
3. **Specs table:**
   - OS: Ubuntu-based Linux
   - Default CPU: 1 vCPU (configurable up to 4 via `cpuCount` — TS SDK + HTTP API; not yet in Python SDK)
   - Default memory: 512MB (configurable up to 2048MB via `memoryMB` — TS SDK + HTTP API; not yet in Python SDK)
   - Storage: 20GB workspace
   - Network: full outbound internet access
   - Pre-installed: Python 3, Node.js, common CLI tools
4. **Creating a sandbox** — `Sandbox.create(opts)` with full param reference:
   - Both SDKs: template, timeout, apiKey/api_key, apiUrl/api_url, envs, metadata
   - **TS-only:** cpuCount, memoryMB (Python SDK does not expose these yet; use HTTP API directly for custom resources)
5. **Connecting to an existing sandbox** — `Sandbox.connect(sandboxId)`
6. **Sandbox lifecycle:**
   - Status states: `running`, `hibernated`, `stopped`, `error`
   - Lifecycle diagram (text-based): running ↔ hibernated → stopped; running → error
   - Rolling timeout: resets on every operation, default 300s
   - What happens on timeout: auto-hibernate if possible, else stop
7. **Hibernation & wake** (TypeScript SDK only for now)
   - `sandbox.hibernate()`, `sandbox.wake()` — TS SDK methods
   - Like closing a laptop lid: memory + disk snapshotted, sandbox ID stays the same
   - Resume in seconds, no cost while hibernated
   - Auto-triggered on idle timeout
   - Difference from checkpoints: hibernation is transparent resume, checkpoints are named snapshots you fork from
   - **Note:** Python SDK does not yet have `hibernate()`/`wake()`. Use HTTP API directly: `POST /api/sandboxes/:id/hibernate`, `POST /api/sandboxes/:id/wake`. CLI: `oc sandbox hibernate`, `oc sandbox wake`.
8. **Other methods** — `kill()`, `isRunning()`/`is_running()`, `setTimeout()`/`set_timeout()`
9. **Sandbox properties** — sandboxId, agent, exec, files, pty
   - **Python-specific:** `async with Sandbox.create() as sandbox:` context manager (auto-kills on exit)

**Absorbs content from:** `sdks/typescript/sandbox.mdx`, `sdks/python/sandbox.mdx`.

#### `sandboxes/running-commands.mdx` — REWRITE

**Structure:**
1. Brief intro: two modes for running shell commands — `run()` (wait for result) and `start()` (streaming/async)
2. **Quick commands: `sandbox.exec.run()`** — CodeGroup TS + Python
   - Full param reference (command, timeout, env, cwd)
   - ProcessResult table
   - Examples: cwd, env vars, timeout, chaining
3. **Async commands: `sandbox.exec.start()`**
   - **TypeScript:** Full streaming — returns `ExecSession` with `onStdout`, `onStderr`, `onExit` callbacks, `sendStdin()`, `kill()`, `close()`. Supports `maxRunAfterDisconnect` (process continues N seconds after WS disconnect).
   - **Python:** Returns `str` (session ID only). No streaming callbacks, no ExecSession object. Use `exec.list()` and `exec.kill()` to manage sessions.
   - Show TS example with full streaming, Python example with `start()` → poll/kill pattern.
   - ExecSession table (TS only): sessionId, done, sendStdin, kill, close
4. **Managing sessions:**
   - `sandbox.exec.list()` — list running sessions (both SDKs)
   - `sandbox.exec.attach()` — reconnect to running session (TS only)
   - `sandbox.exec.kill()` — kill a session (both SDKs)
5. Examples: dev server (TS streaming), long-running process, reconnect pattern (TS only)

**Key improvement:** Use `sandbox.exec.*` consistently (not `sandbox.commands`). Be honest about Python SDK gaps in streaming — don't pretend parity exists.

#### `sandboxes/working-with-files.mdx` — REWRITE

**Structure:** Keep current structure — it's already good. Add CodeGroup for all examples.
1. Reading files (read, readBytes)
2. Writing files (write — text and binary)
3. Listing directories (list, EntryInfo)
4. Managing files (makeDir, remove, exists)
5. Examples: upload & run script, copy files

**Minimal changes needed.** Merge TS/Python into CodeGroups and ensure consistency.

#### `sandboxes/interactive-terminals.mdx` — NEW (promote from SDK-only)

**Structure:**
1. **What is a PTY session** — A full interactive terminal inside the sandbox, like SSH but over WebSocket. Supports colors, full-screen apps (vim, top).
2. Create a PTY session (TS + Python CodeGroup)
3. PtyOpts reference: cols (default 80), rows (default 24), onOutput callback
4. PtySession methods:
   - `send(data)` — both SDKs
   - `close()` — both SDKs
   - `recv()` — **Python-only** (returns bytes; alternative to onOutput callback)
   - **Note:** Neither SDK exposes `resize()`. HTTP API has `POST /sandboxes/:id/pty/:sessionID/resize` but no SDK wrapper yet.
5. Examples: run interactive commands, pipe stdin
6. CLI equivalent: `oc shell <sandbox-id>` (supports `--shell` flag)

#### `sandboxes/checkpoints.mdx` — REWRITE (entity page)

**Structure:**
1. **What is a checkpoint** — A named snapshot of a running sandbox's full state (memory, disk, processes). Create a checkpoint, then fork new sandboxes from it — each fork starts exactly where the checkpoint left off. Think of it like git commits for VMs.
2. Quick example: create checkpoint → fork → verify state (TS + Python CodeGroup)
3. **How checkpoints differ from hibernation:**
   - Hibernation pauses and resumes the *same* sandbox
   - Checkpoints create *new* sandboxes from a saved state
   - A sandbox can have many checkpoints; hibernation is a single pause/resume
4. **API reference:**
   - `sandbox.createCheckpoint(name)` — create
   - `sandbox.listCheckpoints()` — list
   - `Sandbox.createFromCheckpoint(id)` — fork a new sandbox
   - `sandbox.restoreCheckpoint(id)` — revert in-place
   - `sandbox.deleteCheckpoint(id)` — delete
5. CheckpointInfo structure:
   - TS SDK: `id`, `sandboxId`, `orgId`, `name`, `sandboxConfig`, `status`, `sizeBytes`, `createdAt`
   - Python SDK: returns raw dict from API
   - HTTP API: `id`, `sandboxId`, `orgId`, `name`, `status`, `sizeBytes`, `createdAt`
6. Status: `processing` → `ready` (or `failed`)
7. Examples: checkpoint before risky operation, fork for parallel exploration

#### `sandboxes/templates.mdx` — REWRITE (entity page)

**Structure:**
1. **What is a template** — A pre-built base image that sandboxes start from. The `default` template includes Ubuntu, Python, and Node.js. Build custom templates from Dockerfiles to skip setup time.
2. Quick example: build template → create sandbox from it (TS + Python CodeGroup)
3. **Default template** — what's pre-installed (derive from `Dockerfile.default`)
4. **API reference:**
   - **Note:** Templates is a standalone class, not a property on Sandbox. TS: `Templates` (plural), Python: `Template` (singular).
   - `templates.build(name, dockerfile)` — build from Dockerfile
   - `templates.list()` — list available
   - `templates.get(name)` — get details
   - `templates.delete(name)` — delete
   - Using in `Sandbox.create({ template: "my-template" })`
5. TemplateInfo structure: `templateID`/`template_id`, `name`, `tag`, `status`
   - Status values: `building`, `ready`, `error`
6. Example: template with specific language/framework pre-installed

#### `sandboxes/patches.mdx` — REWRITE (entity page)

**Structure:**
1. **What is a patch** — A shell script attached to a checkpoint that runs every time a sandbox is forked from that checkpoint. Use patches to inject configuration, update dependencies, or customize state at fork time without modifying the checkpoint itself.
2. Quick example: create patch on checkpoint (TS + Python CodeGroup)
3. **API reference:**
   - `Sandbox.createCheckpointPatch(checkpointId, { script, description })`
   - `Sandbox.listCheckpointPatches(checkpointId)`
   - `Sandbox.deleteCheckpointPatch(checkpointId, patchId)`
4. When patches run: applied on wake/boot from checkpoint (strategy is always `on_wake`)
5. Execution order: patches run in creation order
6. Failure handling: what happens if a patch script fails
7. Example: inject API keys, update packages at fork time

#### `sandboxes/preview-urls.mdx` — NEW (entity page)

**Structure:**
1. **What is a preview URL** — A public HTTPS URL that exposes a port inside your sandbox to the internet. Start a web server on port 3000 in your sandbox, create a preview URL, and anyone can access it.
2. Quick example: create preview URL (TS + Python CodeGroup)
3. **API reference:**
   - `sandbox.createPreviewURL({ port, domain?, authConfig? })`
   - `sandbox.listPreviewURLs()`
   - `sandbox.deletePreviewURL(port)`
4. **Custom domains** (requires Cloudflare integration on the deployment):
   - Pass `domain` param when creating preview URL
   - SSL is automatic via Cloudflare
   - Note: custom domain setup is org-level configuration (done via dashboard, not SDK)
5. Preview URLs persist across hibernation/wake cycles
6. Examples: share a dev server, multiple ports, custom domain

---

### Reference (`reference/`)

These are exhaustive, lookup-oriented pages. No tutorials, no "why" — just every endpoint/method/type/flag with parameters, return types, and a minimal example. The entity pages (Agents, Sandboxes) and CLI guide pages teach; these pages are the source of truth. Four pages: HTTP API, TypeScript SDK, Python SDK, CLI.

#### `reference/api.mdx` — NEW

**Goal:** Complete HTTP API reference. Every endpoint, request/response format, auth headers, status codes. A developer using curl or a non-SDK language should be able to build a full integration from this page alone.

**Structure:**
1. **Base URL & Authentication**
   - Base: `https://app.opencomputer.dev/api`
   - **Control plane auth:** `X-API-Key: <API_KEY>` header (or `api_key` query param). Used for all `/api/*` routes.
   - **Worker direct auth:** `Authorization: Bearer <JWT>` header (or `token` query param for WebSocket). The JWT is sandbox-scoped and returned in the create sandbox response.
   - Most SDK users only need the API key — the SDK handles JWT auth transparently.
   - All requests/responses are JSON (except file read which returns plain text)
2. **Sandbox Lifecycle**
   - `POST /api/sandboxes` — create (params: templateID, timeout, envs, metadata, cpuCount, memoryMB). Response includes: sandboxID, status, connectURL, token, cpuCount, memoryMB, startedAt, endAt.
   - `GET /api/sandboxes` — list all
   - `GET /api/sandboxes/:id` — get details
   - `DELETE /api/sandboxes/:id` — kill
   - `POST /api/sandboxes/:id/timeout` — set idle timeout
   - `POST /api/sandboxes/:id/hibernate` — hibernate
   - `POST /api/sandboxes/:id/wake` — wake
3. **Commands (Exec)**
   - `POST /api/sandboxes/:id/exec/run` — run command and wait
   - `POST /api/sandboxes/:id/exec` — create exec session
   - `GET /api/sandboxes/:id/exec` — list sessions
   - `GET /api/sandboxes/:id/exec/:sessionID` — WebSocket attach
   - `POST /api/sandboxes/:id/exec/:sessionID/kill` — kill session
4. **Agent Sessions**
   - `POST /api/sandboxes/:id/agent` — create agent session
   - `GET /api/sandboxes/:id/agent` — list agent sessions
   - `POST /api/sandboxes/:id/agent/:sid/prompt` — send follow-up
   - `POST /api/sandboxes/:id/agent/:sid/interrupt` — interrupt
   - `POST /api/sandboxes/:id/agent/:sid/kill` — kill
5. **Filesystem**
   - `GET /api/sandboxes/:id/files?path=...` — read file
   - `PUT /api/sandboxes/:id/files` — write file
   - `GET /api/sandboxes/:id/files/list?path=...` — list directory
   - `POST /api/sandboxes/:id/files/mkdir` — create directory
   - `DELETE /api/sandboxes/:id/files?path=...` — remove
6. **Checkpoints**
   - `POST /api/sandboxes/:id/checkpoints` — create
   - `GET /api/sandboxes/:id/checkpoints` — list
   - `POST /api/sandboxes/:id/checkpoints/:checkpointId/restore` — restore in-place
   - `POST /api/sandboxes/from-checkpoint/:checkpointId` — fork new sandbox
   - `DELETE /api/sandboxes/:id/checkpoints/:checkpointId` — delete
7. **Checkpoint Patches**
   - `POST /api/sandboxes/checkpoints/:checkpointId/patches` — create
   - `GET /api/sandboxes/checkpoints/:checkpointId/patches` — list
   - `DELETE /api/sandboxes/checkpoints/:checkpointId/patches/:patchId` — delete
8. **Preview URLs**
   - `POST /api/sandboxes/:id/preview` — create
   - `GET /api/sandboxes/:id/preview` — list
   - `DELETE /api/sandboxes/:id/preview/:port` — delete
9. **Templates**
   - `POST /api/templates` — build
   - `GET /api/templates` — list
   - `GET /api/templates/:name` — get
   - `DELETE /api/templates/:name` — delete
10. **PTY**
    - `POST /api/sandboxes/:id/pty` — create
    - `GET /api/sandboxes/:id/pty/:sessionID` — WebSocket
    - `POST /api/sandboxes/:id/pty/:sessionID/resize` — resize
    - `DELETE /api/sandboxes/:id/pty/:sessionID` — kill
11. **WebSocket Binary Protocol** (essential for non-SDK users)
    - Exec/PTY sessions use binary WebSocket frames
    - Input: prefix byte `0x00` + stdin data
    - Output prefix bytes: `0x01` (stdout), `0x02` (stderr), `0x03` (exit code, 4-byte big-endian int32), `0x04` (scrollback end marker)
    - On connect: server replays scrollback buffer (historical output), then sends `0x04`, then live-streams
12. **Error format** — `{"error": "message"}` envelope, common status codes (400, 401, 403, 404, 409, 500)

Each endpoint: method, path, request body (JSON), response body (JSON), status codes, curl example.

**Source:** `internal/api/router.go` (routes), `internal/api/sandbox.go` + `exec_session.go` + `agent_session.go` + `filesystem.go` + `templates.go` (handlers).

#### `reference/typescript-sdk.mdx` — NEW

**Goal:** Exhaustive TypeScript SDK reference. Every class, every method, every type, every parameter. The page a developer lands on when they need the exact signature.

**Structure:**
1. **Installation & setup** — `npm install @opencomputer/sdk`, env vars
2. **Sandbox** class
   - Static: `create(opts?)`, `connect(sandboxId, opts?)`, `createFromCheckpoint(checkpointId, opts?)`, `createCheckpointPatch(checkpointId, opts)`, `listCheckpointPatches(checkpointId, opts?)`, `deleteCheckpointPatch(checkpointId, patchId, opts?)`
   - Instance: `kill()`, `isRunning()`, `hibernate()`, `wake(opts?)`, `setTimeout(timeout)`, `createCheckpoint(name)`, `listCheckpoints()`, `restoreCheckpoint(checkpointId)`, `deleteCheckpoint(checkpointId)`, `createPreviewURL(opts)`, `listPreviewURLs()`, `deletePreviewURL(port)`
   - Properties: `sandboxId`, `agent`, `exec`, `files`, `pty`
   - All types: `SandboxOpts`, `CheckpointInfo`, `PatchInfo`, `PatchResult`, `PreviewURLResult`
3. **Agent** class
   - `start(opts?)`, `attach(sessionId, opts?)`, `list()`
   - `AgentSession`: `sessionId`, `done`, `sendPrompt()`, `interrupt()`, `configure()`, `kill()`, `close()`
   - Types: `AgentStartOpts`, `AgentConfig`, `AgentEvent`, `McpServerConfig`
4. **Exec** class
   - `run(command, opts?)`, `start(command, opts?)`, `attach(sessionId, opts?)`, `list()`, `kill(sessionId, signal?)`
   - `ExecSession`: `sessionId`, `done`, `sendStdin()`, `kill()`, `close()`
   - Types: `RunOpts`, `ExecStartOpts`, `ExecAttachOpts` (with `onScrollbackEnd`), `ProcessResult`, `ExecSessionInfo`
5. **Filesystem** class
   - `read(path)`, `readBytes(path)`, `write(path, content)`, `list(path?)`, `makeDir(path)`, `remove(path)`, `exists(path)`
   - Types: `EntryInfo`
6. **Pty** class
   - `create(opts?)`
   - `PtySession`: `sessionId`, `send()`, `close()`
   - Types: `PtyOpts`
7. **Templates** class (note: plural, standalone — not a Sandbox property)
   - `build(name, dockerfile)`, `list()`, `get(name)`, `delete(name)`
   - Types: `TemplateInfo`

Each method: full signature, params with types and defaults, return type, one-line example.

**Source:** `sdks/typescript/src/` — all source files.

#### `reference/python-sdk.mdx` — NEW

**Goal:** Same as TypeScript reference but for Python. Exhaustive, every class/method/type.

**Structure:** Documents the Python SDK accurately, noting gaps vs TypeScript.
1. **Installation & setup** — `pip install opencomputer-sdk`, env vars
2. **Sandbox** class — all static and instance methods (snake_case)
   - Class methods: `create(template, timeout, api_key, api_url, envs, metadata)`, `connect()`, `create_from_checkpoint()`
   - Static: `create_checkpoint_patch()`, `list_checkpoint_patches()`, `delete_checkpoint_patch()`
   - Instance: `kill()`, `is_running()`, `set_timeout()`, `create_checkpoint()`, `list_checkpoints()`, `restore_checkpoint()`, `delete_checkpoint()`, `create_preview_url()`, `list_preview_urls()`, `delete_preview_url()`, `close()`
   - Context manager: `async with Sandbox.create() as sandbox:` (auto-kills on exit)
   - **Not available in Python (use HTTP API):** `hibernate()`, `wake()`, `cpuCount`/`memoryMB` create params
3. **Agent** class
   - `start(prompt, model, system_prompt, allowed_tools, permission_mode, max_turns, cwd, mcp_servers, on_event, on_error)` — **no `resume` param**
   - `attach(session_id, on_event, on_error)`, `list()`
   - `AgentSession`: `session_id`, `collect_events()`, `wait()`, `send_prompt()`, `interrupt()`, `configure()`, `kill()`, `close()`
   - `AgentEvent` dataclass with `type`, `data` fields + dict-like `[]` and `.get()` access
4. **Exec** class
   - `run(command, timeout, env, cwd)` → `ProcessResult`
   - `start(command, args, env, cwd, timeout)` → `str` (session ID, **not** an ExecSession object)
   - `list()`, `kill(session_id, signal)`
   - **Not available in Python:** `attach()`, streaming callbacks, `maxRunAfterDisconnect`
5. **Filesystem** class
   - `read()`, `read_bytes()`, `write()`, `list()`, `make_dir()`, `remove()`, `exists()`
6. **Pty** class — `create(cols, rows, on_output)`, `PtySession` with `send()`, `recv()`, `close()`
   - **Python-unique:** `recv()` method on PtySession (pull-based alternative to callback)
7. **Template** class (note: singular, standalone)
   - `build()`, `list()`, `get()`, `delete()`

Each method: full async signature, params with types and defaults, return type, one-line example.

**Source:** `sdks/python/opencomputer/` — all source files.

#### `reference/cli.mdx` — NEW

**Goal:** Exhaustive CLI reference. Every command, every subcommand, every flag. The lookup page for exact syntax when the CLI guide pages aren't enough.

**Structure:**
1. **Global flags** — `--api-key`, `--api-url`, `--json`
2. **`oc sandbox`** — create (flags: --template, --timeout, --cpu, --memory, --env, --metadata), list, get, kill, hibernate, wake, set-timeout
3. **`oc exec`** — exec (flags: --cwd, --timeout, --env), list, attach, kill
4. **`oc shell`** — shell (flags: --shell)
5. **`oc checkpoint`** — create (--name), list, restore, spawn (--timeout), delete. Alias: `oc cp`
6. **`oc patch`** — create (--script, --description), list, delete
7. **`oc preview`** — create (--port, --domain), list, delete
8. **`oc config`** — set, show

Each command: full syntax, flags table, output example.

**Source:** `cmd/oc/internal/commands/` — all command files.

---

### CLI (guide-like pages)

The CLI has two tiers:

1. **CLI nav group** — 7 guide-like pages covering install, config, and key workflows per topic. Each page teaches patterns, concepts, and when to use what — not exhaustive flag reference.
2. **Reference nav group** — one `reference/cli.mdx` page with every command, every subcommand, every flag. The exhaustive lookup page.

Each CLI guide page ends with a `<Tip>` linking to both:
- The SDK entity page for the same topic (e.g., cli/checkpoint → sandboxes/checkpoints)
- The relevant section in `reference/cli.mdx` for the full flag spec

#### `cli/overview.mdx` — REWRITE
- Installation (4 platforms)
- Configuration (`oc config set`)
- Resolution order (flags > env > config > defaults)
- Global flags
- JSON output mode & scripting with jq
- Key workflows (create-and-shell, piping patterns)
- Command index table linking to per-topic pages

#### `cli/sandbox.mdx` — REWRITE (guide)
- Creating, listing, inspecting sandboxes
- Hibernation & wake patterns
- Adjusting timeouts
- Killing a sandbox
- Common patterns: create-and-shell, filter running sandboxes

#### `cli/exec.mdx` — REWRITE from commands.mdx (guide)
- Running commands, working directory, env vars
- Timeouts
- JSON output capture for scripting
- Managing exec sessions (list, attach, kill)
- When to use exec vs shell

#### `cli/shell.mdx` — REWRITE (guide)
- Opening a shell, choosing shell binary
- What works (full-screen apps, colors, resizing)
- Shell vs exec decision table
- Tips and one-liners

#### `cli/checkpoint.mdx` — REWRITE (guide)
- Creating checkpoints
- Forking pattern (parallel exploration)
- Restoring
- Checkpoint vs hibernate decision table

#### `cli/patch.mdx` — REWRITE from patches.mdx (guide)
- What patches do
- Creating from file or stdin
- Layering setup steps pattern
- Inspecting before spawning

#### `cli/preview.mdx` — REWRITE from previews.mdx (guide)
- Exposing ports
- Sharing a dev server workflow
- Multiple ports
- Custom domains

---

### Guides (KEEP + minor edits)

#### `guides/build-a-lovable-clone.mdx` — KEEP
Good content. Minor edits:
- Update "Coming Soon" section (some features now exist: agent sessions, checkpoints)
- Ensure code examples use `sandbox.exec` not `sandbox.commands`

#### `guides/agent-skill.mdx` — KEEP
Minimal, functional. No changes needed.

---

### New Pages

#### `troubleshooting.mdx` — NEW

**Structure:**
1. Common errors:
   - `401 Unauthorized` → check API key, env var name
   - `Sandbox not found` → sandbox may have been killed or timed out
   - `Connection refused` → sandbox still creating, or hibernated
   - Timeout errors → increase timeout, check idle timeout
2. Debugging tips:
   - Check sandbox status with `sandbox.isRunning()`
   - Use `sandbox.exec.list()` to see running processes
   - Agent stderr via `onError` callback
3. Getting help: GitHub issues link, support channels

---

## Pages to Delete

These pages are fully merged into the new directory-based structure and should be removed:

```
# SDK tab pages (all 16 → merged into agents/ and sandboxes/ pages)
sdks/typescript/overview.mdx     → introduction.mdx install section
sdks/typescript/sandbox.mdx      → sandboxes/overview.mdx
sdks/typescript/commands.mdx     → sandboxes/running-commands.mdx
sdks/typescript/filesystem.mdx   → sandboxes/working-with-files.mdx
sdks/typescript/pty.mdx          → sandboxes/interactive-terminals.mdx
sdks/typescript/templates.mdx    → sandboxes/templates.mdx
sdks/typescript/checkpoints.mdx  → sandboxes/checkpoints.mdx
sdks/typescript/patches.mdx      → sandboxes/patches.mdx
sdks/python/overview.mdx         → (same as TS above)
sdks/python/sandbox.mdx
sdks/python/commands.mdx
sdks/python/filesystem.mdx
sdks/python/pty.mdx
sdks/python/templates.mdx
sdks/python/checkpoints.mdx
sdks/python/patches.mdx

# Old root-level feature pages (3 → moved into agents/ and sandboxes/ dirs)
agents.mdx                       → agents/overview.mdx
running-commands.mdx             → sandboxes/running-commands.mdx
working-with-files.mdx           → sandboxes/working-with-files.mdx

# CLI renames (4)
cli/commands.mdx                 → cli/exec.mdx
cli/checkpoints.mdx              → cli/checkpoint.mdx
cli/patches.mdx                  → cli/patch.mdx
cli/previews.mdx                 → cli/preview.mdx
```

Total: 16 SDK pages deleted, 3 root pages moved, 4 CLI pages renamed.

---

## Audit Notes (2026-03-11)

Cross-referenced against TS SDK, Python SDK, HTTP API handlers, and CLI source code. All findings have been folded into page specs above. This section is a reference for decisions made.

### SDK parity: what to watch for during writing

The Python SDK is **not feature-equivalent** to TypeScript. Every page spec above annotates which features are TS-only. The key gaps: `hibernate`/`wake`, `cpuCount`/`memoryMB`, `exec.start` streaming, `exec.attach`, agent `resume`, `onExit`/`onScrollbackEnd` callbacks. Python-unique features (`collect_events`, `wait`, `recv`, context manager) are also annotated.

### Naming conventions across surfaces

- TS `Templates` (plural) vs Python `Template` (singular) — both standalone, not on Sandbox
- TS camelCase (`sandboxId`, `isDir`) vs Python snake_case (`sandbox_id`, `is_dir`) vs HTTP ALLCAPS-ish (`sandboxID`, `checkpointID`)
- `CheckpointInfo` fields differ per surface — TS has `sandboxConfig` and `orgId`, HTTP includes `sizeBytes`, Python returns raw dict

### Intentionally omitted from docs

| Item | Reason |
|------|--------|
| `alias` param on sandbox create | Declared in types but unused — no code reads it |
| `networkEnabled` param | Declared but no-op — all sandboxes have networking unconditionally |
| `imageRef`, `templateRootfsKey`, `templateWorkspaceKey` | Internal plumbing for template/ECR resolution |
| `port` on sandbox create | Internal routing default; user-facing port exposure is via `createPreviewURL()` |
| `commands.py` legacy file in Python SDK | Not exported in `__init__.py`, superseded by `exec.py` |
| Dashboard-only routes (`/api/dashboard/*`) | Separate auth model (WorkOS sessions), not part of public SDK API |

### Items folded into page specs (no longer open)

| Item | Where |
|------|-------|
| `saveAsTemplate` | templates page (as dashboard feature) |
| `POST /sandboxes/:id/token/refresh` | reference/api.mdx auth section |
| PTY `shell` param | reference/api.mdx PTY section |
| PTY resize (HTTP only, no SDK) | reference/api.mdx + sandboxes/interactive-terminals.mdx (noted as gap) |
| Agent events are SDK-abstracted (no agent WS endpoint) | reference/api.mdx agent section |
| `ExecSessionInfo.attachedClients` | both SDK reference pages |

---

## Execution Order

### Phase 1: Core pages (do first — everything else references these)
1. Create `agents/overview.mdx` (the headline entity — most users start here)
2. Create `sandboxes/overview.mdx` (the foundational primitive)
3. Create `how-it-works.mdx` (architecture + technical decisions)
4. Rewrite `introduction.mdx` (links to agents, sandboxes, how-it-works)
5. Rewrite `quickstart.mdx` (single agent example)

### Phase 2: Agent sub-pages
6. Create `agents/events.mdx`
7. Create `agents/tools.mdx`
8. Create `agents/multi-turn.mdx`

### Phase 3: Sandbox sub-entity and operation pages
9. Rewrite `sandboxes/running-commands.mdx` (merge TS + Python exec pages)
10. Rewrite `sandboxes/working-with-files.mdx` (merge TS + Python filesystem pages)
11. Create `sandboxes/interactive-terminals.mdx` (promote from SDK-only)
12. Rewrite `sandboxes/checkpoints.mdx` (entity page: concept + API)
13. Rewrite `sandboxes/templates.mdx` (entity page: concept + API)
14. Rewrite `sandboxes/patches.mdx` (entity page: concept + API)
15. Create `sandboxes/preview-urls.mdx` (entity page: concept + API)

### Phase 4: Reference Pages
16. Create `reference/api.mdx` (HTTP API — derived from router.go)
17. Create `reference/typescript-sdk.mdx` (derived from sdks/typescript/src/)
18. Create `reference/python-sdk.mdx` (derived from sdks/python/opencomputer/)
19. Create `reference/cli.mdx` (exhaustive CLI reference — derived from cmd/oc/)

### Phase 5: CLI Guide Pages + Support
20. Rewrite `cli/overview.mdx` (install, config, workflows)
21. Rewrite `cli/sandbox.mdx` (guide: sandbox management patterns)
22. Rewrite `cli/exec.mdx` (guide: running commands — rename from commands)
23. Rewrite `cli/shell.mdx` (guide: interactive terminal tips)
24. Rewrite `cli/checkpoint.mdx` (guide: checkpoint workflows)
25. Rewrite `cli/patch.mdx` (guide: patch patterns — rename from patches)
26. Rewrite `cli/preview.mdx` (guide: preview URL workflows — rename from previews)
27. Create `troubleshooting.mdx`

### Phase 6: Cleanup
28. Update `guides/build-a-lovable-clone.mdx` (minor fixes)
29. Delete all `sdks/` pages
30. Delete old root-level feature pages (agents.mdx, running-commands.mdx, working-with-files.mdx)
31. Delete obsolete CLI pages (commands.mdx, checkpoints.mdx [plural], patches.mdx, previews.mdx)

Note: `mint.json` navigation already updated during stub scaffolding pass.

---

## Page Count Summary

| Section | Current | Proposed | Delta |
|---------|---------|----------|-------|
| Getting Started | 2 | 3 | +1 |
| Agents | 1* | 4 | +3 |
| Sandboxes | 2* | 8 | +6 |
| CLI | 7 | 7 | 0 |
| Guides | 2 | 2 | 0 |
| Reference | 0 | 4 | +4 |
| Resources | 0 | 1 | +1 |
| SDK (tabs) | 16 | 0 | -16 |
| **Total** | **30** | **29** | **-1** |

*Current agents.mdx + running-commands.mdx + working-with-files.mdx exist at root level without clear grouping.

CLI nav group = 7 guide-like pages (overview + per-topic). Reference group = HTTP API + TS SDK + Python SDK + CLI Reference (exhaustive). CLI pages teach workflows; Reference/CLI has every flag.

Net result: 1 fewer page, but every page earns its place. Entity pages teach with curated examples; Reference pages are exhaustive lookup. Zero duplication between SDK tabs.

---

## Quality Bar

Each page must pass these checks before shipping:
- [ ] Opens with a working code example (not prose)
- [ ] Code examples use tabs (TypeScript / Python / HTTP API) where applicable
- [ ] HTTP API tab included for operations that map cleanly to a single endpoint
- [ ] Streaming/WebSocket operations can omit HTTP tab (SDK-only is fine)
- [ ] No deprecated API names (`commands` → `exec`)
- [ ] All parameters documented match actual SDK source code (verified per-SDK, not assumed identical)
- [ ] TS-only features clearly marked — no Python tab that shows nonexistent API
- [ ] Python-unique features (context manager, collect_events, recv) documented where relevant
- [ ] No "coming soon" for features that now exist
- [ ] No filler sentences ("In this section we will..." — just do it)
- [ ] Entity pages: `<Tip>` at bottom linking to CLI guide (if applicable) + Reference pages (TS SDK, Python SDK, HTTP API)
- [ ] CLI guide pages: `<Tip>` at bottom linking to SDK entity page + reference/cli.mdx section
