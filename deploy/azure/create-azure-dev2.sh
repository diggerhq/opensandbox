#!/usr/bin/env bash
# create-azure-dev2.sh — Create a SECOND dev OpenSandbox stack on Azure.
#
# This is the CF-cutover testbed. Completely isolated from the existing dev
# cluster (OPENSANDBOX-PROD westus2) which we're keeping as a prod-replica
# for debugging real prod issues without interference.
#
# Differences from create-azure-prod.sh:
#   - Different resource group, VM names, hostname, state file (zero overlap)
#   - NVMe-capable VM SKUs (Dads_v5)
#   - Smaller (cost-optimized for an idle-most-of-the-time testbed)
#   - Cell-aware env vars baked in: CellID, CF event endpoint/secret, etc.
#
# Usage:
#   bash deploy/azure/create-azure-dev2.sh create   # provision (run once)
#   bash deploy/azure/create-azure-dev2.sh deploy   # build + deploy binaries
#   bash deploy/azure/create-azure-dev2.sh status   # show IPs and status
#   bash deploy/azure/create-azure-dev2.sh ssh-cp   # SSH to control plane
#   bash deploy/azure/create-azure-dev2.sh ssh-wk   # SSH to worker
#   bash deploy/azure/create-azure-dev2.sh start    # start both VMs (after stop)
#   bash deploy/azure/create-azure-dev2.sh stop     # deallocate VMs to save cost
#   bash deploy/azure/create-azure-dev2.sh destroy  # tear down everything

set -euo pipefail

# ── Configuration ──
REGION="westus2"
RG="opensandbox-dev2"
VNET="osb-dev2-vnet"
SUBNET="osb-dev2-subnet"
NSG_CP="osb-dev2-nsg-cp"
NSG_WK="osb-dev2-nsg-wk"
CP_VM="osb-dev2-cp"
WK_VM="osb-dev2-worker-1"

# NVMe-capable, cost-optimized for a dev testbed
CP_SIZE="Standard_D2ads_v5"   # 2 vCPU / 8GB / 75GB local NVMe — postgres + redis + CP
WK_SIZE="Standard_D4ads_v5"   # 4 vCPU / 16GB / 150GB local NVMe — nested virt

WK_DISK_SIZE=128              # GB Premium SSD attached at /data (persistent across stop/start)
STORAGE_CONTAINER="checkpoints"
SSH_KEY="$HOME/.ssh/opensandbox-digger.pem"
SSH_PUB=$(ssh-keygen -y -f "$SSH_KEY" 2>/dev/null)
ADMIN_USER="azureuser"
DOMAIN="dev2.opensandbox.ai"

# CF-cutover specifics — point dev2 at the global CF Workers we already deployed.
CELL_ID="azure-westus2-cell-b"
CF_EVENT_ENDPOINT="https://opensandbox-events-ingest-dev.brian-124.workers.dev/ingest"

# State file (separate from prod's so the two stacks never collide)
STATE_FILE="$HOME/.opensandbox-azure-dev2-state"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

log() { echo "[dev2] $*"; }
err() { echo "[dev2] ERROR: $*" >&2; }

save_state() { echo "$1=$2" >> "$STATE_FILE"; }
load_state() {
    if [ -f "$STATE_FILE" ]; then
        # shellcheck disable=SC1090
        source "$STATE_FILE"
    fi
}

# ── Create ──
cmd_create() {
    log "Creating dev2 stack in $REGION (RG=$RG)..."

    # Generate secrets up front so they can be referenced in heredocs.
    # Each create wipes prior state — re-running create makes a fresh stack.
    rm -f "$STATE_FILE"
    API_KEY="osb_$(openssl rand -hex 32)"
    JWT_SECRET=$(openssl rand -hex 32)
    PG_PASSWORD=$(openssl rand -hex 16)
    SESSION_JWT_SECRET=$(openssl rand -hex 32)
    CF_EVENT_SECRET=$(openssl rand -hex 32)
    CF_ADMIN_SECRET=$(openssl rand -hex 32)
    save_state "API_KEY" "$API_KEY"
    save_state "JWT_SECRET" "$JWT_SECRET"
    save_state "PG_PASSWORD" "$PG_PASSWORD"
    save_state "SESSION_JWT_SECRET" "$SESSION_JWT_SECRET"
    save_state "CF_EVENT_SECRET" "$CF_EVENT_SECRET"
    save_state "CF_ADMIN_SECRET" "$CF_ADMIN_SECRET"

    # Resource group
    log "Creating resource group $RG..."
    az group create --name "$RG" --location "$REGION" -o none

    # VNet + subnet — distinct CIDR from prod's 10.100.0.0/16
    log "Creating VNet (10.110.0.0/16)..."
    az network vnet create \
        --resource-group "$RG" \
        --name "$VNET" \
        --address-prefix 10.110.0.0/16 \
        --subnet-name "$SUBNET" \
        --subnet-prefix 10.110.1.0/24 \
        -o none

    # NSG: control plane (public API)
    log "Creating control plane NSG..."
    az network nsg create --resource-group "$RG" --name "$NSG_CP" -o none
    PRIO=100
    for port_name in "SSH:22" "API:8080" "gRPC:9090"; do
        name="${port_name%%:*}"
        port="${port_name##*:}"
        az network nsg rule create \
            --resource-group "$RG" --nsg-name "$NSG_CP" \
            --name "Allow${name}" --priority $PRIO \
            --destination-port-ranges "$port" --protocol Tcp \
            --access Allow --direction Inbound -o none
        PRIO=$((PRIO + 10))
    done

    # NSG: worker (only SSH + gRPC from VNet)
    log "Creating worker NSG..."
    az network nsg create --resource-group "$RG" --name "$NSG_WK" -o none
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_WK" \
        --name "AllowSSH" --priority 100 \
        --destination-port-ranges 22 --protocol Tcp \
        --access Allow --direction Inbound -o none
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_WK" \
        --name "AllowgRPCFromVNet" --priority 200 \
        --destination-port-ranges 9090 --protocol Tcp \
        --source-address-prefixes "10.110.0.0/16" \
        --access Allow --direction Inbound -o none
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_WK" \
        --name "AllowWorkerHTTP" --priority 300 \
        --destination-port-ranges 8081 --protocol Tcp \
        --source-address-prefixes "10.110.0.0/16" \
        --access Allow --direction Inbound -o none

    # Storage account (S3-compatible blob for checkpoints)
    log "Creating storage account..."
    STORAGE_ACCOUNT_NAME="osbdev2$(echo $RG | md5sum | head -c 8)"
    az storage account create \
        --resource-group "$RG" \
        --name "$STORAGE_ACCOUNT_NAME" \
        --location "$REGION" \
        --sku Standard_LRS \
        --kind StorageV2 \
        --access-tier Hot \
        -o none
    save_state "STORAGE_ACCOUNT_NAME" "$STORAGE_ACCOUNT_NAME"

    STORAGE_KEY=$(az storage account keys list \
        --resource-group "$RG" \
        --account-name "$STORAGE_ACCOUNT_NAME" \
        --query "[0].value" -o tsv)
    save_state "STORAGE_KEY" "$STORAGE_KEY"

    az storage container create \
        --name "$STORAGE_CONTAINER" \
        --account-name "$STORAGE_ACCOUNT_NAME" \
        --account-key "$STORAGE_KEY" \
        -o none 2>/dev/null || true

    # ── Control Plane VM ──
    log "Creating control plane VM ($CP_SIZE)..."
    az vm create \
        --resource-group "$RG" \
        --name "$CP_VM" \
        --size "$CP_SIZE" \
        --image Canonical:ubuntu-24_04-lts:server:latest \
        --admin-username "$ADMIN_USER" \
        --ssh-key-values "$SSH_PUB" \
        --vnet-name "$VNET" --subnet "$SUBNET" \
        --nsg "$NSG_CP" \
        --public-ip-address "${CP_VM}-ip" \
        --public-ip-sku Standard \
        --os-disk-size-gb 64 \
        --storage-sku Premium_LRS \
        -o none

    CP_PUBLIC_IP=$(az vm show -d --resource-group "$RG" --name "$CP_VM" --query publicIps -o tsv)
    CP_PRIVATE_IP=$(az vm show -d --resource-group "$RG" --name "$CP_VM" --query privateIps -o tsv)
    save_state "CP_PUBLIC_IP" "$CP_PUBLIC_IP"
    save_state "CP_PRIVATE_IP" "$CP_PRIVATE_IP"
    log "Control plane: public=$CP_PUBLIC_IP private=$CP_PRIVATE_IP"

    # ── Worker VM ──
    log "Creating worker VM ($WK_SIZE)..."
    az vm create \
        --resource-group "$RG" \
        --name "$WK_VM" \
        --size "$WK_SIZE" \
        --image Canonical:ubuntu-24_04-lts:server:latest \
        --admin-username "$ADMIN_USER" \
        --ssh-key-values "$SSH_PUB" \
        --vnet-name "$VNET" --subnet "$SUBNET" \
        --nsg "$NSG_WK" \
        --public-ip-address "${WK_VM}-ip" \
        --public-ip-sku Standard \
        --os-disk-size-gb 64 \
        --storage-sku Premium_LRS \
        -o none

    log "Attaching ${WK_DISK_SIZE}GB data disk..."
    az vm disk attach \
        --resource-group "$RG" \
        --vm-name "$WK_VM" \
        --name "${WK_VM}-data" \
        --size-gb "$WK_DISK_SIZE" \
        --sku Premium_LRS \
        --new \
        -o none

    WK_PUBLIC_IP=$(az vm show -d --resource-group "$RG" --name "$WK_VM" --query publicIps -o tsv)
    WK_PRIVATE_IP=$(az vm show -d --resource-group "$RG" --name "$WK_VM" --query privateIps -o tsv)
    save_state "WK_PUBLIC_IP" "$WK_PUBLIC_IP"
    save_state "WK_PRIVATE_IP" "$WK_PRIVATE_IP"
    log "Worker: public=$WK_PUBLIC_IP private=$WK_PRIVATE_IP"

    # ── Provision Control Plane ──
    log "Provisioning control plane..."
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" PG_PASSWORD="$PG_PASSWORD" 'bash -s' <<'CPSETUP'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq docker.io jq curl
sudo systemctl enable --now docker

sudo mkdir -p /data/postgres /etc/opensandbox /usr/local/bin

# Postgres (tuned)
sudo docker run -d --name postgres \
    --restart unless-stopped \
    --shm-size=2g \
    -p 5432:5432 \
    -e POSTGRES_USER=opensandbox \
    -e POSTGRES_PASSWORD="$PG_PASSWORD" \
    -e POSTGRES_DB=opensandbox \
    -v /data/postgres:/var/lib/postgresql/data \
    postgres:16 \
    postgres \
      -c shared_buffers=512MB \
      -c work_mem=16MB \
      -c effective_cache_size=2GB \
      -c random_page_cost=1.1 \
      -c synchronous_commit=off \
      -c max_connections=100

# Redis
sudo docker run -d --name redis \
    --restart unless-stopped \
    -p 6379:6379 \
    redis:7-alpine

# Wait for Postgres
for i in $(seq 1 30); do
    sudo docker exec postgres pg_isready -U opensandbox 2>/dev/null && break
    sleep 2
done

echo "Control plane provisioned."
CPSETUP

    # ── Provision Worker ──
    log "Provisioning worker (host setup, /data mount)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$SCRIPT_DIR/setup-azure-host.sh" "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/setup-azure-host.sh"

    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" "bash -s" <<'WKSETUP'
set -euo pipefail

# Format and mount the attached Premium SSD data disk at /data.
# Local NVMe (sdb) stays untouched — it's ephemeral and we don't rely on it.
if ! mountpoint -q /data 2>/dev/null; then
    DATA_DISK=$(lsblk -dpno NAME,TYPE | grep disk | while read d t; do
        if ! blkid "$d" &>/dev/null && [ "$d" != "/dev/sda" ] && [ "$d" != "/dev/sdb" ]; then
            echo "$d"; break
        fi
    done)
    if [ -n "$DATA_DISK" ]; then
        echo "Formatting $DATA_DISK as XFS with reflink..."
        sudo mkfs.xfs -m reflink=1 "$DATA_DISK"
        sudo mkdir -p /data
        echo "$DATA_DISK /data xfs defaults,nofail 0 2" | sudo tee -a /etc/fstab
        sudo mount /data
    fi
fi

sudo bash /tmp/setup-azure-host.sh
WKSETUP

    # ── Write env files ──
    log "Writing environment files (with CF-cutover env vars)..."

    # Control plane env — includes CF event forwarder, JWT verifier, halt reconciler
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" "sudo tee /etc/opensandbox/server.env > /dev/null" <<CPENV
OPENSANDBOX_MODE=server
OPENSANDBOX_PORT=8080
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:$PG_PASSWORD@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://localhost:6379
OPENSANDBOX_JWT_SECRET=$JWT_SECRET
OPENSANDBOX_API_KEY=$API_KEY
OPENSANDBOX_REGION=westus2
OPENSANDBOX_SANDBOX_DOMAIN=$DOMAIN
OPENSANDBOX_CELL_ID=$CELL_ID
OPENSANDBOX_CF_EVENT_ENDPOINT=$CF_EVENT_ENDPOINT
OPENSANDBOX_CF_EVENT_SECRET=$CF_EVENT_SECRET
OPENSANDBOX_CF_ADMIN_SECRET=$CF_ADMIN_SECRET
OPENSANDBOX_SESSION_JWT_SECRET=$SESSION_JWT_SECRET
CPENV

    # Worker env — includes CellID so redis_event_publisher starts
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" "sudo tee /etc/opensandbox/worker.env > /dev/null" <<WKENV
OPENSANDBOX_MODE=worker
OPENSANDBOX_VM_BACKEND=qemu
OPENSANDBOX_QEMU_BIN=qemu-system-x86_64
OPENSANDBOX_DATA_DIR=/data/sandboxes
OPENSANDBOX_KERNEL_PATH=/opt/opensandbox/vmlinux
OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
OPENSANDBOX_GRPC_ADVERTISE=$WK_PRIVATE_IP:9090
OPENSANDBOX_HTTP_ADDR=http://$WK_PRIVATE_IP:8081
OPENSANDBOX_JWT_SECRET=$JWT_SECRET
OPENSANDBOX_WORKER_ID=w-azure-westus2-dev2-1
OPENSANDBOX_REGION=westus2
OPENSANDBOX_MAX_CAPACITY=10
OPENSANDBOX_PORT=8081
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=1024
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=2
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:$PG_PASSWORD@$CP_PRIVATE_IP:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://$CP_PRIVATE_IP:6379
OPENSANDBOX_S3_BUCKET=$STORAGE_CONTAINER
OPENSANDBOX_S3_REGION=$REGION
OPENSANDBOX_S3_ENDPOINT=https://$STORAGE_ACCOUNT_NAME.blob.core.windows.net
OPENSANDBOX_S3_ACCESS_KEY_ID=$STORAGE_ACCOUNT_NAME
OPENSANDBOX_S3_SECRET_ACCESS_KEY=$STORAGE_KEY
OPENSANDBOX_CELL_ID=$CELL_ID
WKENV

    # Open Postgres + Redis to VNet
    log "Opening Postgres/Redis to VNet..."
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_CP" \
        --name "AllowPostgresFromVNet" --priority 400 \
        --destination-port-ranges 5432 --protocol Tcp \
        --source-address-prefixes "10.110.0.0/16" \
        --access Allow --direction Inbound -o none
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_CP" \
        --name "AllowRedisFromVNet" --priority 500 \
        --destination-port-ranges 6379 --protocol Tcp \
        --source-address-prefixes "10.110.0.0/16" \
        --access Allow --direction Inbound -o none

    # Postgres listen-on-all + pg_hba VNet allow
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" <<'PGFIX'
sudo docker exec postgres bash -c "echo 'host all all 10.110.0.0/16 md5' >> /var/lib/postgresql/data/pg_hba.conf"
sudo docker exec postgres mkdir -p /var/lib/postgresql/data/conf.d 2>/dev/null || true
sudo docker exec postgres bash -c "echo \"listen_addresses = '*'\" > /var/lib/postgresql/data/conf.d/listen.conf"
sudo docker restart postgres
PGFIX

    log ""
    log "=== dev2 stack created ==="
    log "Resource Group: $RG"
    log "Control plane:  $CP_PUBLIC_IP (private: $CP_PRIVATE_IP)"
    log "Worker:         $WK_PUBLIC_IP (private: $WK_PRIVATE_IP)"
    log "Storage:        $STORAGE_ACCOUNT_NAME.blob.core.windows.net/$STORAGE_CONTAINER"
    log "Cell ID:        $CELL_ID"
    log "API key:        $API_KEY"
    log ""
    log "Set CF EVENT_SECRET to match what's baked here:"
    log "  cd cloudflare-workers/events-ingest"
    log "  echo '$CF_EVENT_SECRET' | npx wrangler secret put EVENT_SECRET"
    log ""
    log "Optional DNS: point $DOMAIN A record to $CP_PUBLIC_IP (in CF dashboard)"
    log ""
    log "Next: bash deploy/azure/create-azure-dev2.sh deploy"
}

# ── Deploy ──
cmd_deploy() {
    load_state
    log "Building and deploying binaries to dev2..."

    cd "$PROJECT_ROOT"

    log "Building server (amd64)..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/opensandbox-server-dev2 ./cmd/server/

    log "Building worker (amd64)..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/opensandbox-worker-dev2 ./cmd/worker/

    log "Building agent (amd64)..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/osb-agent-dev2 ./cmd/agent/

    log "Deploying to control plane ($CP_PUBLIC_IP)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        /tmp/opensandbox-server-dev2 "$ADMIN_USER@$CP_PUBLIC_IP:/tmp/opensandbox-server"
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" \
        'sudo cp /tmp/opensandbox-server /usr/local/bin/ && sudo systemctl restart opensandbox-server || true'

    log "Deploying to worker ($WK_PUBLIC_IP)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        /tmp/opensandbox-worker-dev2 /tmp/osb-agent-dev2 \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/"
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" <<'DEPLOY'
set -euo pipefail
sudo cp /tmp/opensandbox-worker-dev2 /usr/local/bin/opensandbox-worker
sudo cp /tmp/osb-agent-dev2 /usr/local/bin/osb-agent

# Build rootfs if not already present
if [ ! -f /data/firecracker/images/default.ext4 ]; then
    sudo mkdir -p /data/firecracker/images
    if [ -f /tmp/build-rootfs-docker.sh ]; then
        sudo bash /tmp/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default
    else
        echo "WARNING: rootfs build script not on host; uploading..."
    fi
fi

sudo systemctl restart opensandbox-worker || true
DEPLOY

    # Upload rootfs build script (if not already there) for the next time
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$PROJECT_ROOT/deploy/ec2/build-rootfs-docker.sh" \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/build-rootfs-docker.sh"
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$PROJECT_ROOT/deploy/firecracker/rootfs/Dockerfile.default" \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/Dockerfile.default" 2>/dev/null || true

    log ""
    log "=== Deployed to dev2 ==="
    log "Server: ssh + sudo systemctl status opensandbox-server"
    log "Worker: ssh + sudo systemctl status opensandbox-worker"
    log ""
    log "Smoke test: curl -s http://$CP_PUBLIC_IP:8080/api/sandboxes -H \"X-API-Key: $API_KEY\""
}

# ── Status ──
cmd_status() {
    load_state
    log "=== OpenSandbox dev2 stack ==="
    log "Resource Group: $RG"
    log ""
    log "Control Plane:"
    log "  VM:      $CP_VM ($CP_SIZE)"
    log "  Public:  ${CP_PUBLIC_IP:-unknown}"
    log "  Private: ${CP_PRIVATE_IP:-unknown}"
    az vm show -d --resource-group "$RG" --name "$CP_VM" --query "{status:powerState}" -o tsv 2>/dev/null || echo "  (not found)"
    log ""
    log "Worker:"
    log "  VM:      $WK_VM ($WK_SIZE)"
    log "  Public:  ${WK_PUBLIC_IP:-unknown}"
    log "  Private: ${WK_PRIVATE_IP:-unknown}"
    az vm show -d --resource-group "$RG" --name "$WK_VM" --query "{status:powerState}" -o tsv 2>/dev/null || echo "  (not found)"
    log ""
    log "Storage:  ${STORAGE_ACCOUNT_NAME:-unknown}.blob.core.windows.net/$STORAGE_CONTAINER"
    log "Cell ID:  $CELL_ID"
    log "API Key:  ${API_KEY:-unknown}"
    log "Domain:   $DOMAIN → ${CP_PUBLIC_IP:-?} (set CNAME in CF dashboard if desired)"
}

cmd_ssh_cp() {
    load_state
    exec ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP"
}

cmd_ssh_wk() {
    load_state
    exec ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP"
}

# Stop both VMs (deallocate — billing pauses for compute, disks still cost a little)
cmd_stop() {
    log "Deallocating dev2 VMs (use 'start' to bring back up)..."
    az vm deallocate --resource-group "$RG" --name "$CP_VM" --no-wait
    az vm deallocate --resource-group "$RG" --name "$WK_VM" --no-wait
    log "Deallocation initiated. Run 'status' to check progress."
}

cmd_start() {
    log "Starting dev2 VMs..."
    az vm start --resource-group "$RG" --name "$CP_VM" --no-wait
    az vm start --resource-group "$RG" --name "$WK_VM" --no-wait
    log "Start initiated. Public IPs reassigned automatically (Standard SKU pins them)."
}

cmd_destroy() {
    log "This will DELETE the entire resource group '$RG' and all dev2 resources."
    read -p "Are you sure? (yes/no): " confirm
    if [ "$confirm" != "yes" ]; then
        log "Aborted."
        exit 0
    fi
    az group delete --name "$RG" --yes --no-wait
    rm -f "$STATE_FILE"
    log "Resource group $RG deletion started (async)."
}

case "${1:-}" in
    create)  cmd_create ;;
    deploy)  cmd_deploy ;;
    status)  cmd_status ;;
    ssh-cp)  cmd_ssh_cp ;;
    ssh-wk)  cmd_ssh_wk ;;
    start)   cmd_start ;;
    stop)    cmd_stop ;;
    destroy) cmd_destroy ;;
    *)
        echo "Usage: $0 {create|deploy|status|ssh-cp|ssh-wk|start|stop|destroy}"
        exit 1
        ;;
esac
