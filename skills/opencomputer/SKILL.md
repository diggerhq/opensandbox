---
name: opencomputer
description: Manage OpenComputer cloud sandboxes. Use when the user wants to create, run commands in, checkpoint, or manage sandbox environments. Auto-invokes when sandboxes, remote environments, or the oc CLI are mentioned.
allowed-tools: Bash(oc *), Bash(which oc), Bash(curl *), Bash(open *), Bash(xdg-open *), Bash(chmod *), Read, Grep, Glob
---

You have access to the `oc` CLI for managing OpenComputer cloud sandboxes. Use it to create sandboxes, execute commands, manage checkpoints, and more.

## First-time setup (run this BEFORE any other `oc` command)

Before running any sandbox command, make sure the user is set up. Do these checks in order, only fixing what's broken:

### 1. Is the `oc` CLI installed?

```bash
which oc
```

If it returns a path → installed, skip to step 2.

If it returns nothing / non-zero exit → install it with the official one-liner:

```bash
curl -fsSL https://raw.githubusercontent.com/diggerhq/opencomputer/main/scripts/install.sh | bash
```

This installs `oc` to `~/.local/bin/oc`. If `~/.local/bin` is not on the user's PATH, tell them to add `export PATH="$HOME/.local/bin:$PATH"` to their shell rc file, and use the full path `~/.local/bin/oc` for the rest of this session.

### 2. Is the user logged in (API key configured)?

```bash
oc config show
```

If `API Key` is shown (even masked) → logged in, you're done.

If no API key is set, OR a sandbox command fails with an auth error (`401`, "unauthorized", "missing API key"):

1. Open the OpenComputer dashboard in the user's browser so they can create a key:
   - macOS: `open https://app.opencomputer.dev`
   - Linux: `xdg-open https://app.opencomputer.dev`
2. Tell the user (in chat) to:
   - Sign in / sign up at the page that just opened
   - Create an API key
   - Run this command in their terminal once they have the key:
     ```bash
     oc config set api-key YOUR_API_KEY
     ```
3. Wait for the user to confirm they've set the key before proceeding.

Do **not** prompt them for the key in the chat — they should paste it into their own terminal so it never leaves their machine.

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
