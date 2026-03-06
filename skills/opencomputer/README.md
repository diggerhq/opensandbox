# OpenComputer Skill

Give your AI agent the ability to create and manage cloud sandboxes. Works with Claude Code, Codex, Cursor, and 30+ other AI agents that support the [Agent Skills](https://agentskills.io) standard.

## Install

```bash
npx skills add diggerhq/opencomputer
```

## What it does

Once installed, your AI agent can:

- Create and manage cloud sandboxes (`oc create`, `oc sandbox list`, `oc sandbox kill`)
- Execute commands inside sandboxes (`oc exec <id> -- <command>`)
- Open interactive shells (`oc shell <id>`)
- Snapshot and fork sandboxes with checkpoints (`oc checkpoint create/restore/spawn`)
- Apply patches to checkpoints (`oc patch create`)
- Expose sandbox ports via preview URLs (`oc preview create`)

The skill auto-activates when you mention sandboxes, remote environments, or the `oc` CLI.

## Prerequisites

The `oc` CLI must be installed. The skill will guide your agent to install it automatically, or you can install it manually:

```bash
# macOS (Apple Silicon)
curl -fsSL https://github.com/diggerhq/opencomputer/releases/latest/download/oc-darwin-arm64 -o /usr/local/bin/oc
chmod +x /usr/local/bin/oc

# macOS (Intel)
curl -fsSL https://github.com/diggerhq/opencomputer/releases/latest/download/oc-darwin-amd64 -o /usr/local/bin/oc
chmod +x /usr/local/bin/oc

# Linux (x86_64)
curl -fsSL https://github.com/diggerhq/opencomputer/releases/latest/download/oc-linux-amd64 -o /usr/local/bin/oc
chmod +x /usr/local/bin/oc

# Linux (ARM64)
curl -fsSL https://github.com/diggerhq/opencomputer/releases/latest/download/oc-linux-arm64 -o /usr/local/bin/oc
chmod +x /usr/local/bin/oc
```

Then configure your API key:

```bash
oc config set api-key YOUR_API_KEY
```

Get your API key at [app.opencomputer.dev](https://app.opencomputer.dev).

## Example usage

Once installed, just ask your agent naturally:

- "Create a sandbox and install Node.js in it"
- "Run my test suite in a sandbox"
- "Checkpoint this sandbox so I can fork it later"
- "Open a shell to the sandbox"

## Learn more

- [CLI Reference](https://docs.opencomputer.dev/cli/overview)
- [TypeScript SDK](https://docs.opencomputer.dev/sdks/typescript/overview)
- [Python SDK](https://docs.opencomputer.dev/sdks/python/overview)
- [OpenComputer website](https://opencomputer.dev)
