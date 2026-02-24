#!/usr/bin/env bash
set -euo pipefail

# Deploy the opensandbox-worker binary to the EC2 instance.
# Run from the repo root: ./deploy/ec2/deploy-worker.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

SSH_KEY="${SSH_KEY:-$HOME/.ssh/opensandbox-worker.pem}"
WORKER_IP="${WORKER_IP:?Set WORKER_IP to the EC2 instance public IP}"
SSH_USER="${SSH_USER:-ubuntu}"
SSH="ssh -i $SSH_KEY $SSH_USER@$WORKER_IP"
SCP="scp -i $SSH_KEY"

GOARCH="${GOARCH:-arm64}"
echo "==> Building opensandbox-worker (linux/${GOARCH})..."
cd "$REPO_ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o opensandbox-worker ./cmd/worker/
echo "    Built: opensandbox-worker ($(du -h opensandbox-worker | cut -f1), ${GOARCH})"

echo "==> Uploading to $WORKER_IP..."
$SCP opensandbox-worker "$SSH_USER@$WORKER_IP:/tmp/opensandbox-worker"

echo "==> Installing and restarting service..."
$SSH "sudo mv /tmp/opensandbox-worker /usr/local/bin/opensandbox-worker && \
      sudo chmod +x /usr/local/bin/opensandbox-worker && \
      sudo systemctl restart opensandbox-worker"

echo "==> Waiting for worker to start..."
sleep 2
$SSH "sudo systemctl is-active opensandbox-worker"

echo "==> Deployed successfully!"
rm -f "$REPO_ROOT/opensandbox-worker"
