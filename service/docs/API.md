# OpenSandbox Service API Documentation

## Overview

OpenSandbox Service provides on-demand sandbox VMs with instant filesystem snapshots. Authentication is service-level — one API key for all operations.

**Base URL:** `http://localhost:4000`

---

## Authentication

All endpoints (except `/health` and `/api-keys`) require a service-level API key in the `x-api-key` header.

```
x-api-key: ws_abc123def456...
```

---

## API Keys

### Create API Key

```
POST /api-keys
Content-Type: application/json

{
  "name": "my-app"  // optional
}
```

**Response (201):**
```json
{
  "id": 1,
  "key": "ws_a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0u1v2w3x4",
  "name": "my-app",
  "created_at": "2024-12-09T19:30:00",
  "message": "Save your API key - it won't be shown again!"
}
```

### List API Keys

```
GET /api-keys
```

**Response:**
```json
{
  "api_keys": [
    {
      "id": 1,
      "key": "ws_a1b2c3...",
      "name": "my-app",
      "created_at": "2024-12-09T19:30:00"
    }
  ]
}
```

---

## Health Check

```
GET /health
```

**Response:**
```json
{
  "status": "ok",
  "timestamp": "2024-12-09T19:30:00.000Z"
}
```

---

## VM Management

### List All VMs

```
GET /vms
x-api-key: <api_key>
```

**Response:**
```json
{
  "vms": [
    {
      "id": 1,
      "name": "my-sandbox",
      "base_vm": "dev-1",
      "status": "running",
      "agent_port": 3333,
      "ssh_port": 2222,
      "created_at": "2024-12-09T19:30:00",
      "updated_at": "2024-12-09T19:30:15"
    }
  ]
}
```

### Create VM

```
POST /vms
x-api-key: <api_key>
Content-Type: application/json

{
  "name": "my-sandbox"
}
```

**Response (201):**
```json
{
  "id": 1,
  "name": "my-sandbox",
  "base_vm": "dev-1",
  "status": "running",
  "agent_port": 3333,
  "ssh_port": 2222,
  "created_at": "2024-12-09T19:30:00",
  "updated_at": "2024-12-09T19:30:15"
}
```

### Get VM Info

```
GET /vms/:name
x-api-key: <api_key>
```

**Response:**
```json
{
  "id": 1,
  "name": "my-sandbox",
  "base_vm": "dev-1",
  "status": "running",
  "agent_port": 3333,
  "ssh_port": 2222,
  "created_at": "2024-12-09T19:30:00",
  "updated_at": "2024-12-09T19:30:15",
  "agentHealthy": true
}
```

### Delete VM

```
DELETE /vms/:name
x-api-key: <api_key>
```

**Response:**
```json
{
  "message": "VM \"my-sandbox\" destroyed"
}
```

---

## Command Execution

### Run Command

Execute a shell command in the VM's workspace. A Btrfs snapshot is automatically taken after each command.

```
POST /vms/:name/run
x-api-key: <api_key>
Content-Type: application/json

{
  "command": "echo 'Hello World'",
  "timeout": 30
}
```

**Response:**
```json
{
  "stdout": "Hello World\n",
  "stderr": "",
  "exitCode": 0,
  "snapshot": "cmd_1702147200000"
}
```

### Wipe Workspace

```
POST /vms/:name/wipe
x-api-key: <api_key>
```

**Response:**
```json
{
  "message": "Workspace wiped"
}
```

---

## Snapshots

### List Snapshots

```
GET /vms/:name/snapshots
x-api-key: <api_key>
```

**Response:**
```json
{
  "snapshots": [
    {
      "id": 1,
      "vm_name": "my-sandbox",
      "snapshot_name": "initial",
      "command": null,
      "created_at": "2024-12-09T19:30:15"
    },
    {
      "id": 2,
      "vm_name": "my-sandbox",
      "snapshot_name": "cmd_1702147200000",
      "command": "npm install express",
      "created_at": "2024-12-09T19:31:00"
    }
  ]
}
```

### Restore Snapshot

```
POST /vms/:name/snapshots/:snapshot/restore
x-api-key: <api_key>
```

**Response:**
```json
{
  "message": "Restored to snapshot \"cmd_1702147200000\""
}
```

---

## Error Responses

```json
{
  "error": "Description of what went wrong"
}
```

| Code | Meaning |
|------|---------|
| `400` | Bad Request |
| `401` | API key required |
| `403` | Invalid API key |
| `404` | Not found |
| `500` | Server error |

---

## Example: Full Workflow

```bash
# 1. Create an API key
curl -X POST http://localhost:4000/api-keys \
  -H "Content-Type: application/json" \
  -d '{"name": "demo"}'
# Save the key!

# 2. Create a sandbox
curl -X POST http://localhost:4000/vms \
  -H "Content-Type: application/json" \
  -H "x-api-key: ws_your_key" \
  -d '{"name": "test"}'

# 3. Run commands
curl -X POST http://localhost:4000/vms/test/run \
  -H "Content-Type: application/json" \
  -H "x-api-key: ws_your_key" \
  -d '{"command": "echo hello > file.txt"}'

# 4. List snapshots
curl http://localhost:4000/vms/test/snapshots \
  -H "x-api-key: ws_your_key"

# 5. Restore to initial state
curl -X POST http://localhost:4000/vms/test/snapshots/initial/restore \
  -H "x-api-key: ws_your_key"

# 6. Cleanup
curl -X DELETE http://localhost:4000/vms/test \
  -H "x-api-key: ws_your_key"
```

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | 4000 | API server port |
| `BASE_VM` | dev-1 | VirtualBox VM to clone from |
| `VM_USER` | user | SSH username for VMs |
| `VM_PASSWORD` | root | SSH password for VMs |
| `DB_PATH` | ./vms.db | SQLite database path |
