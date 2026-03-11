# OpenComputer Docs Rewrite Plan

## Guiding Principles

1. **Single sidebar, no tabs.** Every page lives in one navigation tree. SDK languages shown side-by-side via CodeGroup, not separated into tabs.
2. **Entity-first.** Pages organized around things the user encounters (sandboxes, agents, checkpoints, templates), not by SDK or abstract category. Each entity page is self-contained: what it is, how to use it, full API reference.
3. **Quality over quantity.** If it can be said in fewer words, it should be. No filler sections. Every page earns its place.
4. **Entity → Example → Reference** flow on each page. Open with what the entity *is* (2-3 sentences), show a working code example, then provide the full API reference below.
5. **Code-forward.** The first thing on every entity page (after the short explanation) should be a working code example. Parameters and types come after.
6. **Honest about gaps.** Don't document features that don't exist yet. Mark experimental/beta features clearly.

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
├── cli/                            ← KEEP (trimmed)
│   ├── overview.mdx
│   ├── sandbox.mdx
│   ├── exec.mdx
│   ├── shell.mdx
│   ├── checkpoint.mdx
│   └── preview.mdx
│
├── guides/                         ← KEEP
│   ├── build-a-lovable-clone.mdx
│   └── agent-skill.mdx
│
│── troubleshooting.mdx            ← NEW
│── changelog.mdx                  ← NEW (stub)
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
        "quickstart"
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
      "group": "Resources",
      "pages": [
        "troubleshooting",
        "changelog"
      ]
    }
  ]
}
```

### Why this structure

- **Agents first.** This is the headline feature — most users land here to run Claude inside sandboxes. Putting Agents right after Getting Started matches the reader's intent.
- **Agents get depth.** Four pages mirror the Sandboxes pattern: an overview page explaining what agents are, then dedicated pages for events (the output you consume), tools (configuring what agents can do), and multi-turn (conversations that span sessions). Each page earns its place by covering a distinct concern.
- **Sandboxes group** contains the sandbox entity page plus everything you do with a sandbox (run commands, work with files, open terminals, create checkpoints, build templates, expose URLs).
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
4. Install (CodeGroup: npm + pip + CLI)
5. Minimal example: create sandbox → run agent → get result (TS + Python side by side)
6. Next steps cards → Quickstart, Agents, Sandboxes

**Changes from current:** Add install for CLI. Tighten copy. Add "Full Linux VM" card. Remove "Run agent tasks easily" card (redundant with Agent SDK card).

#### `quickstart.mdx` — REWRITE

**Goal:** Working code in 5 minutes. Three examples of increasing complexity.

**Structure:**
1. Prerequisites: API key + SDK installed (link to intro for install)
2. Set API key (env var)
3. **Example 1: Run a command** — Simplest possible thing. Create sandbox, run `echo`, print output, kill. (TS + Python)
4. **Example 2: Run an agent** — Create sandbox, `agent.start()` with a real task, stream events. (TS + Python)
5. **Example 3: Checkpoint and fork** — Create sandbox, do work, checkpoint, fork, verify state. (TS + Python)
6. Next steps: link to Agents (deep dive), Sandboxes (the primitive), Checkpoints (persistence)

**Changes from current:** Add command example before agent example (simpler onramp). Add checkpoint example. Fix Python quickstart (currently shows `sandbox.commands.run` instead of agent example — mismatch with TS).

---

### Entity Pages

Each entity page follows this template:
1. **What is this** — 2-3 sentences explaining the entity for someone who's never seen it
2. **Primary code example** (TS + Python via CodeGroup)
3. **API reference** with `<ParamField>` for each method
4. **Additional examples**
5. **Related** — links to CLI equivalent and related entity pages

### Agents (`agents/`)

#### `agents/overview.mdx` — REWRITE (entity page)

**Goal:** Explain what agent sessions are, how they work, and get the reader to a working agent in 60 seconds.

**Structure:**
1. **What is an agent session** — A Claude Agent SDK instance running inside a sandbox. The agent has full access to the sandbox's filesystem and shell. You send it a prompt, it works autonomously — writing files, running commands, iterating on errors — and streams events back as it goes.
2. **Quick example:** `sandbox.agent.start()` with event handling (TS + Python CodeGroup) — the simplest working agent
3. **How it works** — brief (3-4 sentences): the SDK spawns Claude inside the sandbox VM, Claude gets bash/file tools, it works in a loop (think → act → observe), events stream back to your code via WebSocket.
4. **`sandbox.agent.start(opts)`** — full param reference:
   - prompt, model, systemPrompt, allowedTools, permissionMode, maxTurns, cwd, mcpServers, resume, onEvent, onError, onExit
5. **AgentSession** — properties and methods table:
   - sessionId, done, sendPrompt, interrupt, configure, kill, close
   - **Python-specific:** `collect_events()`, `wait()`
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
2. **Resuming across sessions** — `resume` parameter in `agent.start()`
   - How it works: capture `claude_session_id` from `turn_complete` event, pass it as `resume` in a new `agent.start()` call
   - Example: save session ID, create new sandbox from checkpoint, resume conversation
   - **NEW: first time this is documented**
3. **Interrupting** — `session.interrupt()` to stop the current turn
4. **Reconfiguring mid-session** — `session.configure()` to change model, tools, etc.
5. **Managing sessions:**
   - `sandbox.agent.list()` — list active sessions
   - `sandbox.agent.attach(sessionId)` — reconnect to a running session (get events you missed)
   - When to use attach vs resume
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
   - Default CPU: 1 vCPU (configurable up to 4 via `cpuCount`)
   - Default memory: 512MB (configurable up to 2GB via `memoryMB`)
   - Storage: 20GB workspace
   - Network: full outbound internet access
   - Pre-installed: Python 3, Node.js, common CLI tools
4. **Creating a sandbox** — `Sandbox.create(opts)` with full param reference:
   - template, timeout, apiKey, apiUrl, envs, metadata, cpuCount, memoryMB
5. **Connecting to an existing sandbox** — `Sandbox.connect(sandboxId)`
6. **Sandbox lifecycle:**
   - Status states: `creating → running → hibernated → killed`
   - Lifecycle diagram (text-based)
   - Rolling timeout: resets on every operation, default 300s
   - What happens on timeout: auto-hibernate if possible, else kill
7. **Hibernation & wake** — `sandbox.hibernate()`, `sandbox.wake()`
   - Like closing a laptop lid: memory + disk snapshotted, sandbox ID stays the same
   - Resume in seconds, no cost while hibernated
   - Auto-triggered on idle timeout
   - Difference from checkpoints: hibernation is transparent resume, checkpoints are named snapshots you fork from
8. **Other methods** — `kill()`, `isRunning()`, `setTimeout()`
9. **Sandbox properties** — sandboxId, agent, exec, files, pty

**Absorbs content from:** `sdks/typescript/sandbox.mdx`, `sdks/python/sandbox.mdx`.

#### `sandboxes/running-commands.mdx` — REWRITE

**Structure:**
1. Brief intro: two modes for running shell commands — `run()` (wait for result) and `start()` (streaming/async)
2. **Quick commands: `sandbox.exec.run()`** — CodeGroup TS + Python
   - Full param reference (command, timeout, env, cwd)
   - ProcessResult table
   - Examples: cwd, env vars, timeout, chaining
3. **Async commands: `sandbox.exec.start()`** — CodeGroup TS + Python
   - Full param reference (command, args, env, cwd, timeout, maxRunAfterDisconnect, onStdout, onStderr, onExit)
   - **NEW: document `maxRunAfterDisconnect`** — process continues running N seconds after WebSocket disconnect
   - ExecSession table (sessionId, done, sendStdin, kill, close)
4. **Managing sessions:**
   - `sandbox.exec.list()` — list running sessions
   - `sandbox.exec.attach()` — reconnect to running session
   - `sandbox.exec.kill()` — kill a session
5. Examples: dev server, long-running process, reconnect pattern

**Key improvement:** Use `sandbox.exec.*` consistently (not `sandbox.commands`). Document `maxRunAfterDisconnect`. Add session management.

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
1. **What is a PTY session** — A full interactive terminal inside the sandbox, like SSH but over WebSocket. Supports colors, resize, full-screen apps (vim, top).
2. Create a PTY session (TS + Python CodeGroup)
3. PtyOpts reference (cols, rows, onOutput)
4. PtySession methods (send, close)
5. Examples: run interactive commands, pipe stdin
6. CLI equivalent: `oc shell <sandbox-id>`

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
5. CheckpointInfo structure (id, name, status, sandboxId, createdAt)
6. Status: `processing → ready`
7. Examples: checkpoint before risky operation, fork for parallel exploration

#### `sandboxes/templates.mdx` — REWRITE (entity page)

**Structure:**
1. **What is a template** — A pre-built base image that sandboxes start from. The `default` template includes Ubuntu, Python, and Node.js. Build custom templates from Dockerfiles to skip setup time.
2. Quick example: build template → create sandbox from it (TS + Python CodeGroup)
3. **Default template** — what's pre-installed (derive from `Dockerfile.default`)
4. **API reference:**
   - `Template.build(name, dockerfile)` — build from Dockerfile
   - `Template.list()` — list available
   - `Template.get(name)` — get details
   - `Template.delete(name)` — delete
   - Using in `Sandbox.create({ template: "my-template" })`
5. TemplateInfo structure
6. Example: template with specific language/framework pre-installed

#### `sandboxes/patches.mdx` — REWRITE (entity page)

**Structure:**
1. **What is a patch** — A shell script attached to a checkpoint that runs every time a sandbox is forked from that checkpoint. Use patches to inject configuration, update dependencies, or customize state at fork time without modifying the checkpoint itself.
2. Quick example: create patch on checkpoint (TS + Python CodeGroup)
3. **API reference:**
   - `Sandbox.createCheckpointPatch(checkpointId, { script, description })`
   - `Sandbox.listCheckpointPatches(checkpointId)`
   - `Sandbox.deleteCheckpointPatch(checkpointId, patchId)`
4. When patches run (table: fork = yes, restore = yes/no)
5. Execution order: patches run in sequence order
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
4. **Custom domains:**
   - How to verify your domain
   - DNS setup (TXT for verification, CNAME for routing)
   - SSL is automatic
5. Preview URLs persist across hibernation/wake cycles
6. Examples: share a dev server, multiple ports, custom domain

---

### CLI Reference (KEEP, streamline)

CLI pages stay as a separate nav group. These are reference-only (no conceptual content — that lives in feature pages). Each page: flags table, output examples, practical one-liners.

#### `cli/overview.mdx` — REWRITE
- Installation (4 platforms)
- Configuration (`oc config set`)
- Resolution order (flags > env > config > defaults)
- Global flags
- JSON output mode

#### `cli/sandbox.mdx` — KEEP (minor edits)
#### `cli/exec.mdx` — RENAME from commands.mdx, update to match `oc exec` naming
#### `cli/shell.mdx` — KEEP (minor edits)
#### `cli/checkpoint.mdx` — MERGE current checkpoint + patch pages
#### `cli/preview.mdx` — RENAME from previews.mdx (singular)

**Deleted CLI pages:** `cli/patches.mdx` (merged into checkpoint)

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

#### `changelog.mdx` — NEW (stub)

Placeholder page with latest version info and link to GitHub releases. Keep minimal — will grow organically.

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

# CLI renames/merges (3)
cli/commands.mdx                 → cli/exec.mdx
cli/patches.mdx                  → merged into cli/checkpoint.mdx
cli/previews.mdx                 → cli/preview.mdx
```

Total: 16 SDK pages deleted, 3 root pages moved, 3 CLI pages renamed/merged.

---

## Content Gaps to Fill

These are specific pieces of information that exist in the codebase but are missing from docs:

| Gap | Source | Target Page |
|-----|--------|-------------|
| Agent session `resume` parameter | `sdks/typescript/src/agent.ts` | agents/multi-turn.mdx |
| Agent `tool_use_summary` and `system` event types | Agent wrapper code | agents/events.mdx |
| MCP server configuration details | Both SDKs | agents/tools.mdx |
| Python `AgentSession.collect_events()` | `sdks/python/opencomputer/agent.py` | agents/overview.mdx |
| Python `AgentSession.wait()` | `sdks/python/opencomputer/agent.py` | agents/overview.mdx |
| `maxRunAfterDisconnect` in exec | `sdks/typescript/src/exec.ts` | sandboxes/running-commands.mdx |
| Exec session scrollback buffer | `internal/sandbox/scrollback.go` | sandboxes/running-commands.mdx |
| Sandbox resource options (`cpuCount`, `memoryMB`) | `sdks/typescript/src/sandbox.ts` | sandboxes/overview.mdx |
| Sandbox `metadata` option | `sdks/typescript/src/sandbox.ts` | sandboxes/overview.mdx |
| Sandbox `envs` option (persistent env vars) | `sdks/typescript/src/sandbox.ts` | sandboxes/overview.mdx |
| Hibernation API (`sandbox.hibernate()`, `sandbox.wake()`) | Both SDKs | sandboxes/overview.mdx |
| Sandbox status states & transitions | `internal/sandbox/router.go` | sandboxes/overview.mdx |
| Rolling timeout behavior | `internal/sandbox/router.go` | sandboxes/overview.mdx |
| Sandbox `connect()` (attach to existing) | Both SDKs | sandboxes/overview.mdx |
| Default template contents | `deploy/firecracker/rootfs/Dockerfile.default` | sandboxes/templates.mdx |
| Preview URL `authConfig` option | Both SDKs | sandboxes/preview-urls.mdx |
| Preview URL custom domain verification | Worker code | sandboxes/preview-urls.mdx |

---

## Execution Order

### Phase 1: Entity overview pages (do first — everything else references these)
1. Create `agents/overview.mdx` (the headline entity — most users start here)
2. Create `sandboxes/overview.mdx` (the foundational primitive)
3. Rewrite `introduction.mdx` (links to agents + sandboxes)
4. Rewrite `quickstart.mdx`

### Phase 2: Agent sub-pages
5. Create `agents/events.mdx`
6. Create `agents/tools.mdx`
7. Create `agents/multi-turn.mdx`

### Phase 3: Sandbox sub-entity and operation pages
8. Rewrite `sandboxes/running-commands.mdx` (merge TS + Python exec pages)
9. Rewrite `sandboxes/working-with-files.mdx` (merge TS + Python filesystem pages)
10. Create `sandboxes/interactive-terminals.mdx` (promote from SDK-only)
11. Rewrite `sandboxes/checkpoints.mdx` (entity page: concept + API)
12. Rewrite `sandboxes/templates.mdx` (entity page: concept + API)
13. Rewrite `sandboxes/patches.mdx` (entity page: concept + API)
14. Create `sandboxes/preview-urls.mdx` (entity page: concept + API)

### Phase 4: CLI + Support Pages
15. Rewrite `cli/overview.mdx`
16. Update `cli/sandbox.mdx`
17. Create `cli/exec.mdx` (rename from commands)
18. Update `cli/shell.mdx`
19. Create `cli/checkpoint.mdx` (merge checkpoint + patch)
20. Create `cli/preview.mdx` (rename from previews)
21. Create `troubleshooting.mdx`
22. Create `changelog.mdx` (stub)

### Phase 5: Cleanup
23. Update `guides/build-a-lovable-clone.mdx` (minor fixes)
24. Delete all `sdks/` pages
25. Delete old root-level feature pages (agents.mdx, running-commands.mdx, working-with-files.mdx)
26. Delete obsolete CLI pages
27. Update `mint.json` with new navigation

---

## Page Count Summary

| Section | Current | Proposed | Delta |
|---------|---------|----------|-------|
| Getting Started | 2 | 2 | 0 |
| Agents | 1* | 4 | +3 |
| Sandboxes | 2* | 8 | +6 |
| CLI | 7 | 6 | -1 |
| Guides | 2 | 2 | 0 |
| Resources | 0 | 2 | +2 |
| SDK (tabs) | 16 | 0 | -16 |
| **Total** | **30** | **24** | **-6** |

*Current agents.mdx + running-commands.mdx + working-with-files.mdx exist at root level without clear grouping.

Net result: 6 fewer pages, proper depth on both top-level entities, zero duplication.

---

## Quality Bar

Each page must pass these checks before shipping:
- [ ] Opens with a working code example (not prose)
- [ ] Every code example uses CodeGroup for TS + Python (feature pages)
- [ ] No deprecated API names (`commands` → `exec`)
- [ ] All parameters documented match actual SDK code
- [ ] No "coming soon" for features that now exist
- [ ] No filler sentences ("In this section we will..." — just do it)
- [ ] Cross-links to related pages where useful
- [ ] CLI equivalent noted where applicable
