# OpenComputer Docs Rewrite Plan

## Guiding Principles

1. **Single sidebar, no tabs.** Every page lives in one navigation tree.
2. **Entity-first.** Pages organized around things the user encounters (sandboxes, agents, checkpoints, templates), not by SDK or abstract category. Each entity page should be self-contained conceptually: what it is, when to use it, and the common workflows.
3. **Three-tab examples.** Every code example wraps in tabs: TypeScript / Python / HTTP API (where applicable). The user picks their preferred surface once and sees it everywhere. Some examples are SDK-only (no HTTP equivalent for streaming); some are HTTP-only (auth headers). Use judgement — include a tab only when it adds value.
4. **Quality over quantity.** If it can be said in fewer words, it should be. No filler sections. Every page earns its place.
5. **Entity → Example → API At A Glance** flow on each page. Open with what the entity *is* (2-3 sentences), show a working code example, then summarize the methods most readers need before linking down to exhaustive reference.
6. **Code-forward.** The first thing on every entity page (after the short explanation) should be a working code example. Parameters and types come after.
7. **Reference pages are contract-authoritative.** The Agents/Sandboxes pages teach with curated examples and stable behavioral guidance. The Reference pages document exact endpoints, methods, types, parameters, flags, and payloads.
8. **Contracts come from code, not guesswork.** Do not hand-spec request/response fields, status enums, defaults, or CLI flags unless they were checked against the current handlers, SDK types, and command source.
9. **Honest about gaps.** Don't document features that don't exist yet. Mark experimental/beta features clearly.

---

## Pre-Rewrite State (for context)

### What existed (30 .mdx files)
- Introduction + Quickstart (solid onboarding)
- 3 feature pages: Agents, Running Commands, Working with Files
- 8 TypeScript SDK pages (separate tab)
- 8 Python SDK pages (separate tab, mirrors TS)
- 7 CLI reference pages (separate tab)
- 2 guides (Lovable clone, Agent Skill)

### Key problems (why we're rewriting)
1. **Tab separation creates duplication.** "Running Commands" exists as a feature page, a TS SDK page, a Python SDK page, and a CLI page. Four places for one concept.
2. **No conceptual foundation.** Docs jump straight to API calls without explaining what a sandbox *is*, its lifecycle, resource model, or how persistence works.
3. **Missing critical content.** No connection-model explanation (control plane vs worker), no error reference, no troubleshooting, no architecture overview.
4. **SDK/code gaps.** `resume` in Agent sessions, `maxRunAfterDisconnect` in exec, hibernation semantics, preview URL domain verification — all in code but undocumented.
5. **Inconsistent API naming between SDKs.** `sandbox.exec` vs `sandbox.commands` (deprecated alias still used in Python quickstart examples).

---

## Structure

### File tree

```
docs/
├── mint.json
├── images/
│
│── introduction.mdx               ← REWRITE
│── quickstart.mdx                 ← REWRITE
│── how-it-works.mdx               ← NEW
│
├── agents/
│   ├── overview.mdx               ← REWRITE (entity page)
│   ├── events.mdx                 ← NEW
│   ├── tools.mdx                  ← NEW
│   └── multi-turn.mdx             ← NEW
│
├── sandboxes/
│   ├── overview.mdx               ← NEW (entity page)
│   ├── running-commands.mdx       ← REWRITE
│   ├── working-with-files.mdx     ← REWRITE
│   ├── interactive-terminals.mdx  ← NEW
│   ├── checkpoints.mdx            ← REWRITE (entity page)
│   ├── templates.mdx              ← REWRITE (entity page)
│   ├── patches.mdx                ← REWRITE (entity page)
│   └── preview-urls.mdx           ← NEW (entity page)
│
├── cli/                            ← Guide-like pages
│   ├── overview.mdx               ← REWRITE
│   ├── sandbox.mdx                ← REWRITE
│   ├── exec.mdx                   ← REWRITE from commands.mdx
│   ├── shell.mdx                  ← REWRITE
│   ├── checkpoint.mdx             ← REWRITE from checkpoints.mdx
│   ├── patch.mdx                  ← REWRITE from patches.mdx
│   └── preview.mdx                ← REWRITE from previews.mdx
│
├── reference/                      ← Exhaustive lookup pages
│   ├── api.mdx                    ← NEW
│   ├── typescript-sdk.mdx         ← NEW
│   ├── python-sdk.mdx             ← NEW
│   └── cli.mdx                    ← NEW
│
├── guides/
│   ├── build-a-lovable-clone.mdx  ← KEEP (minor edits)
│   └── agent-skill.mdx            ← KEEP
│
│── troubleshooting.mdx            ← NEW
│
├── sdks/                           ← DELETE entire directory (content merged into entity pages)
```

### mint.json Navigation

```json
{
  "tabs": [],
  "navigation": [
    {
      "group": "Getting Started",
      "pages": ["introduction", "quickstart", "how-it-works"]
    },
    {
      "group": "Agents",
      "pages": ["agents/overview", "agents/events", "agents/tools", "agents/multi-turn"]
    },
    {
      "group": "Sandboxes",
      "pages": [
        "sandboxes/overview", "sandboxes/running-commands", "sandboxes/working-with-files",
        "sandboxes/interactive-terminals", "sandboxes/checkpoints", "sandboxes/templates",
        "sandboxes/patches", "sandboxes/preview-urls"
      ]
    },
    {
      "group": "CLI",
      "pages": ["cli/overview", "cli/sandbox", "cli/exec", "cli/shell", "cli/checkpoint", "cli/patch", "cli/preview"]
    },
    {
      "group": "Guides",
      "pages": ["guides/build-a-lovable-clone", "guides/agent-skill"]
    },
    {
      "group": "Reference",
      "pages": ["reference/api", "reference/typescript-sdk", "reference/python-sdk", "reference/cli"]
    },
    {
      "group": "Resources",
      "pages": ["troubleshooting"]
    }
  ]
}
```

### Why this structure

- **Agents first.** This is the headline feature — most users land here to run Claude inside sandboxes.
- **Agents get depth.** Four pages: overview, events, tools, multi-turn. Each covers a distinct concern.
- **Sandboxes group** contains the sandbox entity page plus everything you do with a sandbox.
- **CLI group** has guide-like pages (install, config, key workflows per topic). Each teaches patterns; the exhaustive flag reference lives in `reference/cli.mdx`.
- **Reference is second-to-last.** Lookup-oriented — most users read entity/CLI pages first and only reach for Reference when they need exact signatures.
- **Directory = group.** Folder structure mirrors navigation. No orphan files at the root (except Getting Started and Resources).

---

## Page Patterns

Per-page specifications live in each page stub as TODO comments.
The page stubs are the **source of truth** for page-level content decisions.

### Entity page template

Each entity page follows this pattern:
1. **What is this** — 2-3 sentences explaining the entity
2. **Primary code example** in tabs (TypeScript / Python / HTTP API where applicable)
3. **API at a glance** — the common methods, parameters, and caveats
4. **Additional examples** (also tabbed where applicable)
5. **Cross-links** — `<Tip>` at the bottom with CLI equivalent + Reference page links

### Contract discipline

- **Reference pages own exact contracts.** Request bodies, response shapes, enum values, CLI flags, and route availability live in `reference/*`.
- **Entity and CLI guide pages stay curated.** They explain workflows, tradeoffs, and the few methods/flags most readers need, but they should not mirror exhaustive contract detail.
- **Document surface differences explicitly.** When behavior differs across TS / Python / HTTP / CLI, call it out with availability notes instead of forcing parity in prose.
- **Explain the connection model centrally.** `how-it-works` and `reference/api` must both cover control plane vs worker-direct access, `connectURL`, JWT auth, and worker-only operations.

### Cross-linking pattern

Entity pages link down to CLI guide + Reference. CLI guide pages link to the SDK entity page + `reference/cli.mdx`. This creates a navigable triangle: entity page ↔ CLI guide ↔ reference. A reader never hits a dead end.

### Reference pages

Exhaustive, lookup-oriented. No tutorials, no "why" — every endpoint/method/type/flag with parameters, return types, and a minimal example. Four pages: HTTP API, TypeScript SDK, Python SDK, CLI.

### CLI guide pages

Two-tier model:
1. **CLI nav group** — 7 guide-like pages covering install, config, and key workflows per topic.
2. **Reference nav group** — one `reference/cli.mdx` page with every command, subcommand, and flag.

Each CLI guide page ends with a `<Tip>` linking to the SDK entity page + `reference/cli.mdx`.

### Migration discipline

- Remove or redirect the replaced page in the same change that lands its replacement. Do not keep old and new canonical pages alive in parallel longer than necessary.
- Add migration callouts when the public story changed (`sandbox.commands` -> `sandbox.exec`, legacy `/commands`, `base` vs `default`, CLI renames).

---

## Pages to Delete

These old pages are fully merged into the new structure and should be removed in Phase 6:

```
# SDK tab pages (16 → merged into agents/ and sandboxes/ pages)
sdks/typescript/overview.mdx     → introduction.mdx install section
sdks/typescript/sandbox.mdx      → sandboxes/overview.mdx
sdks/typescript/commands.mdx     → sandboxes/running-commands.mdx
sdks/typescript/filesystem.mdx   → sandboxes/working-with-files.mdx
sdks/typescript/pty.mdx          → sandboxes/interactive-terminals.mdx
sdks/typescript/templates.mdx    → sandboxes/templates.mdx
sdks/typescript/checkpoints.mdx  → sandboxes/checkpoints.mdx
sdks/typescript/patches.mdx      → sandboxes/patches.mdx
sdks/python/overview.mdx         → (same mapping as TS above)
sdks/python/sandbox.mdx
sdks/python/commands.mdx
sdks/python/filesystem.mdx
sdks/python/pty.mdx
sdks/python/templates.mdx
sdks/python/checkpoints.mdx
sdks/python/patches.mdx

# Old root-level feature pages (3)
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

Cross-referenced against TS SDK, Python SDK, HTTP API handlers, and CLI source code. All findings have been folded into page stubs. This section is a reference for decisions made.

### SDK parity gaps

The Python SDK is **not feature-equivalent** to TypeScript. Key gaps:
- `hibernate`/`wake` — TS only
- `cpuCount`/`memoryMB` on create — TS only
- `exec.start` streaming (callbacks, ExecSession) — TS only
- `exec.attach` — TS only
- Agent `resume` — TS only
- `onExit`/`onScrollbackEnd` callbacks — TS only

Python-unique features: `collect_events`, `wait`, `recv`, context manager.

### Naming conventions across surfaces

- TS `Templates` (plural) vs Python `Template` (singular) — both standalone, not on Sandbox
- TS camelCase vs Python snake_case vs HTTP mixed-case (`sandboxID`, `checkpointID`)
- `CheckpointInfo` fields differ per surface — TS has `sandboxConfig` + `orgId`, HTTP includes `sizeBytes`, Python returns raw dict

### Intentionally omitted from docs

| Item | Reason |
|------|--------|
| `alias` param on sandbox create | Declared in types but unused |
| `networkEnabled` param | Declared but no-op — all sandboxes have networking |
| `imageRef`, `templateRootfsKey`, `templateWorkspaceKey` | Internal plumbing |
| `port` on sandbox create | Internal routing default |
| `commands.py` legacy file in Python SDK | Not exported, superseded by `exec.py` |
| Dashboard-only routes (`/api/dashboard/*`) | Separate auth model, not public API |

### Items folded into page stubs

| Item | Where |
|------|-------|
| `saveAsTemplate` | templates page (dashboard feature) |
| `POST /sandboxes/:id/token/refresh` | reference/api.mdx auth section |
| PTY `shell` param | reference/api.mdx PTY section |
| PTY resize (HTTP only, no SDK) | reference/api.mdx + sandboxes/interactive-terminals.mdx |
| Agent events are SDK-abstracted | reference/api.mdx agent section |
| `ExecSessionInfo.attachedClients` | both SDK reference pages |

---

## Execution Order

Treat this as an operating model, not a rigid serial checklist.

### Build Workflow

- **Stub first.** If the plan and a page stub disagree, the stub wins.
- **Read source, then write.** For every page, read the stub plus the authoritative code paths listed in the Source Map before drafting.
- **Reference-first for contracts.** If a page depends on exact payloads, flags, or method signatures, update the relevant `reference/*` section in the same change.
- **Replace, don't accumulate.** When a new page supersedes an old one, remove or redirect the old page in the same batch.
- **Write in shippable batches.** Prefer landing one coherent cluster at a time rather than scattering partial edits across the tree.
- **Self-audit before done.** Run the Quality Bar and cross-link checks before considering a page finished.

### Priority Bands

These are priority bands, not a strict sequence. Within a band, order can change based on momentum or blockers.

#### Band A: Foundations

- `how-it-works.mdx`
- `reference/api.mdx`
- `reference/cli.mdx`
- `agents/overview.mdx`
- `sandboxes/overview.mdx`

#### Band B: Contract-Heavy Entity Pages

- `agents/events.mdx`
- `agents/tools.mdx`
- `agents/multi-turn.mdx`
- `sandboxes/running-commands.mdx`
- `sandboxes/working-with-files.mdx`
- `sandboxes/interactive-terminals.mdx`
- `sandboxes/checkpoints.mdx`
- `sandboxes/templates.mdx`
- `sandboxes/patches.mdx`
- `sandboxes/preview-urls.mdx`
- `reference/typescript-sdk.mdx`
- `reference/python-sdk.mdx`

#### Band C: Onboarding, CLI Guides, and Cleanup

- `introduction.mdx`
- `quickstart.mdx`
- `cli/overview.mdx`
- `cli/sandbox.mdx`
- `cli/exec.mdx`
- `cli/shell.mdx`
- `cli/checkpoint.mdx`
- `cli/patch.mdx`
- `cli/preview.mdx`
- `troubleshooting.mdx`
- `guides/build-a-lovable-clone.mdx` (minor fixes)
- Delete old `sdks/` pages
- Delete old root-level pages (`agents.mdx`, `running-commands.mdx`, `working-with-files.mdx`)
- Delete obsolete CLI pages (`commands.mdx`, `checkpoints.mdx`, `patches.mdx`, `previews.mdx`)

---

## Source Map

Use these as the primary code sources when writing each page family.

### `reference/api.mdx`

- `internal/api/router.go`
- `internal/api/sandbox.go`
- `internal/api/exec_session.go`
- `internal/api/agent_session.go`
- `internal/api/filesystem.go`
- `internal/api/pty.go`
- `internal/api/templates.go`
- `internal/worker/http_server.go`

### `agents/*`

- `sdks/typescript/src/agent.ts`
- `sdks/python/opencomputer/agent.py`
- `scripts/claude-agent-wrapper/index.ts`

### `sandboxes/*`

- `sdks/typescript/src/sandbox.ts`
- `sdks/python/opencomputer/sandbox.py`
- `sdks/typescript/src/exec.ts`
- `sdks/python/opencomputer/exec.py`
- `sdks/typescript/src/filesystem.ts`
- `sdks/python/opencomputer/filesystem.py`
- `sdks/typescript/src/pty.ts`
- `sdks/python/opencomputer/pty.py`
- `internal/firecracker/manager.go`
- `internal/firecracker/snapshot.go`

### `templates`

- `sdks/typescript/src/template.ts`
- `sdks/python/opencomputer/template.py`
- `deploy/firecracker/rootfs/Dockerfile.default`
- `internal/api/templates.go`
- `internal/db/store.go`

### `cli/*` and `reference/cli.mdx`

- `cmd/oc/internal/commands/*`

### Cross-checking real-world usage

Use downstream consumers when you need to understand how the public surface is actually being used:

- `../agents-api/`
- `../base360-checkin-agent/`

---

## Open Decisions / Blockers

The writing agent should not rediscover these from scratch while drafting.

- **Connection model must be explained centrally.** Hosted usage spans control plane and worker-direct surfaces (`connectURL`, JWT auth, token refresh, worker-only operations).
- **Python template API ergonomics are awkward.** If there is no public construction story worth documenting, make template docs HTTP-first or improve the SDK before documenting it as a first-class Python surface.
- **`default` vs `base` needs a single public story.** Docs should choose one canonical framing and treat the other as legacy/backward-compatibility.
- **Do not over-promise exact restore semantics.** Hibernation/checkpoint/fork pages must avoid blanket “exactly where you left off” language when cold-boot fallback exists.
- **Dashboard-only features need deliberate placement.** `saveAsTemplate` and `/api/dashboard/*` should only be surfaced where that distinction is explicit.

---

## Quality Bar

Each page must pass these checks before shipping:
- [ ] Opens with a working code example (not prose)
- [ ] Code examples use tabs (TypeScript / Python / HTTP API) where applicable
- [ ] HTTP API tab included for operations that map cleanly to a single endpoint
- [ ] Streaming/WebSocket operations can omit HTTP tab (SDK-only is fine)
- [ ] No deprecated API names (`commands` → `exec`)
- [ ] All parameters documented match actual SDK source code (verified per-SDK, not assumed identical)
- [ ] Exact API field casing matches the live handlers/types (`sessionID`, `sandboxID`, `text`, `cmd`, etc.)
- [ ] TS-only features clearly marked — no Python tab that shows nonexistent API
- [ ] Python-unique features (context manager, collect_events, recv) documented where relevant
- [ ] No invented SDK convenience APIs or constructors
- [ ] Defaults/specs are only documented when they are true product guarantees, not template contents or deployment config
- [ ] No "coming soon" for features that now exist
- [ ] No filler sentences ("In this section we will..." — just do it)
- [ ] Entity pages: `<Tip>` at bottom linking to CLI guide (if applicable) + Reference pages (TS SDK, Python SDK, HTTP API)
- [ ] CLI guide pages: `<Tip>` at bottom linking to SDK entity page + reference/cli.mdx section
- [ ] If terminology or workflow changed publicly (`commands` → `exec`, CLI rename, `base` vs `default`), add a migration note where readers will expect it
