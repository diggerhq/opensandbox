---
name: openqemu
description: Create and interact with OpenSandbox QEMU sandboxes — create, exec, read/write files, list, destroy, hibernate/wake, checkpoint/fork, scale resources
disable-model-invocation: false
user-invocable: true
allowed-tools: Bash, Read, Write
---

# OpenSandbox Skill

You have access to OpenSandbox, a cloud sandbox environment running QEMU VMs. Use it to run code, test commands, and manipulate files in isolated Linux environments.

## Configuration

```
API_URL=${OPENSANDBOX_API_URL:-http://localhost:8080}
API_KEY=${OPENSANDBOX_API_KEY}
```

Set `OPENSANDBOX_API_URL` and `OPENSANDBOX_API_KEY` environment variables before using.
All requests require `-H 'X-API-Key: $API_KEY'` and `-H 'Content-Type: application/json'`.

## Available Operations

### Create a sandbox
```bash
curl -s -X POST $API_URL/api/sandboxes \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $API_KEY" \
  -d '{"timeout":3600}'
```
Returns JSON with `sandboxID`, `status`, `token`.

### Run a command (fire-and-forget, returns stdout/stderr)
```bash
curl -s -X POST $API_URL/api/sandboxes/{SANDBOX_ID}/exec/run \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $API_KEY" \
  -d '{"cmd":"python3","args":["-c","print(1+1)"],"timeout":30}'
```
Returns JSON with `exitCode`, `stdout`, `stderr`. This is the simplest way to run commands.

### Create an exec session (long-lived, for streaming)
```bash
curl -s -X POST $API_URL/api/sandboxes/{SANDBOX_ID}/exec \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $API_KEY" \
  -d '{"cmd":"bash","args":["-c","echo hello"]}'
```

### Read a file
```bash
curl -s $API_URL/api/sandboxes/{SANDBOX_ID}/files?path=/etc/os-release \
  -H "X-API-Key: $API_KEY"
```

### Write a file
```bash
curl -s -X PUT "$API_URL/api/sandboxes/{SANDBOX_ID}/files?path=/tmp/test.py" \
  -H "X-API-Key: $API_KEY" \
  -H 'Content-Type: application/octet-stream' \
  --data-binary 'print("hello world")'
```

### List directory
```bash
curl -s "$API_URL/api/sandboxes/{SANDBOX_ID}/files/list?path=/workspace" \
  -H "X-API-Key: $API_KEY"
```

### List sandboxes
```bash
curl -s $API_URL/api/sandboxes \
  -H "X-API-Key: $API_KEY"
```

### Destroy a sandbox
```bash
curl -s -X DELETE $API_URL/api/sandboxes/{SANDBOX_ID} \
  -H "X-API-Key: $API_KEY"
```

### Hibernate / Wake
```bash
# Hibernate (saves state to S3, stops billing)
curl -s -X POST $API_URL/api/sandboxes/{SANDBOX_ID}/hibernate \
  -H "X-API-Key: $API_KEY"

# Wake (golden restore ~520ms, workspace + rootfs preserved)
curl -s -X POST $API_URL/api/sandboxes/{SANDBOX_ID}/wake \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $API_KEY" \
  -d '{"timeout":300}'
```

### Checkpoint / Fork
```bash
# Create checkpoint (savevm snapshot inside qcow2)
curl -s -X POST $API_URL/api/sandboxes/{SANDBOX_ID}/checkpoints \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $API_KEY" \
  -d '{"name":"my-checkpoint"}'
# Returns JSON with `id`, `status` (processing → ready)

# Fork a new sandbox from a checkpoint
curl -s -X POST $API_URL/api/sandboxes/from-checkpoint/{CHECKPOINT_ID} \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $API_KEY" \
  -d '{"timeout":3600}'
# Returns JSON with `sandboxID` (status: creating → running)
```

### Resource Scaling

Scale sandbox resources at runtime. Memory hotplug adds real RAM to the VM (not just cgroup limits).
Pricing model: 1 vCPU per 1GB RAM (linear scaling). Setting memoryMB auto-calculates CPU.

#### External API (from your client/SDK)
```bash
# Scale up to 2GB RAM + 2 vCPUs
curl -s -X PUT $API_URL/api/sandboxes/{SANDBOX_ID}/limits \
  -H 'Content-Type: application/json' \
  -H "X-API-Key: $API_KEY" \
  -d '{"maxMemoryMB": 2048, "cpuPercent": 200, "maxPids": 256}'
```

#### Internal API (from inside the sandbox — 169.254.169.254)
```bash
# From inside the sandbox:
curl -s http://169.254.169.254/v1/status
# → {"sandboxId":"sb-xxx","uptime":42.5}

curl -s http://169.254.169.254/v1/limits
# → {"cpuPercent":100,"memUsage":52428800,"memLimit":907051008,"pids":3}

curl -s -X POST http://169.254.169.254/v1/scale -d '{"memoryMB": 4096}'
# → {"ok":true,"memoryMB":4096,"cpuPercent":400,"maxPids":0}
# Auto-calculates: 4GB RAM → 400% CPU (4 vCPUs)

curl -s http://169.254.169.254/v1/metadata
# → {"region":"use2","template":"default"}
```

#### Default cgroup limits
- `pids.max = 128` (prevents fork bombs)
- `memory.max = 90% of VM RAM`
- `cpu.max = 80% of vCPUs` (reserves 20% for agent)
- Fork bomb protection: cgroup kill on exec timeout

## Architecture

- **Agent communication**: virtio-serial (survives QEMU migration, enables sub-second wake)
- **Storage**: rootfs qcow2 (COW overlay on base image) + workspace qcow2 (separate /workspace drive)
- **Persistence**: rootfs installs (apt/pip) AND /workspace files survive hibernate/wake. Only /tmp (tmpfs) is lost.
- **Resource isolation**: cgroup v2 inside guest — agent (PID 1) in root cgroup, user processes in /sandbox cgroup
- **Fork bomb protection**: pids.max + cpu.max + cgroup kill on exec timeout

## Workflow

When the user asks you to use a sandbox, or asks $ARGUMENTS:

1. **Create a sandbox** if one isn't already active in this conversation. Save the `sandboxID`. Always use `"timeout":3600` (1 hour idle timeout).
2. **Use `exec/run`** for commands that produce output — it returns stdout/stderr directly.
3. **Use `files` API** for reading/writing files in the sandbox.
4. **Clean up** — destroy the sandbox when done, or let it time out (1 hour of inactivity).

## Tips

- Sandbox VMs run Ubuntu with python3, git, curl, jq, and standard Linux tools.
- Default timeout is 300s (5 min). Pass `"timeout":3600` for longer sessions.
- The `exec/run` endpoint is synchronous — it waits for the command to finish and returns the result. Use this for most operations.
- For exec/run, the JSON field for the command binary is `cmd` (not `command`), and arguments go in `args`.
- Environment variables can be passed as `"envs":{"KEY":"VALUE"}`.
- Working directory can be set with `"cwd":"/path"`.
- **Background processes**: Use `setsid cmd </dev/null >/dev/null 2>&1 &` in one exec, then interact in a separate exec.
- **Persistence**: `/workspace/` and rootfs changes (apt install, pip install) survive hibernate/wake. `/tmp/` does not (tmpfs).
- **Checkpoints**: Use `POST /checkpoints` to snapshot, then `POST /from-checkpoint/{id}` to fork. Wait for fork status to become "running" before executing commands.
- **Pricing model**: 1 vCPU per 1GB RAM (linear scaling, 256MB–16GB). Per-second billing. Hibernated VMs are free.
