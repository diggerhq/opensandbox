---
name: opencomputer
description: Manage OpenComputer cloud sandboxes. Use when the user wants to create, run commands in, checkpoint, or manage sandbox environments. Auto-invokes when sandboxes, remote environments, or the oc CLI are mentioned.
allowed-tools: Bash(oc *), Bash(which oc), Read, Grep, Glob
---

You have access to the `oc` CLI for managing OpenComputer cloud sandboxes. Use it to create sandboxes, execute commands, manage checkpoints, and more.

## Prerequisites

The `oc` CLI must be installed and configured. Check with:
```
which oc
```

If not installed, download the latest binary from GitHub Releases:
```bash
# macOS (Apple Silicon)
curl -fsSL https://github.com/diggerhq/opencomputer/releases/latest/download/oc-darwin-arm64 -o /usr/local/bin/oc && chmod +x /usr/local/bin/oc

# macOS (Intel)
curl -fsSL https://github.com/diggerhq/opencomputer/releases/latest/download/oc-darwin-amd64 -o /usr/local/bin/oc && chmod +x /usr/local/bin/oc

# Linux (x86_64)
curl -fsSL https://github.com/diggerhq/opencomputer/releases/latest/download/oc-linux-amd64 -o /usr/local/bin/oc && chmod +x /usr/local/bin/oc

# Linux (ARM64)
curl -fsSL https://github.com/diggerhq/opencomputer/releases/latest/download/oc-linux-arm64 -o /usr/local/bin/oc && chmod +x /usr/local/bin/oc
```

Configuration requires an API key. Set it via:
```
oc config set api-key <key>
```
Or pass `--api-key` on every command. The API URL defaults to `https://app.opencomputer.dev`.

## CLI Reference

### Sandbox Lifecycle

```bash
# Create a sandbox (returns sandbox ID)
oc sandbox create --template base --timeout 300 --cpu 1 --memory 512
oc sandbox create --env KEY=VALUE --env KEY2=VALUE2
oc create  # shortcut

# List running sandboxes
oc sandbox list
oc ls  # shortcut

# Get sandbox details
oc sandbox get <sandbox-id>

# Kill a sandbox
oc sandbox kill <sandbox-id>

# Hibernate (saves state to S3, can wake later)
oc sandbox hibernate <sandbox-id>

# Wake a hibernated sandbox
oc sandbox wake <sandbox-id> --timeout 300

# Update timeout
oc sandbox set-timeout <sandbox-id> <seconds>
```

### Execute Commands

```bash
# Run a command in a sandbox
oc exec <sandbox-id> -- echo hello
oc exec <sandbox-id> --cwd /app -- npm install
oc exec <sandbox-id> --timeout 120 -- make build
oc exec <sandbox-id> --env NODE_ENV=production -- node server.js

# JSON output includes exitCode, stdout, stderr
oc exec <sandbox-id> --json -- whoami
```

### Checkpoints

Checkpoints snapshot a running sandbox. You can restore to a checkpoint (in-place revert) or spawn new sandboxes from one (fork).

```bash
# Create a checkpoint
oc checkpoint create <sandbox-id> --name "after-setup"

# List checkpoints
oc checkpoint list <sandbox-id>

# Restore sandbox to a checkpoint (in-place revert)
oc checkpoint restore <sandbox-id> <checkpoint-id>

# Spawn a new sandbox from a checkpoint (fork)
oc checkpoint spawn <checkpoint-id> --timeout 300

# Delete a checkpoint
oc checkpoint delete <sandbox-id> <checkpoint-id>
```

### Checkpoint Patches

Patches are scripts applied when sandboxes are spawned from a checkpoint. Use them to customize forked environments.

```bash
# Create a patch (reads script from file)
oc patch create <checkpoint-id> --script ./setup.sh --description "Install deps"

# Create a patch from stdin
echo "apt install -y curl" | oc patch create <checkpoint-id> --script -

# List patches
oc patch list <checkpoint-id>

# Delete a patch
oc patch delete <checkpoint-id> <patch-id>
```

### Preview URLs

Expose a sandbox port via a public URL.

```bash
# Create a preview URL
oc preview create <sandbox-id> --port 3000
oc preview create <sandbox-id> --port 8080 --domain myapp.example.com

# List preview URLs
oc preview list <sandbox-id>

# Delete a preview URL
oc preview delete <sandbox-id> <port>
```

### Interactive Shell

```bash
# Open an interactive terminal session
oc shell <sandbox-id>
oc shell <sandbox-id> --shell /bin/zsh
```

### Global Flags

All commands support:
- `--json` — output as JSON instead of tables
- `--api-key <key>` — override API key
- `--api-url <url>` — override API URL

## Workflow Patterns

### Create and use a sandbox
```bash
ID=$(oc create --json | jq -r '.sandboxID')
oc exec $ID -- apt update && apt install -y nodejs
oc exec $ID -- node -e "console.log('hello')"
oc sandbox kill $ID
```

### Checkpoint workflow (setup once, fork many)
```bash
# Create base environment
ID=$(oc create --json | jq -r '.sandboxID')
oc exec $ID -- apt update && apt install -y python3 pip
oc exec $ID -- pip install flask

# Checkpoint it
CP=$(oc checkpoint create $ID --name "python-flask" --json | jq -r '.id')
# Wait for checkpoint to be ready
oc checkpoint list $ID

# Spawn copies from the checkpoint
FORK1=$(oc checkpoint spawn $CP --json | jq -r '.sandboxID')
FORK2=$(oc checkpoint spawn $CP --json | jq -r '.sandboxID')
```

### Add a patch to customize forks
```bash
oc patch create $CP --script ./inject-config.sh --description "Add app config"
# All future spawns from $CP will run inject-config.sh on boot
```

## Important Notes

- Always use `--json` and parse with `jq` when you need to extract IDs or fields programmatically.
- Sandbox IDs look like `sb-xxxxxxxx`. Checkpoint IDs are UUIDs.
- Checkpoints take a few seconds to become `ready`. Poll with `oc checkpoint list` if needed.
- Use `oc sandbox kill` to clean up sandboxes when done.
- The `oc exec` command exits with the remote process exit code.
