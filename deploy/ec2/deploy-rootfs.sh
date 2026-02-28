#!/usr/bin/env bash
set -euo pipefail

# Deploy rootfs images to the EC2 worker instance.
# Run from the repo root: ./deploy/ec2/deploy-rootfs.sh [default|ubuntu|python|node|all]
#
# This script:
#   1. Cross-compiles osb-agent for Linux ARM64
#   2. Uploads the agent, Dockerfile(s), and build script to the worker
#   3. Runs build-rootfs.sh remotely to build the ext4 image(s)
#   4. Cleans up temporary files on the worker
#
# Environment variables:
#   WORKER_IP    (required) — EC2 instance public IP
#   SSH_KEY      (optional) — path to SSH key (default: ~/.ssh/opensandbox-worker.pem)
#   SSH_USER     (optional) — SSH user (default: ubuntu)
#   IMAGES_DIR   (optional) — remote images dir (default: /data/sandboxes/firecracker/images)
#   GOARCH       (optional) — target arch (default: arm64)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

SSH_KEY="${SSH_KEY:-$HOME/.ssh/opensandbox-worker.pem}"
WORKER_IP="${WORKER_IP:?Set WORKER_IP to the EC2 instance public IP}"
SSH_USER="${SSH_USER:-ubuntu}"
IMAGES_DIR="${IMAGES_DIR:-/data/sandboxes/firecracker/images}"
GOARCH="${GOARCH:-arm64}"

SSH="ssh -o StrictHostKeyChecking=no -i $SSH_KEY $SSH_USER@$WORKER_IP"
SCP="scp -o StrictHostKeyChecking=no -i $SSH_KEY"

TARGETS=("${@:-default}")

cd "$REPO_ROOT"

echo "==> Building osb-agent (linux/${GOARCH})..."
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o bin/osb-agent-rootfs ./cmd/agent/
echo "    Built: $(du -h bin/osb-agent-rootfs | cut -f1)"

# Determine which Dockerfiles to upload
DOCKERFILES=()
for target in "${TARGETS[@]}"; do
    if [ "$target" = "all" ]; then
        DOCKERFILES=(deploy/firecracker/rootfs/Dockerfile.*)
        break
    else
        df="deploy/firecracker/rootfs/Dockerfile.${target}"
        if [ ! -f "$df" ]; then
            echo "ERROR: Dockerfile not found: $df"
            exit 1
        fi
        DOCKERFILES+=("$df")
    fi
done

echo "==> Uploading build files to $WORKER_IP..."
REMOTE_DIR="/tmp/osb-rootfs-build"
$SSH "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR/deploy/firecracker/rootfs $REMOTE_DIR/bin $REMOTE_DIR/scripts"

$SCP bin/osb-agent-rootfs "$SSH_USER@$WORKER_IP:$REMOTE_DIR/bin/osb-agent"
$SSH "chmod +x $REMOTE_DIR/bin/osb-agent"

$SCP scripts/build-rootfs.sh "$SSH_USER@$WORKER_IP:$REMOTE_DIR/scripts/build-rootfs.sh"
$SSH "chmod +x $REMOTE_DIR/scripts/build-rootfs.sh"

for df in "${DOCKERFILES[@]}"; do
    $SCP "$df" "$SSH_USER@$WORKER_IP:$REMOTE_DIR/deploy/firecracker/rootfs/$(basename "$df")"
done

echo "==> Building rootfs image(s) on worker: ${TARGETS[*]}..."
$SSH "sudo IMAGES_DIR=$IMAGES_DIR AGENT_BIN=$REMOTE_DIR/bin/osb-agent $REMOTE_DIR/scripts/build-rootfs.sh ${TARGETS[*]}"

echo "==> Verifying images..."
$SSH "ls -lh $IMAGES_DIR/*.ext4"

echo "==> Cleaning up..."
$SSH "rm -rf $REMOTE_DIR"
rm -f bin/osb-agent-rootfs

echo "==> Done! Deployed rootfs for: ${TARGETS[*]}"
