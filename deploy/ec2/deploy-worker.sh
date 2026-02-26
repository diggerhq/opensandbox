#!/usr/bin/env bash
set -euo pipefail

# Deploy the opensandbox-worker + osb-agent binaries to the EC2 instance.
# Run from the repo root: ./deploy/ec2/deploy-worker.sh
#
# This script:
#   1. Cross-compiles worker and agent binaries for Linux ARM64
#   2. Uploads them to the EC2 instance
#   3. Installs and restarts the worker service
#
# Optional: Set DEPLOY_KERNEL=1 to also download and deploy the Firecracker kernel.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

SSH_KEY="${SSH_KEY:-$HOME/.ssh/opensandbox-worker.pem}"
WORKER_IP="${WORKER_IP:?Set WORKER_IP to the EC2 instance public IP}"
SSH_USER="${SSH_USER:-ubuntu}"
SSH="ssh -i $SSH_KEY $SSH_USER@$WORKER_IP"
SCP="scp -i $SSH_KEY"

GOARCH="${GOARCH:-arm64}"
DEPLOY_KERNEL="${DEPLOY_KERNEL:-0}"

cd "$REPO_ROOT"

echo "==> Building opensandbox-worker (linux/${GOARCH})..."
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o bin/opensandbox-worker-deploy ./cmd/worker/
echo "    Built: opensandbox-worker ($(du -h bin/opensandbox-worker-deploy | cut -f1), ${GOARCH})"

echo "==> Building osb-agent (linux/${GOARCH})..."
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o bin/osb-agent-deploy ./cmd/agent/
echo "    Built: osb-agent ($(du -h bin/osb-agent-deploy | cut -f1), ${GOARCH})"

echo "==> Uploading binaries to $WORKER_IP..."
$SCP bin/opensandbox-worker-deploy "$SSH_USER@$WORKER_IP:/tmp/opensandbox-worker"
$SCP bin/osb-agent-deploy "$SSH_USER@$WORKER_IP:/tmp/osb-agent"

echo "==> Installing binaries..."
$SSH "sudo mv /tmp/opensandbox-worker /usr/local/bin/opensandbox-worker && \
      sudo chmod +x /usr/local/bin/opensandbox-worker && \
      sudo mv /tmp/osb-agent /usr/local/bin/osb-agent && \
      sudo chmod +x /usr/local/bin/osb-agent"

# Optionally deploy kernel
if [ "$DEPLOY_KERNEL" = "1" ]; then
    echo "==> Downloading Firecracker kernel..."
    FC_VERSION="${FC_VERSION:-v1.9.1}"
    KERNEL_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/vmlinux-5.10.225.bin"

    $SSH "sudo mkdir -p /data/firecracker && \
          curl -fSL -o /tmp/vmlinux '$KERNEL_URL' && \
          sudo mv /tmp/vmlinux /data/firecracker/vmlinux-arm64 && \
          echo 'Kernel installed: \$(file /data/firecracker/vmlinux-arm64)'"
fi

echo "==> Restarting worker service..."
$SSH "sudo systemctl restart opensandbox-worker"

echo "==> Waiting for worker to start..."
sleep 2
$SSH "sudo systemctl is-active opensandbox-worker"

echo "==> Deployed successfully!"
echo "    Worker: /usr/local/bin/opensandbox-worker"
echo "    Agent:  /usr/local/bin/osb-agent"

# Cleanup
rm -f bin/opensandbox-worker-deploy bin/osb-agent-deploy
