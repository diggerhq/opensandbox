#!/usr/bin/env bash
set -euo pipefail

# Deploy the opensandbox-server binary + web dashboard to the EC2 instance.
# Run from the repo root: ./deploy/ec2/deploy-server.sh
#
# This script:
#   1. Cross-compiles the server binary for Linux
#   2. Builds the web dashboard
#   3. Pulls secrets from AWS Secrets Manager
#   4. Uploads everything and restarts the service
#
# Required env vars:
#   WORKER_IP   - EC2 instance public IP
#
# Optional env vars:
#   SSH_KEY     - path to SSH key (default: ~/.ssh/opensandbox-worker.pem)
#   SSH_USER    - SSH user (default: ubuntu)
#   GOARCH      - target arch (default: amd64)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

SSH_KEY="${SSH_KEY:-$HOME/.ssh/opensandbox-worker.pem}"
WORKER_IP="${WORKER_IP:?Set WORKER_IP to the EC2 instance public IP}"
SSH_USER="${SSH_USER:-ubuntu}"
SSH="ssh -i $SSH_KEY $SSH_USER@$WORKER_IP"
SCP="scp -i $SSH_KEY"

GOARCH="${GOARCH:-amd64}"

cd "$REPO_ROOT"

# 1. Build server binary
echo "==> Building opensandbox-server (linux/${GOARCH})..."
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o bin/opensandbox-server-deploy ./cmd/server/
echo "    Built: opensandbox-server ($(du -h bin/opensandbox-server-deploy | cut -f1), ${GOARCH})"

# 2. Build web dashboard
echo "==> Building web dashboard..."
(cd web && npm ci --silent && npm run build --silent)
tar czf bin/web-dist.tar.gz -C web dist
echo "    Built: web/dist"

# 3. Upload
echo "==> Uploading to $WORKER_IP..."
$SCP bin/opensandbox-server-deploy "$SSH_USER@$WORKER_IP:/tmp/opensandbox-server"
$SCP bin/web-dist.tar.gz "$SSH_USER@$WORKER_IP:/tmp/web-dist.tar.gz"

# 4. Install and pull secrets
echo "==> Installing binary and web assets..."
$SSH "sudo mv /tmp/opensandbox-server /usr/local/bin/opensandbox-server && \
      sudo chmod +x /usr/local/bin/opensandbox-server && \
      sudo mkdir -p /opt/opensandbox/web && \
      sudo tar xzf /tmp/web-dist.tar.gz -C /opt/opensandbox/web && \
      rm /tmp/web-dist.tar.gz"

# 5. Pull secrets from AWS Secrets Manager and merge into server.env
echo "==> Pulling secrets from AWS Secrets Manager..."
$SSH 'SECRETS_ARN=$(grep OPENSANDBOX_SECRETS_ARN /etc/opensandbox/server.env 2>/dev/null | cut -d= -f2-)
if [ -n "$SECRETS_ARN" ]; then
    SECRETS_JSON=$(aws --region us-east-2 secretsmanager get-secret-value \
        --secret-id "$SECRETS_ARN" --query SecretString --output text 2>&1) || {
        echo "    WARNING: Failed to pull secrets: $SECRETS_JSON"
        echo "    Skipping secrets merge, using existing env file."
        exit 0
    }
    # Parse JSON keys and merge into server.env (secrets override existing values)
    echo "$SECRETS_JSON" | python3 -c "
import json, sys
secrets = json.load(sys.stdin)
# Read existing env
existing = {}
try:
    with open(\"/etc/opensandbox/server.env\") as f:
        for line in f:
            line = line.strip()
            if line and not line.startswith(\"#\") and \"=\" in line:
                k, v = line.split(\"=\", 1)
                existing[k] = v
except FileNotFoundError:
    pass
# Merge secrets (secrets win)
existing.update(secrets)
# Write back
with open(\"/tmp/server.env.new\", \"w\") as f:
    for k, v in existing.items():
        f.write(f\"{k}={v}\n\")
" && sudo mv /tmp/server.env.new /etc/opensandbox/server.env
    echo "    Secrets merged into server.env"
else
    echo "    No OPENSANDBOX_SECRETS_ARN found, skipping secrets pull."
fi'

# 6. Restart
echo "==> Restarting server service..."
$SSH "sudo systemctl restart opensandbox-server"

echo "==> Waiting for server to start..."
sleep 2
$SSH "sudo systemctl is-active opensandbox-server"

echo "==> Deployed successfully!"
echo "    Server: /usr/local/bin/opensandbox-server"
echo "    Web:    /opt/opensandbox/web/dist"

# Cleanup
rm -f bin/opensandbox-server-deploy bin/web-dist.tar.gz
