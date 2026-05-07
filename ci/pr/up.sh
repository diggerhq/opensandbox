#!/usr/bin/env bash
# Bring up an ephemeral test stack for a PR.
#
# Usage: ./up.sh <PR_NUM> [WORKERS=0]
#
# Creates: pr_<num> Postgres database, pr-<num>-checkpoints storage container,
# pr-<num>-server VM (and pr-<num>-worker-X VMs if WORKERS>0). Deploys server
# binary, waits for /healthz green. Echoes connection info.
#
# v1: server-only. WORKERS>0 not yet implemented.

set -euo pipefail

PR_NUM="${1:-}"
WORKERS="${2:-0}"

[[ -n "$PR_NUM" ]] || { echo "usage: $0 <PR_NUM> [WORKERS=0]"; exit 1; }
[[ "$PR_NUM" =~ ^[0-9]+$ ]] || { echo "PR_NUM must be numeric"; exit 1; }
if [[ "$WORKERS" != "0" ]]; then
  echo "FATAL: worker bring-up not yet implemented (need kernel + base image bootstrap). Use WORKERS=0 for v1."
  exit 1
fi

RG="opencomputer-ci"
LOCATION="centralus"
KV="opencomputer-ci-kv"
VNET="oc-ci-vnet"
SUBNET="compute"
SSH_KEY_PRIV="$HOME/.ssh/opencomputer-ci"
SSH_KEY_PUB="$HOME/.ssh/opencomputer-ci.pub"
SSH_OPTS="-i $SSH_KEY_PRIV -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
MAX_CONCURRENT_PRS="${MAX_CONCURRENT_PRS:-8}"

REPO_ROOT=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)

# Concurrency cap: refuse to spin up another stack if MAX_CONCURRENT_PRS active.
# Counts existing pr-*-server VMs (excluding this one if it already exists).
ACTIVE=$(az vm list -g "$RG" --query "[?starts_with(name,'pr-') && ends_with(name,'-server') && name != 'pr-${PR_NUM}-server'] | length(@)" -o tsv)
if [[ "$ACTIVE" -ge "$MAX_CONCURRENT_PRS" ]]; then
  echo "FATAL: $ACTIVE PR stacks already active (cap=$MAX_CONCURRENT_PRS). Wait or tear one down."
  az vm list -g "$RG" --query "[?starts_with(name,'pr-') && ends_with(name,'-server')].{name:name,created:timeCreated}" -o table
  exit 1
fi

echo ">>> [0/8] read persistent state from KV"
kv() { az keyvault secret show --vault-name "$KV" --name "$1" --query value -o tsv; }
DATA_VM_PRIV=$(kv data-vm-private-ip)
DATA_VM_PIP=$(az vm show -d -g "$RG" -n oc-ci-data --query publicIps -o tsv)
PG_PASS=$(kv pg-password)
REDIS_PASS=$(kv redis-password)
API_KEY=$(kv server-api-key)
JWT=$(kv server-jwt-secret)
ENC_KEY=$(kv server-secret-encryption-key)
STORAGE=$(kv storage-account-name)
STORAGE_KEY=$(kv worker-s3-secret-key)

DB_NAME="pr_$PR_NUM"
CONTAINER="pr-${PR_NUM}-checkpoints"
SERVER_VM="pr-${PR_NUM}-server"

echo ">>> [1/8] build opensandbox-server (linux/amd64)"
SERVER_BIN=$(mktemp /tmp/opensandbox-server-XXXX)
trap "rm -f $SERVER_BIN" EXIT
(cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$SERVER_BIN" ./cmd/server/) >/dev/null
echo "    binary: $(du -h "$SERVER_BIN" | cut -f1)"

echo ">>> [2/8] CREATE DATABASE $DB_NAME"
ssh $SSH_OPTS azureuser@"$DATA_VM_PIP" \
  "PGPASSWORD='$PG_PASS' psql -h localhost -U postgres -d postgres -tc \"SELECT 1 FROM pg_database WHERE datname='$DB_NAME'\" | grep -q 1 \
   || PGPASSWORD='$PG_PASS' psql -h localhost -U postgres -d postgres -c \"CREATE DATABASE $DB_NAME OWNER osbciuser;\""

echo ">>> [3/8] storage container $CONTAINER"
az storage container create --account-name "$STORAGE" --account-key "$STORAGE_KEY" -n "$CONTAINER" -o none 2>/dev/null || true

echo ">>> [4/8] provision $SERVER_VM"
if ! az vm show -g "$RG" -n "$SERVER_VM" -o none 2>/dev/null; then
  az vm create -g "$RG" -n "$SERVER_VM" \
    --image Ubuntu2404 \
    --size Standard_B2s \
    --vnet-name "$VNET" --subnet "$SUBNET" \
    --public-ip-address "${SERVER_VM}-pip" \
    --public-ip-sku Standard \
    --admin-username azureuser \
    --ssh-key-values "$SSH_KEY_PUB" \
    --os-disk-size-gb 32 \
    --nsg oc-ci-nsg-compute \
    --tags purpose=ci pr="$PR_NUM" \
    -o none
fi
SERVER_PIP=$(az vm show -d -g "$RG" -n "$SERVER_VM" --query publicIps -o tsv)
SERVER_PRIV=$(az vm show -d -g "$RG" -n "$SERVER_VM" --query privateIps -o tsv)

echo ">>> [5/8] wait for SSH on $SERVER_PIP"
for _ in $(seq 1 40); do
  ssh $SSH_OPTS -o ConnectTimeout=5 azureuser@"$SERVER_PIP" "echo ok" 2>/dev/null | grep -q ok && break
  sleep 5
done

echo ">>> [6/8] render env + scp + systemctl start"
SERVER_ENV=$(mktemp)
SERVER_UNIT=$(mktemp)
cat > "$SERVER_ENV" <<EOF
OPENSANDBOX_MODE=server
OPENSANDBOX_PORT=8080
OPENSANDBOX_HTTP_ADDR=http://$SERVER_PIP:8080
OPENSANDBOX_DATABASE_URL=postgres://osbciuser:$PG_PASS@$DATA_VM_PRIV:5432/$DB_NAME?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://default:$REDIS_PASS@$DATA_VM_PRIV:6379/0
OPENSANDBOX_API_KEY=$API_KEY
OPENSANDBOX_JWT_SECRET=$JWT
OPENSANDBOX_SECRET_ENCRYPTION_KEY=$ENC_KEY
OPENSANDBOX_REGION=$LOCATION
OPENSANDBOX_S3_BUCKET=$CONTAINER
OPENSANDBOX_S3_ENDPOINT=https://$STORAGE.blob.core.windows.net
OPENSANDBOX_S3_REGION=$LOCATION
OPENSANDBOX_S3_ACCESS_KEY_ID=$STORAGE
OPENSANDBOX_S3_SECRET_ACCESS_KEY=$STORAGE_KEY
OPENSANDBOX_MIN_WORKERS=0
OPENSANDBOX_MAX_WORKERS=$WORKERS
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=2
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=1024
OPENSANDBOX_DEFAULT_SANDBOX_DISK_MB=20480
OPENSANDBOX_SANDBOX_DOMAIN=ci.local
OPENSANDBOX_IDLE_RESERVE=0
EOF

cat > "$SERVER_UNIT" <<EOF
[Unit]
Description=opensandbox server (PR $PR_NUM)
After=network.target

[Service]
Type=simple
ExecStart=/opt/opensandbox/server
EnvironmentFile=/etc/opensandbox/server.env
WorkingDirectory=/opt/opensandbox
Restart=on-failure
RestartSec=5
KillMode=process
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
EOF

scp $SSH_OPTS "$SERVER_BIN" azureuser@"$SERVER_PIP":/tmp/opensandbox-server >/dev/null
scp $SSH_OPTS "$SERVER_ENV" azureuser@"$SERVER_PIP":/tmp/server.env >/dev/null
scp $SSH_OPTS "$SERVER_UNIT" azureuser@"$SERVER_PIP":/tmp/opensandbox-server.service >/dev/null
ssh $SSH_OPTS azureuser@"$SERVER_PIP" "sudo bash -se" <<'REMOTE'
set -e
mkdir -p /opt/opensandbox /etc/opensandbox
mv /tmp/opensandbox-server /opt/opensandbox/server
chmod +x /opt/opensandbox/server
mv /tmp/server.env /etc/opensandbox/server.env
chmod 600 /etc/opensandbox/server.env
mv /tmp/opensandbox-server.service /etc/systemd/system/opensandbox-server.service
systemctl daemon-reload
systemctl enable opensandbox-server >/dev/null 2>&1
systemctl restart opensandbox-server
REMOTE

rm -f "$SERVER_ENV" "$SERVER_UNIT"

echo ">>> [7/8] wait for /healthz"
HEALTHY=0
for i in $(seq 1 40); do
  resp=$(curl -s -m 3 "http://$SERVER_PIP:8080/healthz" 2>/dev/null || true)
  if [[ -n "$resp" ]] && echo "$resp" | grep -qiE "ok|status"; then
    HEALTHY=1
    echo "    healthy after ${i}x3s — response: $resp"
    break
  fi
  sleep 3
done

if [[ "$HEALTHY" == "0" ]]; then
  echo "    /healthz did not respond. server logs:"
  ssh $SSH_OPTS azureuser@"$SERVER_PIP" "sudo journalctl -u opensandbox-server --no-pager -n 50 || true"
  exit 1
fi

echo ">>> [8/8] DONE"
cat <<DONE

================================================================
PR-$PR_NUM stack is up.

  server URL:   http://$SERVER_PIP:8080
  api key:      $API_KEY
  database:     $DB_NAME (on data VM $DATA_VM_PRIV)
  storage:      $CONTAINER (account: $STORAGE)
  workers:      $WORKERS (server-only mode for v1)

  smoke:        curl -H "Authorization: Bearer $API_KEY" http://$SERVER_PIP:8080/api/orgs
  ssh:          ssh -i ~/.ssh/opencomputer-ci azureuser@$SERVER_PIP
  logs:         ssh ... 'sudo journalctl -u opensandbox-server -f'
  teardown:     ./down.sh $PR_NUM
================================================================
DONE
