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

The two top-level entities are **Sandboxes** (the compute primitive) and **Agents** (Claude running inside sandboxes). Everything else — files, checkpoints, templates, patches, preview URLs — are sub-entities that the user needs to understand in context. Each entity page is self-contained: opens with what the entity *is*, then shows how to use it. No separate "Concepts" section.

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
│── sandboxes.mdx                  ← NEW (entity: what sandboxes are + lifecycle + create/kill/hibernate API)
│── agents.mdx                     ← REWRITE (entity: what agents are + full session API)
│── running-commands.mdx           ← REWRITE (merge SDK exec pages, unified CodeGroup)
│── working-with-files.mdx         ← REWRITE (merge SDK filesystem pages, unified CodeGroup)
│── interactive-terminals.mdx      ← NEW (promote from SDK-only to top-level)
│── checkpoints.mdx                ← REWRITE (entity: what checkpoints are + API)
│── templates.mdx                  ← REWRITE (entity: what templates are + API)
│── patches.mdx                    ← REWRITE (entity: what patches are + API)
│── preview-urls.mdx               ← NEW (entity: what preview URLs are + API)
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
│   ├── typescript/                  (content merged into entity/feature pages)
│   └── python/                     (content merged into entity/feature pages)
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
      "group": "Sandboxes",
      "pages": [
        "sandboxes",
        "running-commands",
        "working-with-files",
        "interactive-terminals",
        "checkpoints",
        "templates",
        "patches",
        "preview-urls"
      ]
    },
    {
      "group": "Agents",
      "pages": [
        "agents"
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

- **Sandboxes group** contains the sandbox entity page plus everything you do with a sandbox (run commands, work with files, open terminals, create checkpoints, build templates, expose URLs). These are all operations on or properties of the sandbox primitive.
- **Agents group** is its own top-level group because agents are conceptually distinct — they're not just another operation on a sandbox, they're an autonomous actor *inside* one. Currently one page, but the group gives room to grow (e.g., MCP servers, multi-turn patterns).
- **No "Concepts" section.** Each entity page (sandboxes, agents, checkpoints, templates, patches, preview URLs) opens with what the entity is and why it exists, then shows the API. Concept and reference live together — one place to look.
- **No "Features" section.** The word "features" is meaningless navigation. The sidebar groups tell you what things *are*.

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
6. Next steps cards → Quickstart, Concepts/Sandboxes

**Changes from current:** Add install for CLI. Tighten copy. Add "Full Linux VM" card. Remove "Run agent tasks easily" card (redundant with Agent SDK card).

#### `quickstart.mdx` — REWRITE

**Goal:** Working code in 5 minutes. Three examples of increasing complexity.

**Structure:**
1. Prerequisites: API key + SDK installed (link to intro for install)
2. Set API key (env var)
3. **Example 1: Run a command** — Simplest possible thing. Create sandbox, run `echo`, print output, kill. (TS + Python)
4. **Example 2: Run an agent** — Create sandbox, `agent.start()` with a real task, stream events. (TS + Python)
5. **Example 3: Checkpoint and fork** — Create sandbox, do work, checkpoint, fork, verify state. (TS + Python)
6. Next steps: link to Concepts for deeper understanding, Features for API reference

**Changes from current:** Add command example before agent example (simpler onramp). Add checkpoint example. Fix Python quickstart (currently shows `sandbox.commands.run` instead of agent example — mismatch with TS).

---

### Entity Pages

Each entity page follows this template:
1. **What is this** — 2-3 sentences explaining the entity for someone who's never seen it
2. **Primary code example** (TS + Python via CodeGroup)
3. **API reference** with `<ParamField>` for each method
4. **Additional examples**
5. **Related** — links to CLI equivalent and related entity pages

#### `sandboxes.mdx` — NEW (entity page)

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

**Absorbs content from:** old `concepts/sandboxes.mdx`, `concepts/persistence.mdx` (hibernation section), `concepts/networking.mdx` (outbound access mention), `sdks/typescript/sandbox.mdx`, `sdks/python/sandbox.mdx`.

#### `agents.mdx` — REWRITE (entity page)

**Goal:** The definitive page for understanding and using agent sessions. This is the other top-level entity alongside sandboxes.

**Structure:**
1. **What is an agent session** — A Claude Agent SDK instance running inside a sandbox. The agent has full access to the sandbox's filesystem and shell. You send it a prompt, it works autonomously — writing files, running commands, iterating on errors — and streams events back as it goes.
2. **Quick example:** `sandbox.agent.start()` with event handling (TS + Python CodeGroup)
3. **`sandbox.agent.start(opts)`** — full param reference:
   - prompt, model, systemPrompt, allowedTools, permissionMode, maxTurns, cwd, mcpServers, resume, onEvent, onError, onExit
   - **NEW: document `resume` param** — pass `claude_session_id` from a previous `turn_complete` event to continue a conversation
4. **AgentSession** — properties and methods table:
   - sessionId, done, sendPrompt, interrupt, configure, kill, close
   - **Python-specific:** `collect_events()`, `wait()`
5. **Agent events** — table of all event types:
   - ready, configured, assistant, tool_use_summary, system, result, turn_complete, interrupted, error
6. **Examples:**
   - Follow-up prompts
   - MCP servers configuration
   - Resume a previous conversation (NEW)
   - List active sessions / attach to existing
7. **Note:** Agent sessions are SDK-only (no CLI command yet)

**Key improvements:** Merge three current agent pages into one. Add `resume` docs. Add Python `collect_events()`. Document all event types including `system` and `tool_use_summary`.

#### `running-commands.mdx` — REWRITE

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

#### `working-with-files.mdx` — REWRITE

**Structure:** Keep current structure — it's already good. Add CodeGroup for all examples.
1. Reading files (read, readBytes)
2. Writing files (write — text and binary)
3. Listing directories (list, EntryInfo)
4. Managing files (makeDir, remove, exists)
5. Examples: upload & run script, copy files

**Minimal changes needed.** Merge TS/Python into CodeGroups and ensure consistency.

#### `interactive-terminals.mdx` — NEW (promote from SDK-only)

**Structure:**
1. **What is a PTY session** — A full interactive terminal inside the sandbox, like SSH but over WebSocket. Supports colors, resize, full-screen apps (vim, top).
2. Create a PTY session (TS + Python CodeGroup)
3. PtyOpts reference (cols, rows, onOutput)
4. PtySession methods (send, close)
5. Examples: run interactive commands, pipe stdin
6. CLI equivalent: `oc shell <sandbox-id>`

#### `checkpoints.mdx` — REWRITE (entity page)

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

#### `templates.mdx` — REWRITE (entity page)

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

#### `patches.mdx` — REWRITE (entity page)

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

#### `preview-urls.mdx` — NEW (entity page)

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

These pages are fully merged into unified feature pages and should be removed:

```
sdks/typescript/overview.mdx     → content merged into introduction.mdx install section
sdks/typescript/sandbox.mdx      → merged into concepts/sandboxes.mdx + feature pages
sdks/typescript/commands.mdx     → merged into running-commands.mdx
sdks/typescript/filesystem.mdx   → merged into working-with-files.mdx
sdks/typescript/pty.mdx          → merged into interactive-terminals.mdx
sdks/typescript/templates.mdx    → merged into templates.mdx
sdks/typescript/checkpoints.mdx  → merged into checkpoints.mdx
sdks/typescript/patches.mdx      → merged into patches.mdx

sdks/python/overview.mdx         → same as above
sdks/python/sandbox.mdx
sdks/python/commands.mdx
sdks/python/filesystem.mdx
sdks/python/pty.mdx
sdks/python/templates.mdx
sdks/python/checkpoints.mdx
sdks/python/patches.mdx

cli/commands.mdx                 → renamed to cli/exec.mdx
cli/patches.mdx                  → merged into cli/checkpoint.mdx
cli/previews.mdx                 → renamed to cli/preview.mdx
```

Total: 16 SDK pages deleted, 3 CLI pages renamed/merged.

---

## Content Gaps to Fill

These are specific pieces of information that exist in the codebase but are missing from docs:

| Gap | Source | Target Page |
|-----|--------|-------------|
| Agent session `resume` parameter | `sdks/typescript/src/agent.ts` | agents.mdx |
| `maxRunAfterDisconnect` in exec | `sdks/typescript/src/exec.ts` | running-commands.mdx |
| Sandbox resource options (`cpuCount`, `memoryMB`) | `sdks/typescript/src/sandbox.ts` | sandboxes.mdx |
| Sandbox `metadata` option | `sdks/typescript/src/sandbox.ts` | sandboxes.mdx |
| Sandbox `envs` option (persistent env vars) | `sdks/typescript/src/sandbox.ts` | sandboxes.mdx |
| Hibernation API (`sandbox.hibernate()`, `sandbox.wake()`) | Both SDKs | sandboxes.mdx |
| Sandbox status states & transitions | `internal/sandbox/router.go` | sandboxes.mdx |
| Default template contents | `deploy/firecracker/rootfs/Dockerfile.default` | templates.mdx |
| Preview URL `authConfig` option | Both SDKs | preview-urls.mdx |
| Preview URL custom domain verification | Worker code | preview-urls.mdx |
| Python `AgentSession.collect_events()` | `sdks/python/opencomputer/agent.py` | agents.mdx |
| Python `AgentSession.wait()` | `sdks/python/opencomputer/agent.py` | agents.mdx |
| Agent `tool_use_summary` and `system` event types | Agent wrapper code | agents.mdx |
| Rolling timeout behavior | `internal/sandbox/router.go` | sandboxes.mdx |
| Exec session scrollback buffer | `internal/sandbox/scrollback.go` | running-commands.mdx |
| Sandbox `connect()` (attach to existing) | Both SDKs | sandboxes.mdx |

---

## Execution Order

### Phase 1: Top-level entities (do first — everything else references these)
1. Create `sandboxes.mdx` (the foundational entity page)
2. Rewrite `agents.mdx` (the other top-level entity)
3. Rewrite `introduction.mdx` (now links to entity pages, not concepts)
4. Rewrite `quickstart.mdx`

### Phase 2: Sandbox sub-entity and operation pages
5. Rewrite `running-commands.mdx` (merge TS + Python exec pages)
6. Rewrite `working-with-files.mdx` (merge TS + Python filesystem pages)
7. Create `interactive-terminals.mdx` (promote from SDK-only)
8. Rewrite `checkpoints.mdx` (entity page: concept + API)
9. Rewrite `templates.mdx` (entity page: concept + API)
10. Rewrite `patches.mdx` (entity page: concept + API)
11. Create `preview-urls.mdx` (entity page: concept + API)

### Phase 3: CLI + Support Pages
12. Rewrite `cli/overview.mdx`
13. Update `cli/sandbox.mdx`
14. Create `cli/exec.mdx` (rename from commands)
15. Update `cli/shell.mdx`
16. Create `cli/checkpoint.mdx` (merge checkpoint + patch)
17. Create `cli/preview.mdx` (rename from previews)
18. Create `troubleshooting.mdx`
19. Create `changelog.mdx` (stub)

### Phase 4: Cleanup
20. Update `guides/build-a-lovable-clone.mdx` (minor fixes)
21. Delete all `sdks/` pages
22. Delete obsolete CLI pages
23. Update `mint.json` with new navigation

---

## Page Count Summary

| Section | Current | Proposed | Delta |
|---------|---------|----------|-------|
| Getting Started | 2 | 2 | 0 |
| Sandboxes | 3 | 8 | +5 |
| Agents | 0* | 1 | +1 |
| CLI | 7 | 6 | -1 |
| Guides | 2 | 2 | 0 |
| Resources | 0 | 2 | +2 |
| SDK (tabs) | 16 | 0 | -16 |
| **Total** | **30** | **21** | **-9** |

*agents.mdx existed but wasn't in its own nav group.

Net result: 9 fewer pages, more complete coverage, zero duplication. Every entity self-contained.

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
