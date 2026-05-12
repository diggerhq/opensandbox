#!/usr/bin/env bash
# Bring up an ephemeral test stack for a PR.
#
# Usage: ./up.sh <PR_NUM> [WORKERS=0]
#
# Creates: pr_<num> Postgres database, pr-<num>-checkpoints storage container,
# pr-<num>-server VM, and N pr-<num>-worker-X VMs. Deploys server + worker
# binaries (built from the current tree) and replaces the gallery-baked agent
# inside default.ext4 with the PR's agent. Waits for /healthz green and worker
# registration. Echoes connection info.

set -euo pipefail

PR_NUM="${1:-}"
WORKERS="${2:-0}"

[[ -n "$PR_NUM" ]] || { echo "usage: $0 <PR_NUM> [WORKERS=0]"; exit 1; }
[[ "$PR_NUM" =~ ^[0-9]+$ ]] || { echo "PR_NUM must be numeric"; exit 1; }
[[ "$WORKERS" =~ ^[0-9]+$ ]] || { echo "WORKERS must be numeric"; exit 1; }

RG="opencomputer-ci"
LOCATION="${CI_LOCATION:-eastus2}"
KV="opencomputer-ci-kv"
VNET="oc-ci-vnet"
SUBNET="compute"
SSH_KEY_PRIV="$HOME/.ssh/opencomputer-ci"
SSH_KEY_PUB="$HOME/.ssh/opencomputer-ci.pub"
SSH_OPTS="-i $SSH_KEY_PRIV -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
MAX_CONCURRENT_PRS="${MAX_CONCURRENT_PRS:-8}"
WORKER_SIZE="${WORKER_SIZE:-Standard_D16ads_v7}"

# Prod gallery image — CI grants opensandbox-test-gh-action SP Reader on this.
WORKER_IMAGE_VERSION="${WORKER_IMAGE_VERSION:-1.0.66}"
WORKER_IMAGE_ID="/subscriptions/070cd7a5-4e38-4ad2-9f40-bf261000a040/resourceGroups/opencomputer-prod/providers/Microsoft.Compute/galleries/oc_prod_gallery/images/osb-worker-v7/versions/${WORKER_IMAGE_VERSION}"

REPO_ROOT=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)

ACTIVE=$(az vm list -g "$RG" --query "[?starts_with(name,'pr-') && ends_with(name,'-server') && name != 'pr-${PR_NUM}-server'] | length(@)" -o tsv)
if [[ "$ACTIVE" -ge "$MAX_CONCURRENT_PRS" ]]; then
  echo "FATAL: $ACTIVE PR stacks already active (cap=$MAX_CONCURRENT_PRS). Wait or tear one down."
  az vm list -g "$RG" --query "[?starts_with(name,'pr-') && ends_with(name,'-server')].{name:name,created:timeCreated}" -o table
  exit 1
fi

echo ">>> [0/9] read persistent state from KV"
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

SCRATCH=$(mktemp -d)
trap "rm -rf $SCRATCH" EXIT

echo ">>> [1/9] build linux/amd64 binaries (server + worker + agent)"
SERVER_BIN="$SCRATCH/opensandbox-server"
WORKER_BIN="$SCRATCH/opensandbox-worker"
AGENT_BIN="$SCRATCH/osb-agent"
(cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$SERVER_BIN" ./cmd/server/) >/dev/null
if [[ "$WORKERS" -gt 0 ]]; then
  (cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$WORKER_BIN" ./cmd/worker/) >/dev/null
  (cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$AGENT_BIN" ./cmd/agent/) >/dev/null
  echo "    current: server=$(du -h "$SERVER_BIN" | cut -f1) worker=$(du -h "$WORKER_BIN" | cut -f1) agent=$(du -h "$AGENT_BIN" | cut -f1)"
else
  echo "    server=$(du -h "$SERVER_BIN" | cut -f1) (workers=0, skipping worker+agent build)"
fi

# Optional cross-version setup: if BASELINE_FROM_GALLERY=1 and WORKERS>=2,
# worker 1 keeps the gallery image's pre-baked agent + worker binaries (= the
# current prod golden), while workers 2..N get the PR's overrides. Lets the
# migrate test exercise real old-golden → new-golden upgrade on every PR.
BASELINE_FROM_GALLERY="${BASELINE_FROM_GALLERY:-0}"
if [[ "$BASELINE_FROM_GALLERY" == "1" && "$WORKERS" -lt 2 ]]; then
  echo "    BASELINE_FROM_GALLERY=1 requires WORKERS>=2, ignoring"
  BASELINE_FROM_GALLERY=0
fi
if [[ "$BASELINE_FROM_GALLERY" == "1" ]]; then
  echo "    cross-version mode: worker 1 uses gallery-baked binaries (current prod golden)"
fi

echo ">>> [2/9] CREATE DATABASE $DB_NAME"
ssh $SSH_OPTS azureuser@"$DATA_VM_PIP" \
  "PGPASSWORD='$PG_PASS' psql -h localhost -U postgres -d postgres -tc \"SELECT 1 FROM pg_database WHERE datname='$DB_NAME'\" | grep -q 1 \
   || PGPASSWORD='$PG_PASS' psql -h localhost -U postgres -d postgres -c \"CREATE DATABASE $DB_NAME OWNER osbciuser;\""

echo ">>> [3/9] storage container $CONTAINER"
# Azure soft-deletes containers and refuses recreation for ~30s after delete.
# If down.sh just ran for this PR, the first `container create` will silently
# fail and workers boot without a container to upload bases to. Poll until
# the container actually exists.
for i in $(seq 1 24); do
  if az storage container show --account-name "$STORAGE" --account-key "$STORAGE_KEY" -n "$CONTAINER" -o none 2>/dev/null; then
    break
  fi
  out=$(az storage container create --account-name "$STORAGE" --account-key "$STORAGE_KEY" -n "$CONTAINER" 2>&1 || true)
  if echo "$out" | grep -q '"created": true'; then
    break
  fi
  echo "    container not ready yet (attempt $i, retrying in 5s)"
  sleep 5
done
if ! az storage container show --account-name "$STORAGE" --account-key "$STORAGE_KEY" -n "$CONTAINER" -o none 2>/dev/null; then
  echo "FATAL: container $CONTAINER did not become available"
  exit 1
fi

echo ">>> [4/9] provision $SERVER_VM"
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
    --tags purpose=ci pr="$PR_NUM" role=server \
    -o none
fi
SERVER_PIP=$(az vm show -d -g "$RG" -n "$SERVER_VM" --query publicIps -o tsv)
SERVER_PRIV=$(az vm show -d -g "$RG" -n "$SERVER_VM" --query privateIps -o tsv)

echo ">>> [5/9] wait for SSH on $SERVER_PIP"
for _ in $(seq 1 40); do
  ssh $SSH_OPTS -o ConnectTimeout=5 azureuser@"$SERVER_PIP" "echo ok" 2>/dev/null | grep -q ok && break
  sleep 5
done

echo ">>> [6/9] deploy server (env + binary + systemd)"
cat > "$SCRATCH/server.env" <<EOF
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
OPENSANDBOX_S3_FORCE_PATH_STYLE=false
OPENSANDBOX_MIN_WORKERS=0
OPENSANDBOX_MAX_WORKERS=$WORKERS
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=2
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=1024
OPENSANDBOX_DEFAULT_SANDBOX_DISK_MB=20480
OPENSANDBOX_SANDBOX_DOMAIN=ci.local
OPENSANDBOX_IDLE_RESERVE=0
EOF

cat > "$SCRATCH/server.service" <<EOF
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

scp $SSH_OPTS "$SERVER_BIN" "$SCRATCH/server.env" "$SCRATCH/server.service" azureuser@"$SERVER_PIP":/tmp/ >/dev/null
ssh $SSH_OPTS azureuser@"$SERVER_PIP" "sudo bash -se" <<'REMOTE'
set -e
mkdir -p /opt/opensandbox /etc/opensandbox
mv /tmp/opensandbox-server /opt/opensandbox/server
chmod +x /opt/opensandbox/server
mv /tmp/server.env /etc/opensandbox/server.env
chmod 600 /etc/opensandbox/server.env
mv /tmp/server.service /etc/systemd/system/opensandbox-server.service
systemctl daemon-reload
systemctl enable opensandbox-server >/dev/null 2>&1
systemctl restart opensandbox-server
REMOTE

echo ">>> [7/9] wait for /healthz"
HEALTHY=0
for i in $(seq 1 40); do
  resp=$(curl -s -m 3 "http://$SERVER_PIP:8080/healthz" 2>/dev/null || true)
  if [[ -n "$resp" ]] && echo "$resp" | grep -qiE "ok|status"; then
    HEALTHY=1
    echo "    healthy after ${i}x3s"
    break
  fi
  sleep 3
done
if [[ "$HEALTHY" == "0" ]]; then
  echo "    /healthz did not respond. server logs:"
  ssh $SSH_OPTS azureuser@"$SERVER_PIP" "sudo journalctl -u opensandbox-server --no-pager -n 50 || true"
  exit 1
fi

# Seed the static API key into pr_<num>.api_keys so PG-backed auth accepts it.
# Server-mode validates X-API-Key against the api_keys table, not the env var.
KEY_HASH=$(printf '%s' "$API_KEY" | { sha256sum 2>/dev/null || shasum -a 256; } | cut -d' ' -f1)
KEY_PREFIX=$(printf '%s' "$API_KEY" | cut -c1-8)
ssh $SSH_OPTS azureuser@"$DATA_VM_PIP" "PGPASSWORD='$PG_PASS' psql -h localhost -U postgres -d $DB_NAME" <<SQL >/dev/null
INSERT INTO orgs (id, name, slug) VALUES ('00000000-0000-0000-0000-000000000001', 'CI Org', 'ci')
ON CONFLICT DO NOTHING;
INSERT INTO api_keys (id, org_id, key_hash, key_prefix, name)
VALUES ('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000001', '$KEY_HASH', '$KEY_PREFIX', 'ci-key')
ON CONFLICT DO NOTHING;
SQL

# ─── Worker provisioning ────────────────────────────────────────────────────
if [[ "$WORKERS" -gt 0 ]]; then
  # Disk setup happens over SSH after VM boot. The gallery image's cloud-init
  # config skips runcmd, so embedding it as custom-data is unreliable.

  provision_worker() {
    local idx=$1
    local vm_name="pr-${PR_NUM}-worker-${idx}"
    local stage="[worker $idx]"

    if ! az vm show -g "$RG" -n "$vm_name" -o none 2>/dev/null; then
      echo "    $stage az vm create (cross-region first boot is ~3-4 min slower)"
      az vm create -g "$RG" -n "$vm_name" \
        --image "$WORKER_IMAGE_ID" \
        --size "$WORKER_SIZE" \
        --vnet-name "$VNET" --subnet "$SUBNET" \
        --public-ip-address "${vm_name}-pip" \
        --public-ip-sku Standard \
        --admin-username azureuser \
        --ssh-key-values "$SSH_KEY_PUB" \
        --os-disk-size-gb 64 \
        --nsg oc-ci-nsg-compute \
        --tags purpose=ci pr="$PR_NUM" role=worker \
        -o none
    fi
    local pip=$(az vm show -d -g "$RG" -n "$vm_name" --query publicIps -o tsv)
    local priv=$(az vm show -d -g "$RG" -n "$vm_name" --query privateIps -o tsv)

    echo "    $stage waiting for SSH on $pip"
    for _ in $(seq 1 60); do
      ssh $SSH_OPTS -o ConnectTimeout=5 azureuser@"$pip" "echo ok" 2>/dev/null | grep -q ok && break
      sleep 5
    done

    cat > "$SCRATCH/worker-${idx}.env" <<EOF
OPENSANDBOX_MODE=worker
OPENSANDBOX_VM_BACKEND=qemu
OPENSANDBOX_QEMU_BIN=qemu-system-x86_64
OPENSANDBOX_DATA_DIR=/data2/sandboxes
OPENSANDBOX_KERNEL_PATH=/opt/opensandbox/vmlinux
OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
OPENSANDBOX_GRPC_ADVERTISE=$priv:9090
OPENSANDBOX_HTTP_ADDR=http://$priv:8081
OPENSANDBOX_PORT=8081
OPENSANDBOX_JWT_SECRET=$JWT
OPENSANDBOX_WORKER_ID=w-pr${PR_NUM}-${idx}
OPENSANDBOX_REGION=$LOCATION
OPENSANDBOX_SANDBOX_DOMAIN=ci.local
OPENSANDBOX_MAX_CAPACITY=20
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=2
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=1024
OPENSANDBOX_DEFAULT_SANDBOX_DISK_MB=20480
OPENSANDBOX_DATABASE_URL=postgres://osbciuser:$PG_PASS@$DATA_VM_PRIV:5432/$DB_NAME?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://default:$REDIS_PASS@$DATA_VM_PRIV:6379/0
OPENSANDBOX_S3_BUCKET=$CONTAINER
OPENSANDBOX_S3_ENDPOINT=https://$STORAGE.blob.core.windows.net
OPENSANDBOX_S3_REGION=$LOCATION
OPENSANDBOX_S3_ACCESS_KEY_ID=$STORAGE
OPENSANDBOX_S3_SECRET_ACCESS_KEY=$STORAGE_KEY
OPENSANDBOX_S3_FORCE_PATH_STYLE=false
EOF

    # Worker 1 uses the gallery image's pre-baked binaries when in cross-version
    # mode — that represents the current prod golden. Other workers get the
    # PR's freshly-built binaries.
    local use_gallery_baseline=0
    if [[ "$idx" == "1" && "$BASELINE_FROM_GALLERY" == "1" ]]; then
      use_gallery_baseline=1
      echo "    $stage cross-version mode: keeping gallery-baked binaries"
    fi
    echo "    $stage scp env"
    scp $SSH_OPTS "$SCRATCH/worker-${idx}.env" azureuser@"$pip":/tmp/worker.env >/dev/null
    if [[ "$use_gallery_baseline" == "0" ]]; then
      echo "    $stage scp PR-built worker + agent"
      scp $SSH_OPTS "$WORKER_BIN" azureuser@"$pip":/tmp/opensandbox-worker >/dev/null
      scp $SSH_OPTS "$AGENT_BIN" azureuser@"$pip":/tmp/osb-agent >/dev/null
    fi
    ssh $SSH_OPTS azureuser@"$pip" "sudo bash -se" <<REMOTE
set -e
USE_GALLERY_BASELINE=$use_gallery_baseline
systemctl stop opensandbox-worker 2>/dev/null || true

# Mount the local NVMe at /data2 with XFS+reflink (required by the worker's
# QEMU manager). On D-d-series_v7 the local disk is an unformatted nvme device
# that isn't the OS disk.
if ! mountpoint -q /data2; then
  ROOT_DEV=\$(findmnt -no SOURCE / | sed "s|p\\?[0-9]*\$||")
  DEV=""
  for d in /dev/nvme*n1; do
    [ -b "\$d" ] || continue
    [ "\$d" = "\$ROOT_DEV" ] && continue
    if ! blkid "\$d" >/dev/null 2>&1; then DEV="\$d"; break; fi
  done
  if [ -z "\$DEV" ]; then echo "FATAL: no unformatted local NVMe found" >&2; exit 1; fi
  echo "formatting \$DEV"
  mkfs.xfs -m reflink=1 -f "\$DEV"
  mkdir -p /data2
  mount "\$DEV" /data2
fi
mkdir -p /data2/sandboxes /data2/checkpoints
chmod 0777 /data2 /data2/sandboxes /data2/checkpoints

mkdir -p /etc/opensandbox
mv /tmp/worker.env /etc/opensandbox/worker.env
chmod 600 /etc/opensandbox/worker.env

if [[ "\$USE_GALLERY_BASELINE" == "1" ]]; then
  echo "cross-version mode: keeping gallery-baked /usr/local/bin/opensandbox-worker and ext4-baked agent"
else
  # Override worker binary + swap PR's agent into default.ext4 via loop-mount.
  mv /tmp/opensandbox-worker /usr/local/bin/opensandbox-worker
  chmod +x /usr/local/bin/opensandbox-worker
  mkdir -p /mnt/rootfs-${idx}
  mount -o loop /data/firecracker/images/default.ext4 /mnt/rootfs-${idx}
  cp /tmp/osb-agent /mnt/rootfs-${idx}/usr/local/bin/osb-agent
  chmod +x /mnt/rootfs-${idx}/usr/local/bin/osb-agent
  sync
  umount /mnt/rootfs-${idx}
  rm /tmp/osb-agent
  # Drop any cached golden snapshot — agent change invalidates it.
  rm -rf /data/sandboxes/golden /data2/sandboxes/golden 2>/dev/null || true
fi

systemctl daemon-reload
systemctl enable opensandbox-worker >/dev/null 2>&1 || true
systemctl restart opensandbox-worker
REMOTE
    echo "    $stage ready (priv=$priv)"
  }

  echo ">>> [8/9] provision $WORKERS worker(s) in parallel"
  pids=()
  for i in $(seq 1 "$WORKERS"); do
    provision_worker "$i" &
    pids+=($!)
  done
  fail=0
  for pid in "${pids[@]}"; do
    if ! wait "$pid"; then fail=1; fi
  done
  if [[ "$fail" -ne 0 ]]; then
    echo "    one or more workers failed to provision"
    exit 1
  fi

  echo ">>> wait for worker readiness via /api/workers (golden_version populated)"
  REGISTERED=0
  for i in $(seq 1 60); do
    # A worker is "ready" when it shows up in /api/workers AND has golden_version
    # set — that field is only populated after the golden snapshot finishes
    # building (~25s post-startup), which is when the worker can actually
    # accept sandbox creation requests.
    ready=$(curl -s -m 5 -H "X-API-Key: $API_KEY" "http://$SERVER_PIP:8080/api/workers" 2>/dev/null \
      | jq '[.[] // empty | select(.golden_version != null and .golden_version != "")] | length' 2>/dev/null || echo 0)
    if [[ "$ready" =~ ^[0-9]+$ ]] && [[ "$ready" -ge "$WORKERS" ]]; then
      REGISTERED=1
      echo "    $ready worker(s) ready (golden built) after ${i}x5s"
      break
    fi
    sleep 5
  done
  if [[ "$REGISTERED" == "0" ]]; then
    echo "    workers did not register. Last /api/workers response:"
    curl -s -H "X-API-Key: $API_KEY" "http://$SERVER_PIP:8080/api/workers" 2>&1 | head -20
    echo "    --- worker 1 logs:"
    pip=$(az vm show -d -g "$RG" -n "pr-${PR_NUM}-worker-1" --query publicIps -o tsv 2>/dev/null || true)
    [[ -n "$pip" ]] && ssh $SSH_OPTS azureuser@"$pip" "sudo journalctl -u opensandbox-worker --no-pager -n 50 || true"
    exit 1
  fi

  # Warmup probe: golden_version populated means the snapshot is built, but
  # the first exec round-trip (API → gRPC → vsock → agent → process spawn)
  # often takes longer than tests' 30s HTTP timeout on a cold worker. Spin
  # a throwaway sandbox + echo to confirm the full exec path is hot before
  # declaring the stack ready.
  echo ">>> warmup probe: spawn sandbox + echo + delete"
  WARM=0
  for i in $(seq 1 18); do
    sbox=$(curl -s -m 30 -X POST -H "X-API-Key: $API_KEY" -H "Content-Type: application/json" \
      -d '{"cpuCount":1,"memoryMB":1024,"diskMB":20480,"timeout":60}' \
      "http://$SERVER_PIP:8080/api/sandboxes" 2>/dev/null | jq -r '.sandboxID // empty')
    if [[ -n "$sbox" ]]; then
      out=$(curl -s -m 30 -X POST -H "X-API-Key: $API_KEY" -H "Content-Type: application/json" \
        -d '{"cmd":"echo","args":["warmup"],"timeout":10}' \
        "http://$SERVER_PIP:8080/api/sandboxes/$sbox/exec/run" 2>/dev/null)
      curl -s -X DELETE -H "X-API-Key: $API_KEY" "http://$SERVER_PIP:8080/api/sandboxes/$sbox" >/dev/null 2>&1
      if echo "$out" | grep -q '"exitCode":0'; then
        WARM=1
        echo "    warmup OK after ${i} attempt(s)"
        break
      fi
    fi
    sleep 5
  done
  if [[ "$WARM" == "0" ]]; then
    echo "    FATAL: warmup probe never succeeded (worker exec path not ready)"
    exit 1
  fi
fi

echo ">>> [9/9] DONE"

# Emit step outputs for the GitHub Actions workflow.
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "server_url=http://$SERVER_PIP:8080"
    echo "api_key=$API_KEY"
    echo "workers=$WORKERS"
  } >> "$GITHUB_OUTPUT"
fi

cat <<DONE

================================================================
PR-$PR_NUM stack is up.

  server URL:   http://$SERVER_PIP:8080
  api key:      $API_KEY
  database:     $DB_NAME (on data VM $DATA_VM_PRIV)
  storage:      $CONTAINER (account: $STORAGE)
  workers:      $WORKERS (image: osb-worker-v7:${WORKER_IMAGE_VERSION})

  smoke:        curl -H "X-API-Key: $API_KEY" http://$SERVER_PIP:8080/api/orgs
  ssh server:   ssh -i ~/.ssh/opencomputer-ci azureuser@$SERVER_PIP
  ssh worker 1: ssh -i ~/.ssh/opencomputer-ci azureuser@\$(az vm show -d -g $RG -n pr-${PR_NUM}-worker-1 --query publicIps -o tsv 2>/dev/null)
  logs:         ssh ... 'sudo journalctl -u opensandbox-{server,worker} -f'
  teardown:     ./down.sh $PR_NUM
================================================================
DONE
