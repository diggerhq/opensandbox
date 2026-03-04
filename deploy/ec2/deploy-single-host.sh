#!/usr/bin/env bash
set -euo pipefail

# Deploy OpenSandbox (server + worker) to a single EC2 instance.
#
# Run from repo root:
#   WORKER_IP=54.167.9.152 ./deploy/ec2/deploy-single-host.sh
#
# Prerequisites:
#   - Instance provisioned with: deploy/ec2/setup-single-host.sh
#   - SSH access to the instance
#
# This script:
#   1. Provisions the instance (if --setup flag passed)
#   2. Rsyncs the repo to the instance
#   3. Builds server, worker, and agent binaries (on instance)
#   4. Starts PostgreSQL + Redis via Docker
#   5. Downloads the Firecracker kernel
#   6. Builds the default rootfs image
#   7. Starts the worker (port 8081, gRPC 9090)
#   8. Starts the server / control plane (port 8080)
#   9. Seeds test data and runs a smoke test

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

SSH_KEY="${SSH_KEY:-$HOME/.ssh/opensandbox-worker.pem}"
WORKER_IP="${WORKER_IP:?Set WORKER_IP to the EC2 instance public IP}"
SSH_USER="${SSH_USER:-ubuntu}"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=10"
SSH="ssh $SSH_OPTS -i $SSH_KEY $SSH_USER@$WORKER_IP"
SCP="scp $SSH_OPTS -i $SSH_KEY"

REMOTE_DIR="/home/${SSH_USER}/opensandbox"

# Detect instance architecture
REMOTE_ARCH=$($SSH "uname -m")
case "$REMOTE_ARCH" in
  x86_64)  GOARCH="amd64"; KERNEL_SUFFIX="x86_64" ;;
  aarch64) GOARCH="arm64"; KERNEL_SUFFIX="arm64" ;;
  *)       echo "ERROR: Unsupported architecture: $REMOTE_ARCH"; exit 1 ;;
esac
echo "==> Target: $WORKER_IP ($REMOTE_ARCH / $GOARCH)"

# ===================================================================
# Step 0 (optional): Provision the instance
# ===================================================================
if [[ "${1:-}" == "--setup" ]]; then
    echo "==> Step 0: Provisioning instance..."
    $SSH 'bash -s' < "$SCRIPT_DIR/setup-single-host.sh"
    echo "==> Provisioning complete. Re-run without --setup to deploy."
    exit 0
fi

# ===================================================================
# Step 1: Rsync the repo
# ===================================================================
echo "==> Step 1: Syncing repo to instance..."
rsync -az --delete \
    -e "ssh $SSH_OPTS -i $SSH_KEY" \
    --exclude='.git' \
    --exclude='bin/' \
    --exclude='node_modules/' \
    --exclude='web/dist/' \
    --exclude='.claude/' \
    --exclude='delme/' \
    "$REPO_ROOT/" "$SSH_USER@$WORKER_IP:$REMOTE_DIR/"
echo "    Synced to $REMOTE_DIR"

# ===================================================================
# Step 2: Build binaries on instance
# ===================================================================
echo "==> Step 2: Building binaries on instance..."
$SSH "export PATH=\$PATH:/usr/local/go/bin && \
    cd $REMOTE_DIR && \
    mkdir -p bin && \
    echo '  Building opensandbox-server...' && \
    CGO_ENABLED=0 go build -o bin/opensandbox-server ./cmd/server/ && \
    echo '  Building opensandbox-worker...' && \
    CGO_ENABLED=0 go build -o bin/opensandbox-worker ./cmd/worker/ && \
    echo '  Building osb-agent...' && \
    CGO_ENABLED=0 go build -o bin/osb-agent ./cmd/agent/ && \
    echo '  Done:' && \
    ls -lh bin/opensandbox-server bin/opensandbox-worker bin/osb-agent"

# Install binaries
echo "    Installing binaries..."
$SSH "sudo cp $REMOTE_DIR/bin/opensandbox-server /usr/local/bin/opensandbox-server && \
    sudo cp $REMOTE_DIR/bin/opensandbox-worker /usr/local/bin/opensandbox-worker && \
    sudo cp $REMOTE_DIR/bin/osb-agent /usr/local/bin/osb-agent && \
    sudo chmod +x /usr/local/bin/opensandbox-server /usr/local/bin/opensandbox-worker /usr/local/bin/osb-agent"

# ===================================================================
# Step 3: Start PostgreSQL + Redis via Docker
# ===================================================================
echo "==> Step 3: Starting PostgreSQL + Redis..."
$SSH 'docker rm -f postgres redis 2>/dev/null || true

echo "  Starting PostgreSQL..."
docker run -d --name postgres --restart unless-stopped \
  -e POSTGRES_DB=opensandbox -e POSTGRES_USER=opensandbox -e POSTGRES_PASSWORD=opensandbox \
  -p 5432:5432 postgres:16-alpine

echo "  Starting Redis..."
docker run -d --name redis --restart unless-stopped \
  -p 6379:6379 redis:7-alpine

echo "  Waiting for PostgreSQL to be ready..."
for i in $(seq 1 30); do
    if docker exec postgres pg_isready -U opensandbox -q 2>/dev/null; then
        echo "  PostgreSQL ready"
        break
    fi
    sleep 1
done

echo "  Waiting for Redis to be ready..."
for i in $(seq 1 10); do
    if docker exec redis redis-cli ping 2>/dev/null | grep -q PONG; then
        echo "  Redis ready"
        break
    fi
    sleep 1
done'

# ===================================================================
# Step 4: Download Firecracker kernel
# ===================================================================
echo "==> Step 4: Downloading Firecracker kernel..."
# Firecracker doesn't ship kernels in releases since v1.8+.
# Use the quickstart kernel from the Firecracker S3 bucket.
case "$REMOTE_ARCH" in
  x86_64)  KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin" ;;
  aarch64) KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/aarch64/kernels/vmlinux.bin" ;;
esac

$SSH "sudo mkdir -p /data/firecracker && \
    if [ -f /data/firecracker/vmlinux-${KERNEL_SUFFIX} ]; then
        echo '  Kernel already exists, skipping download'
    else
        echo '  Downloading kernel from ${KERNEL_URL}...'
        curl -fSL -o /tmp/vmlinux '${KERNEL_URL}' && \
        sudo mv /tmp/vmlinux /data/firecracker/vmlinux-${KERNEL_SUFFIX} && \
        echo '  Kernel installed: \$(ls -lh /data/firecracker/vmlinux-${KERNEL_SUFFIX})'
    fi"

# ===================================================================
# Step 5: Build rootfs image
# ===================================================================
echo "==> Step 5: Building default rootfs image..."
$SSH "export PATH=\$PATH:/usr/local/go/bin && \
    cd $REMOTE_DIR && \
    if [ -f /data/firecracker/images/default.ext4 ]; then
        echo '  Rootfs image already exists, skipping build'
        echo '  (delete /data/firecracker/images/default.ext4 to rebuild)'
    else
        echo '  Building rootfs (this takes a few minutes)...'
        sudo AGENT_BIN=$REMOTE_DIR/bin/osb-agent IMAGES_DIR=/data/firecracker/images \
            ./scripts/build-rootfs.sh default
    fi"

# ===================================================================
# Step 6: Stop existing services and start the worker
# ===================================================================
echo "==> Step 6: Starting worker..."

# Stop any existing instances
$SSH 'sudo systemctl stop opensandbox-worker 2>/dev/null || true
sudo systemctl stop opensandbox-server 2>/dev/null || true
# Kill any manually-started instances
sudo pkill -f "opensandbox-worker" 2>/dev/null || true
sudo pkill -f "opensandbox-server" 2>/dev/null || true
sleep 1'

# Write worker env file
$SSH "sudo tee /etc/opensandbox/worker.env > /dev/null << 'ENV'
HOME=/root
OPENSANDBOX_MODE=worker
OPENSANDBOX_PORT=8081
OPENSANDBOX_JWT_SECRET=dev-secret
OPENSANDBOX_WORKER_ID=w-test-1
OPENSANDBOX_REGION=use1
OPENSANDBOX_HTTP_ADDR=http://${WORKER_IP}:8081
OPENSANDBOX_GRPC_ADVERTISE=127.0.0.1:9090
OPENSANDBOX_DATA_DIR=/data/sandboxes
OPENSANDBOX_FIRECRACKER_BIN=/usr/local/bin/firecracker
OPENSANDBOX_KERNEL_PATH=/data/firecracker/vmlinux-${KERNEL_SUFFIX}
OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
OPENSANDBOX_MAX_CAPACITY=10
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=512
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=1
OPENSANDBOX_REDIS_URL=redis://localhost:6379
DATABASE_URL=postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable
ENV"

$SSH "sudo systemctl start opensandbox-worker"
echo "    Worker starting on port 8081 (gRPC 9090)..."
sleep 3

# Check worker status
$SSH "sudo systemctl is-active opensandbox-worker && \
    echo '  Worker is running' || \
    (echo '  ERROR: Worker failed to start. Logs:' && sudo journalctl -u opensandbox-worker --no-pager -n 20 && exit 1)"

# ===================================================================
# Step 7: Start the server (control plane)
# ===================================================================
echo "==> Step 7: Starting server (control plane)..."

# Write server env file
$SSH "sudo tee /etc/opensandbox/server.env > /dev/null << 'ENV'
OPENSANDBOX_MODE=server
OPENSANDBOX_PORT=8080
OPENSANDBOX_API_KEY=test-key
OPENSANDBOX_JWT_SECRET=dev-secret
OPENSANDBOX_REGION=use1
OPENSANDBOX_WORKER_ID=cp-test-1
OPENSANDBOX_HTTP_ADDR=http://${WORKER_IP}:8080
OPENSANDBOX_DATA_DIR=/tmp/opensandbox-data
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://localhost:6379
ENV"

$SSH "sudo systemctl start opensandbox-server"
echo "    Server starting on port 8080..."
sleep 3

# Check server status
$SSH "sudo systemctl is-active opensandbox-server && \
    echo '  Server is running' || \
    (echo '  ERROR: Server failed to start. Logs:' && sudo journalctl -u opensandbox-server --no-pager -n 20 && exit 1)"

# ===================================================================
# Step 8: Seed test data and smoke test
# ===================================================================
echo "==> Step 8: Seeding test data and running smoke test..."

# Wait for migrations to complete
sleep 2

$SSH "PGPASSWORD=opensandbox psql -h localhost -U opensandbox -d opensandbox -q -c \"
  INSERT INTO orgs (name, slug) VALUES ('Test Org', 'test-org') ON CONFLICT (slug) DO NOTHING;
  INSERT INTO api_keys (org_id, key_hash, key_prefix, name)
  SELECT id, encode(sha256('test-key'::bytea), 'hex'), 'test-key', 'Dev Key'
  FROM orgs WHERE slug = 'test-org'
  ON CONFLICT (key_hash) DO NOTHING;
\" && echo '  Test org and API key seeded'"

# Verify worker registered in Redis
echo "    Checking worker registration..."
$SSH 'docker exec redis redis-cli GET worker:w-test-1 | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
print(f\"  Worker registered: id={data[\"worker_id\"]}, region={data[\"region\"]}, grpc={data[\"grpc_addr\"]}\")
" 2>/dev/null || echo "  WARNING: Worker not yet registered in Redis (may need a few more seconds)"'

# Smoke test: create a sandbox
echo "    Creating test sandbox..."
RESULT=$($SSH "curl -s -w '\n%{http_code}' -X POST http://localhost:8080/api/sandboxes \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: test-key' \
  -d '{\"templateID\":\"default\"}'")

HTTP_CODE=$(echo "$RESULT" | tail -1)
BODY=$(echo "$RESULT" | head -n -1)

if [ "$HTTP_CODE" = "200" ] || [ "$HTTP_CODE" = "201" ]; then
    SANDBOX_ID=$(echo "$BODY" | python3 -c "import json,sys; print(json.loads(sys.stdin.read()).get('sandboxID','?'))" 2>/dev/null || echo "?")
    echo "    Sandbox created successfully (HTTP $HTTP_CODE): $SANDBOX_ID"

    # Execute a command in the sandbox to verify end-to-end
    echo "    Executing test command..."
    sleep 2
    CMD_RESULT=$($SSH "curl -s -X POST http://localhost:8080/api/sandboxes/${SANDBOX_ID}/commands \
      -H 'Content-Type: application/json' \
      -H 'X-API-Key: test-key' \
      -d '{\"cmd\":\"echo Hello from Firecracker && uname -a\"}'")
    echo "    $CMD_RESULT"
else
    echo "    WARNING: Sandbox creation returned HTTP $HTTP_CODE"
    echo "    $BODY"
fi

echo ""
echo "============================================"
echo " Deployment complete!"
echo ""
echo " Services:"
echo "   Server (CP):  http://$WORKER_IP:8080"
echo "   Worker:       http://$WORKER_IP:8081"
echo "   Worker gRPC:  $WORKER_IP:9090"
echo "   PostgreSQL:   localhost:5432"
echo "   Redis:        localhost:6379"
echo ""
echo " Test commands:"
echo "   # Create sandbox:"
echo "   curl -X POST http://$WORKER_IP:8080/api/sandboxes \\"
echo "     -H 'Content-Type: application/json' \\"
echo "     -H 'X-API-Key: test-key' \\"
echo "     -d '{\"templateID\":\"default\"}'"
echo ""
echo " Logs:"
echo "   ssh -i $SSH_KEY $SSH_USER@$WORKER_IP"
echo "   sudo journalctl -u opensandbox-server -f"
echo "   sudo journalctl -u opensandbox-worker -f"
echo "============================================"
