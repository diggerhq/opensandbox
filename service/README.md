# OpenSandbox Service

Multi-runtime sandbox orchestrator with instant Btrfs snapshots.

## Quick Start

```bash
npm install
npm run dev
```

## Environment Variables

### Runtime Selection

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_RUNTIME` | `virtualbox` | Which runtime to use: `docker`, `virtualbox`, or `firecracker` |

### Docker Runtime

| Variable | Default | Description |
|----------|---------|-------------|
| `DOCKER_IMAGE` | `ubuntu:22.04` | Base Docker image |
| `DOCKER_BTRFS_PATH` | `/var/lib/sandbox-btrfs` | Path for Btrfs-backed storage |
| `DOCKER_NETWORK` | `none` | Docker network mode |

### VirtualBox Runtime

| Variable | Default | Description |
|----------|---------|-------------|
| `VBOX_BASE_VM` | `dev-1` | Name of the base VirtualBox VM |
| `VBOX_BASE_SNAPSHOT` | `base` | Name of the clean snapshot to clone from |

### Firecracker Runtime (Linux only)

| Variable | Default | Description |
|----------|---------|-------------|
| `FC_BIN` | `/usr/local/bin/firecracker` | Path to Firecracker binary |
| `FC_KERNEL` | `/var/lib/firecracker/vmlinux` | Path to kernel image |
| `FC_ROOTFS` | `/var/lib/firecracker/rootfs.ext4` | Path to root filesystem |
| `FC_BTRFS_PATH` | `/var/lib/firecracker-btrfs` | Path for Btrfs-backed storage |

### S3 Export/Import

| Variable | Default | Description |
|----------|---------|-------------|
| `S3_BUCKET` | - | S3 bucket for snapshot storage |
| `S3_REGION` | `us-east-1` | AWS region |
| `S3_PREFIX` | `snapshots/` | Key prefix for uploads |

### Server

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `4000` | HTTP server port |

## Example `.env`

```bash
# Use Docker for development
SANDBOX_RUNTIME=docker
DOCKER_IMAGE=ubuntu:22.04

# Or use VirtualBox
# SANDBOX_RUNTIME=virtualbox
# VBOX_BASE_VM=dev-1
# VBOX_BASE_SNAPSHOT=base

# Optional: S3 for snapshot export
# S3_BUCKET=my-sandbox-snapshots
# S3_REGION=us-west-2
```

## Runtime Comparison

| Feature | Docker | VirtualBox | Firecracker |
|---------|--------|------------|-------------|
| Startup time | ~1s | ~30s | ~125ms |
| Isolation | Container | Full VM | microVM |
| Platforms | All | All | Linux + KVM |
| Btrfs snapshots | ✅ | ✅ | ✅ |
| Network isolation | ✅ | ✅ | ✅ |
| Best for | Development | Testing | Production |

## API Usage

```bash
# Create API key
curl -X POST http://localhost:4000/api-keys

# Create sandbox
curl -X POST http://localhost:4000/vms \
  -H "x-api-key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-sandbox"}'

# Run command
curl -X POST http://localhost:4000/vms/my-sandbox/run \
  -H "x-api-key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"command": "echo hello"}'

# List snapshots
curl http://localhost:4000/vms/my-sandbox/snapshots \
  -H "x-api-key: YOUR_KEY"

# Restore to snapshot
curl -X POST http://localhost:4000/vms/my-sandbox/restore \
  -H "x-api-key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"snapshot": "initial"}'
```

