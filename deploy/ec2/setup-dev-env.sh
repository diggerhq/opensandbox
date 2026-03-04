#!/usr/bin/env bash
# setup-dev-env.sh — Install env files + systemd units for dev deployment
# Called by: make deploy-dev (via SSH after rsync)
# Usage: sudo bash setup-dev-env.sh <API_KEY>
set -euo pipefail

API_KEY="${1:?Usage: $0 API_KEY}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Get private IP via IMDSv2
TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")
PRIVATE_IP=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/local-ipv4)

KERNEL_PATH="/opt/opensandbox/vmlinux"

# Create directories
mkdir -p /etc/opensandbox /opt/opensandbox /data/sandboxes /data/firecracker/images /data/checkpoints

# Server env
cat > /etc/opensandbox/server.env << EOF
OPENSANDBOX_MODE=server
OPENSANDBOX_PORT=8080
OPENSANDBOX_API_KEY=${API_KEY}
OPENSANDBOX_JWT_SECRET=dev-secret
OPENSANDBOX_HTTP_ADDR=http://0.0.0.0:8080
OPENSANDBOX_WORKER_ID=cp-dev-1
OPENSANDBOX_REGION=use1
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://localhost:6379
EOF
chmod 644 /etc/opensandbox/server.env

# Worker env
cat > /etc/opensandbox/worker.env << EOF
HOME=/root
OPENSANDBOX_MODE=worker
OPENSANDBOX_PORT=8081
OPENSANDBOX_HTTP_ADDR=http://${PRIVATE_IP}:8081
OPENSANDBOX_GRPC_ADVERTISE=${PRIVATE_IP}:9090
OPENSANDBOX_JWT_SECRET=dev-secret
OPENSANDBOX_WORKER_ID=w-dev-1
OPENSANDBOX_REGION=use1
OPENSANDBOX_DATA_DIR=/data/sandboxes
OPENSANDBOX_FIRECRACKER_BIN=/usr/local/bin/firecracker
OPENSANDBOX_KERNEL_PATH=${KERNEL_PATH}
OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
OPENSANDBOX_MAX_CAPACITY=10
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=512
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=1
OPENSANDBOX_REDIS_URL=redis://localhost:6379
DATABASE_URL=postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable
EOF
chmod 644 /etc/opensandbox/worker.env

# Install systemd units from repo
cp "$SCRIPT_DIR/opensandbox-server.service" /etc/systemd/system/
cp "$SCRIPT_DIR/opensandbox-worker.service" /etc/systemd/system/
systemctl daemon-reload

echo "Env files and systemd units installed (private_ip=$PRIVATE_IP)"
