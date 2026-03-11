# OpenComputer Docs Rewrite Plan

## Guiding Principles

1. **Single sidebar, no tabs.** Every page lives in one navigation tree. SDK languages shown side-by-side via CodeGroup, not separated into tabs.
2. **Feature-first, not SDK-first.** Pages organized by what you can *do* (run commands, create checkpoints), not by which SDK you use. Each feature page is the single source of truth — concept + TypeScript + Python + CLI all in one place where appropriate.
3. **Quality over quantity.** If it can be said in fewer words, it should be. No filler sections. Every page earns its place.
4. **Concept → Usage → Reference** flow on each page. Lead with *what* and *why* (1-2 sentences), show the common usage pattern, then provide the full API reference below.
5. **Code-forward.** The first thing on every feature page (after the one-liner description) should be a working code example. Parameters and types come after.
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

```
docs/
├── mint.json
├── images/
│   ├── favicon.svg
│   ├── logo-light.svg
│   ├── logo-dark.svg
│   └── architecture.svg          ← NEW (simple diagram)
│
│── introduction.mdx               ← REWRITE
│── quickstart.mdx                 ← REWRITE
│
├── concepts/                       ← NEW section
│   ├── sandboxes.mdx
│   ├── persistence.mdx
│   └── networking.mdx
│
│── agents.mdx                     ← REWRITE (merge TS/Python SDK agent pages)
│── running-commands.mdx           ← REWRITE (merge TS/Python SDK exec pages)
│── working-with-files.mdx         ← REWRITE (merge TS/Python SDK filesystem pages)
│── interactive-terminals.mdx      ← REWRITE (merge TS/Python SDK pty pages)
│── checkpoints.mdx                ← REWRITE (merge TS/Python SDK checkpoint pages)
│── templates.mdx                  ← REWRITE (merge TS/Python SDK template pages)
│── patches.mdx                    ← REWRITE (merge TS/Python SDK patch pages)
│── preview-urls.mdx               ← REWRITE (merge TS/Python SDK + CLI preview pages)
│
├── cli/                            ← KEEP (trimmed)
│   ├── overview.mdx
│   ├── sandbox.mdx
│   ├── exec.mdx
│   ├── shell.mdx
│   ├── checkpoint.mdx
│   └── preview.mdx
│
├── guides/                         ← KEEP + expand
│   ├── build-a-lovable-clone.mdx
│   └── agent-skill.mdx
│
│── troubleshooting.mdx            ← NEW
│── changelog.mdx                  ← NEW (stub)
│
├── sdks/                           ← DELETE entire directory
│   ├── typescript/                  (content merged into feature pages)
│   └── python/                     (content merged into feature pages)
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
      "group": "Concepts",
      "pages": [
        "concepts/sandboxes",
        "concepts/persistence",
        "concepts/networking"
      ]
    },
    {
      "group": "Features",
      "pages": [
        "agents",
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

### Concepts (NEW section)

#### `concepts/sandboxes.mdx` — NEW

**Goal:** Mental model for what a sandbox is and how it behaves. No API reference here — pure concepts.

**Structure:**
1. What is a sandbox (Firecracker microVM, full Linux, isolated, ephemeral by default)
2. Sandbox lifecycle diagram: `creating → running → hibernated → (wake) → running → killed`
3. Resource specs table:
   - OS: Ubuntu-based Linux
   - Default CPU: 1 vCPU (configurable up to 4)
   - Default memory: 512MB (configurable up to 2GB)
   - Storage: 20GB workspace
   - Network: full outbound internet access
   - Pre-installed: Python 3, Node.js, common CLI tools
4. Idle timeout behavior (rolling timeout, default 300s, resets on every operation)
5. What happens on timeout (auto-hibernate if possible, else kill)
6. Sandbox identity (`sandbox_id` string, used in all API calls)

**Sources:** `internal/firecracker/` (VM config), `internal/sandbox/router.go` (timeout logic), `deploy/firecracker/rootfs/Dockerfile.default` (base image).

#### `concepts/persistence.mdx` — NEW

**Goal:** Explain the persistence model: checkpoints, templates, patches, hibernation. How they relate.

**Structure:**
1. Overview: sandboxes are ephemeral by default but OpenComputer provides three persistence mechanisms
2. **Hibernation** — pause a running sandbox, resume later. Like closing a laptop lid.
   - VM state (memory + disk) snapshotted
   - Sandbox ID stays the same
   - Resume in seconds
   - No cost while hibernated
   - Auto-triggered on idle timeout
3. **Checkpoints** — named snapshots you can fork from. Like git commits.
   - Create from running sandbox
   - Fork new sandboxes from any checkpoint (fast, ~1s)
   - Restore in-place (revert sandbox to checkpoint state)
   - Status: `processing → ready`
4. **Templates** — pre-built base images. Like Docker images.
   - `default` template: Ubuntu + Python + Node.js
   - Custom templates from Dockerfiles
   - `SaveAsTemplate` from a running sandbox
5. **Patches** — scripts that run on checkpoint restore. Modify state at fork time.
   - Attached to checkpoints
   - Execute in sequence order
   - Use case: inject config, update dependencies
6. Comparison table: hibernation vs checkpoint vs template

#### `concepts/networking.mdx` — NEW

**Goal:** Explain how sandbox networking works — outbound access, preview URLs, custom domains.

**Structure:**
1. Outbound networking: full internet access by default, no configuration needed
2. Preview URLs: expose a port to the public internet via HTTPS
   - `createPreviewURL({ port: 3000 })` → returns public URL
   - Multiple ports supported simultaneously
   - URLs persist across hibernation/wake cycles
3. Custom domains: point your own domain at sandbox preview URLs
   - DNS verification flow
   - CNAME setup
   - SSL automatic
4. No inbound SSH (use PTY/exec instead)

---

### Features (unified SDK + concept pages)

Each feature page follows this template:
1. One-sentence description
2. Primary code example (TS + Python via CodeGroup)
3. API reference with `<ParamField>` for each method
4. Additional examples
5. Related: links to CLI equivalent and related concepts

#### `agents.mdx` — REWRITE (merge from: current agents.mdx + sdks/typescript/agent + sdks/python/agent)

**Structure:**
1. Description: run Claude Agent SDK sessions inside a sandbox
2. Quick example: `sandbox.agent.start()` with events (TS + Python CodeGroup)
3. **`sandbox.agent.start(opts)`** — full param reference
   - All params: prompt, model, systemPrompt, allowedTools, permissionMode, maxTurns, cwd, mcpServers, resume, onEvent, onError, onExit
   - **NEW: document `resume` param** — pass `claude_session_id` from a previous `turn_complete` event to continue a conversation
4. **AgentSession** — properties and methods table
   - sessionId, done, sendPrompt, interrupt, configure, kill, close
   - **Python addition:** `collect_events()`, `wait()`
5. **Agent events** — table of event types with descriptions
   - Add `system` and `tool_use_summary` event types (currently missing from docs)
6. **Examples:**
   - Follow-up prompts (start, wait, start again)
   - MCP servers configuration
   - List active sessions
   - Attach to existing session
   - Resume a previous conversation (NEW)
7. CLI equivalent: note that agent sessions are SDK-only (no CLI command yet)

**Key improvement:** Merge all three current agent pages into one. Add `resume` documentation. Add Python `collect_events()`.

#### `running-commands.mdx` — REWRITE (merge from: current + sdks/typescript/commands + sdks/python/commands)

**Structure:**
1. Two modes: `sandbox.exec.run()` (wait for result) and `sandbox.exec.start()` (streaming/async)
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

**Key improvement:** Consolidate to use `sandbox.exec.*` consistently (not `sandbox.commands`). Document `maxRunAfterDisconnect`. Add session management.

#### `working-with-files.mdx` — REWRITE (merge from: current + sdks/typescript/filesystem + sdks/python/filesystem)

**Structure:** Keep current structure, it's already good. Add CodeGroup for all examples.
1. Reading files (read, readBytes)
2. Writing files (write — text and binary)
3. Listing directories (list, EntryInfo)
4. Managing files (makeDir, remove, exists)
5. Examples: upload & run script, copy files

**Minimal changes needed.** Just merge TS/Python into CodeGroups and ensure consistency.

#### `interactive-terminals.mdx` — REWRITE (merge from: sdks/typescript/pty + sdks/python/pty)

**Structure:**
1. What PTY sessions are (full interactive terminal, like SSH but via WebSocket)
2. Create a PTY session (TS + Python CodeGroup)
3. PtyOpts reference (cols, rows, onOutput)
4. PtySession methods (send, close)
5. Examples: run interactive commands, pipe stdin
6. CLI equivalent: `oc shell <sandbox-id>`

**Note:** Currently has no top-level feature page (only exists in SDK tabs). Promoting to feature page.

#### `checkpoints.mdx` — REWRITE (merge from: sdks/typescript/checkpoints + sdks/python/checkpoints + cli/checkpoints)

**Structure:**
1. What checkpoints are (link to concepts/persistence)
2. Create a checkpoint
3. List checkpoints
4. Fork a new sandbox from checkpoint (`Sandbox.createFromCheckpoint()`)
5. Restore in-place (`sandbox.restoreCheckpoint()`)
6. Delete a checkpoint
7. CheckpointInfo structure
8. Examples: checkpoint before risky operation, fork for parallel exploration

#### `templates.mdx` — REWRITE (merge from: sdks/typescript/templates + sdks/python/templates)

**Structure:**
1. What templates are (link to concepts/persistence)
2. Default template (what's pre-installed)
3. Build a custom template from Dockerfile
4. List / get / delete templates
5. Use a template when creating a sandbox
6. TemplateInfo structure
7. Example: create a template with specific tools pre-installed

#### `patches.mdx` — REWRITE (merge from: sdks/typescript/patches + sdks/python/patches + cli/patches)

**Structure:**
1. What patches are (scripts that modify checkpoints at fork time)
2. Create a patch
3. List patches (ordered by sequence)
4. Delete a patch
5. When patches apply (table: fork vs restore)
6. Failure handling
7. Example: inject config, update dependencies at fork time

#### `preview-urls.mdx` — REWRITE (merge from: cli/previews, sandbox methods)

**Structure:**
1. What preview URLs do (expose sandbox port to public internet via HTTPS)
2. Create a preview URL (SDK + CLI)
3. List active previews
4. Delete a preview
5. Custom domains (brief, link to concepts/networking)
6. Examples: share a dev server, multiple ports

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
| Sandbox resource options (`cpuCount`, `memoryMB`) | `sdks/typescript/src/sandbox.ts` | concepts/sandboxes.mdx |
| Sandbox `metadata` option | `sdks/typescript/src/sandbox.ts` | concepts/sandboxes.mdx |
| Sandbox `envs` option (persistent env vars) | `sdks/typescript/src/sandbox.ts` | concepts/sandboxes.mdx |
| Hibernation API (`sandbox.hibernate()`, `sandbox.wake()`) | Both SDKs | concepts/persistence.mdx |
| Sandbox status states & transitions | `internal/sandbox/router.go` | concepts/sandboxes.mdx |
| Default template contents | `deploy/firecracker/rootfs/Dockerfile.default` | templates.mdx |
| Preview URL `authConfig` option | Both SDKs | preview-urls.mdx |
| Preview URL custom domain verification | Worker code | concepts/networking.mdx |
| Python `AgentSession.collect_events()` | `sdks/python/opencomputer/agent.py` | agents.mdx |
| Python `AgentSession.wait()` | `sdks/python/opencomputer/agent.py` | agents.mdx |
| Agent `tool_use_summary` and `system` event types | Agent wrapper code | agents.mdx |
| Rolling timeout behavior | `internal/sandbox/router.go` | concepts/sandboxes.mdx |
| Exec session scrollback buffer | `internal/sandbox/scrollback.go` | running-commands.mdx |
| Sandbox `connect()` (attach to existing) | Both SDKs | concepts/sandboxes.mdx |

---

## Execution Order

### Phase 1: Foundation (do first)
1. Create `concepts/sandboxes.mdx`
2. Create `concepts/persistence.mdx`
3. Create `concepts/networking.mdx`
4. Rewrite `introduction.mdx`
5. Rewrite `quickstart.mdx`

### Phase 2: Feature Pages (merge SDK content)
6. Rewrite `agents.mdx` (merge TS + Python agent pages)
7. Rewrite `running-commands.mdx` (merge TS + Python exec pages)
8. Rewrite `working-with-files.mdx` (merge TS + Python filesystem pages)
9. Create `interactive-terminals.mdx` (merge TS + Python PTY pages)
10. Rewrite `checkpoints.mdx` (merge TS + Python checkpoint pages)
11. Rewrite `templates.mdx` (merge TS + Python template pages)
12. Rewrite `patches.mdx` (merge TS + Python patch pages)
13. Rewrite `preview-urls.mdx` (new unified page)

### Phase 3: CLI + Support Pages
14. Rewrite `cli/overview.mdx`
15. Update `cli/sandbox.mdx`
16. Create `cli/exec.mdx` (rename from commands)
17. Update `cli/shell.mdx`
18. Create `cli/checkpoint.mdx` (merge checkpoint + patch)
19. Create `cli/preview.mdx` (rename from previews)
20. Create `troubleshooting.mdx`
21. Create `changelog.mdx` (stub)

### Phase 4: Cleanup
22. Update `guides/build-a-lovable-clone.mdx` (minor fixes)
23. Delete all `sdks/` pages
24. Delete obsolete CLI pages
25. Update `mint.json` with new navigation
26. Create `images/architecture.svg` (simple sandbox lifecycle diagram)

---

## Page Count Summary

| Section | Current | Proposed | Delta |
|---------|---------|----------|-------|
| Getting Started | 2 | 2 | 0 |
| Concepts | 0 | 3 | +3 |
| Features | 3 | 8 | +5 |
| CLI | 7 | 6 | -1 |
| Guides | 2 | 2 | 0 |
| Resources | 0 | 2 | +2 |
| SDK (tabs) | 16 | 0 | -16 |
| **Total** | **30** | **23** | **-7** |

Net result: 7 fewer pages, but more complete coverage. Zero duplication.

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
