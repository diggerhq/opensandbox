#!/usr/bin/env bash
# create-opencomputer-prod.sh — Create OpenComputer production stack on Azure (East US 2)
#
# Creates:
#   1. Resource group, VNet, NSGs
#   2. Managed Azure PostgreSQL Flexible Server
#   3. Managed Azure Cache for Redis
#   4. Storage account (S3-compatible blob for checkpoints)
#   5. Control plane VM (server)
#   6. Worker VM (D64d_v5 — 64 vCPU, 256GB RAM, QEMU/KVM with nested virt)
#
# Prerequisites:
#   - az CLI installed and logged in (brew install azure-cli && az login)
#   - SSH key at ~/.ssh/opensandbox-digger.pem
#
# Usage:
#   bash deploy/azure/create-opencomputer-prod.sh create    # provision everything
#   bash deploy/azure/create-opencomputer-prod.sh deploy    # build + deploy binaries
#   bash deploy/azure/create-opencomputer-prod.sh status    # show IPs and status
#   bash deploy/azure/create-opencomputer-prod.sh ssh-cp    # SSH to control plane
#   bash deploy/azure/create-opencomputer-prod.sh ssh-wk    # SSH to worker
#   bash deploy/azure/create-opencomputer-prod.sh destroy   # tear down everything

set -euo pipefail

# ── Configuration ──
REGION="eastus2"
RG="opencomputer-prod"
VNET="oc-vnet"
SUBNET="oc-subnet"
NSG_CP="oc-nsg-controlplane"
NSG_WK="oc-nsg-worker"
CP_VM="oc-controlplane"
WK_VM="oc-worker-1"
CP_SIZE="Standard_D2s_v5"       # 2 vCPU, 8GB — control plane
WK_SIZE="Standard_D64d_v5"      # 64 vCPU, 256GB RAM, 2400GB temp NVMe
WK_DISK_SIZE=512                 # GB Premium SSD for /data (persistent)
STORAGE_CONTAINER="checkpoints"
SSH_KEY="$HOME/.ssh/opensandbox-digger.pem"
SSH_PUB=$(ssh-keygen -y -f "$SSH_KEY" 2>/dev/null)
ADMIN_USER="azureuser"
DOMAIN="opencomputer.dev"

STATE_FILE="$HOME/.opencomputer-prod-state"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

log() { echo "[opencomputer] $*"; }
err() { echo "[opencomputer] ERROR: $*" >&2; exit 1; }

save_state() {
    if [ -f "$STATE_FILE" ] && grep -q "^$1=" "$STATE_FILE" 2>/dev/null; then
        sed -i.bak "s|^$1=.*|$1=$2|" "$STATE_FILE"
        rm -f "${STATE_FILE}.bak"
    else
        echo "$1=$2" >> "$STATE_FILE"
    fi
}
load_state() {
    if [ -f "$STATE_FILE" ]; then
        source "$STATE_FILE"
    fi
}

# ── Create ──
cmd_create() {
    log "Creating OpenComputer production stack in $REGION..."

    # Generate secrets upfront
    API_KEY="osb_$(openssl rand -hex 32)"
    JWT_SECRET=$(openssl rand -hex 32)
    PG_PASSWORD="pg$(openssl rand -hex 16)"
    save_state "API_KEY" "$API_KEY"
    save_state "JWT_SECRET" "$JWT_SECRET"
    save_state "PG_PASSWORD" "$PG_PASSWORD"

    # ── Resource group ──
    log "Creating resource group $RG..."
    az group create --name "$RG" --location "$REGION" -o none

    # ── VNet + subnets ──
    log "Creating VNet..."
    az network vnet create \
        --resource-group "$RG" \
        --name "$VNET" \
        --address-prefix 10.200.0.0/16 \
        --subnet-name "$SUBNET" \
        --subnet-prefix 10.200.1.0/24 \
        -o none

    # ── NSGs ──
    log "Creating control plane NSG..."
    az network nsg create --resource-group "$RG" --name "$NSG_CP" -o none
    PRIO=100
    for port_name in "SSH:22" "API:8080" "HTTPS:443"; do
        name="${port_name%%:*}"
        port="${port_name##*:}"
        az network nsg rule create \
            --resource-group "$RG" --nsg-name "$NSG_CP" \
            --name "Allow${name}" --priority $PRIO \
            --destination-port-ranges "$port" --protocol Tcp \
            --access Allow --direction Inbound -o none
        PRIO=$((PRIO + 10))
    done
    # gRPC from VNet only
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_CP" \
        --name "AllowgRPCFromVNet" --priority 200 \
        --destination-port-ranges 9090 --protocol Tcp \
        --source-address-prefixes "10.200.0.0/16" \
        --access Allow --direction Inbound -o none

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
        --source-address-prefixes "10.200.0.0/16" \
        --access Allow --direction Inbound -o none
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_WK" \
        --name "AllowWorkerHTTPFromVNet" --priority 300 \
        --destination-port-ranges 8081 --protocol Tcp \
        --source-address-prefixes "10.200.0.0/16" \
        --access Allow --direction Inbound -o none

    # ── Storage Account ──
    log "Creating storage account..."
    STORAGE_ACCOUNT_NAME="occkpt$(echo $RG | md5sum | head -c 8)"
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

    # Attach persistent data disk (Premium SSD)
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

    # Open Postgres/Redis to VNet
    log "Opening Postgres/Redis ports to VNet..."
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_CP" \
        --name "AllowPostgresFromVNet" --priority 400 \
        --destination-port-ranges 5432 --protocol Tcp \
        --source-address-prefixes "10.200.0.0/16" \
        --access Allow --direction Inbound -o none
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_CP" \
        --name "AllowRedisFromVNet" --priority 500 \
        --destination-port-ranges 6379 --protocol Tcp \
        --source-address-prefixes "10.200.0.0/16" \
        --access Allow --direction Inbound -o none

    # ── Provision Control Plane ──
    log "Provisioning control plane (Docker + Postgres + Redis)..."
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" "bash -s" << CPSETUP
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq docker.io jq curl
sudo systemctl enable --now docker
sudo mkdir -p /etc/opensandbox /usr/local/bin

# PostgreSQL
sudo docker run -d --name postgres \
    --restart unless-stopped \
    --shm-size=2g \
    -p 5432:5432 \
    -e POSTGRES_USER=opensandbox \
    -e POSTGRES_PASSWORD=$PG_PASSWORD \
    -e POSTGRES_DB=opensandbox \
    -v /data/postgres:/var/lib/postgresql/data \
    postgres:16 \
    postgres \
      -c shared_buffers=1GB \
      -c work_mem=32MB \
      -c effective_cache_size=4GB \
      -c random_page_cost=1.1 \
      -c synchronous_commit=off \
      -c max_connections=200

# Redis
sudo docker run -d --name redis \
    --restart unless-stopped \
    -p 6379:6379 \
    redis:7-alpine

# Wait for Postgres to be ready
for i in \$(seq 1 30); do
    sudo docker exec postgres pg_isready -U opensandbox 2>/dev/null && break
    sleep 2
done

# Configure Postgres to listen on all interfaces for VNet access
sudo docker exec postgres bash -c "echo 'host all all 10.200.0.0/16 md5' >> /var/lib/postgresql/data/pg_hba.conf"
sudo docker exec postgres mkdir -p /var/lib/postgresql/data/conf.d 2>/dev/null || true
sudo docker exec postgres bash -c "echo \"listen_addresses = '*'\" > /var/lib/postgresql/data/conf.d/listen.conf"
sudo docker restart postgres
sleep 3

echo "Control plane provisioned (Postgres + Redis running)."
CPSETUP

    # ── Provision Worker ──
    log "Provisioning worker (QEMU/KVM setup)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$SCRIPT_DIR/setup-azure-host.sh" "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/setup-azure-host.sh"

    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" "bash -s" << WKSETUP
set -euo pipefail

# Format and mount data disk
if ! mountpoint -q /data 2>/dev/null; then
    DATA_DISK=\$(lsblk -dpno NAME,TYPE | grep disk | while read d t; do
        if ! blkid "\$d" &>/dev/null && [ "\$d" != "/dev/sda" ] && [ "\$d" != "/dev/sdb" ]; then
            echo "\$d"; break
        fi
    done)
    if [ -n "\$DATA_DISK" ]; then
        echo "Formatting \$DATA_DISK as XFS with reflink..."
        sudo mkfs.xfs -m reflink=1 "\$DATA_DISK"
        sudo mkdir -p /data
        echo "\$DATA_DISK /data xfs defaults,nofail 0 2" | sudo tee -a /etc/fstab
        sudo mount /data
    fi
fi

# Also set up local NVMe temp storage if available (D64d has 2400GB)
if ls /dev/nvme*n1 2>/dev/null | head -1 | grep -q nvme; then
    NVME=\$(ls /dev/nvme*n1 2>/dev/null | head -1)
    if [ -n "\$NVME" ] && ! mountpoint -q /data/sandboxes 2>/dev/null; then
        echo "Formatting NVMe \$NVME as XFS with reflink for sandbox data..."
        sudo mkfs.xfs -m reflink=1 -f "\$NVME"
        sudo mkdir -p /data/sandboxes
        echo "\$NVME /data/sandboxes xfs defaults,nofail 0 2" | sudo tee -a /etc/fstab
        sudo mount /data/sandboxes
    fi
fi

sudo bash /tmp/setup-azure-host.sh
WKSETUP

    # ── Write env files ──
    log "Writing environment files..."

    # Control plane env
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" "sudo tee /etc/opensandbox/server.env > /dev/null" << CPENV
OPENSANDBOX_MODE=server
OPENSANDBOX_PORT=8080
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:${PG_PASSWORD}@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://localhost:6379
OPENSANDBOX_JWT_SECRET=$JWT_SECRET
OPENSANDBOX_API_KEY=$API_KEY
OPENSANDBOX_REGION=$REGION
OPENSANDBOX_SANDBOX_DOMAIN=workers.$DOMAIN
OPENSANDBOX_CONTROLPLANE_IP=$CP_PRIVATE_IP
OPENSANDBOX_MAX_WORKERS=4
OPENSANDBOX_MIN_WORKERS=1
CPENV

    # Worker env
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" "sudo tee /etc/opensandbox/worker.env > /dev/null" << WKENV
OPENSANDBOX_MODE=worker
OPENSANDBOX_VM_BACKEND=qemu
OPENSANDBOX_QEMU_BIN=qemu-system-x86_64
OPENSANDBOX_DATA_DIR=/data/sandboxes
OPENSANDBOX_KERNEL_PATH=/opt/opensandbox/vmlinux
OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
OPENSANDBOX_GRPC_ADVERTISE=$WK_PRIVATE_IP:9090
OPENSANDBOX_HTTP_ADDR=http://$WK_PRIVATE_IP:8081
OPENSANDBOX_JWT_SECRET=$JWT_SECRET
OPENSANDBOX_WORKER_ID=w-oc-eastus2-1
OPENSANDBOX_REGION=$REGION
OPENSANDBOX_SANDBOX_DOMAIN=workers.$DOMAIN
OPENSANDBOX_MAX_CAPACITY=250
OPENSANDBOX_PORT=8081
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=1024
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=2
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:${PG_PASSWORD}@${CP_PRIVATE_IP}:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://${CP_PRIVATE_IP}:6379
OPENSANDBOX_S3_BUCKET=$STORAGE_CONTAINER
OPENSANDBOX_S3_REGION=$REGION
OPENSANDBOX_S3_ENDPOINT=https://${STORAGE_ACCOUNT_NAME}.blob.core.windows.net
OPENSANDBOX_S3_ACCESS_KEY_ID=$STORAGE_ACCOUNT_NAME
OPENSANDBOX_S3_SECRET_ACCESS_KEY=$STORAGE_KEY
OPENSANDBOX_MACHINE_ID=oc-worker-1
WKENV

    log ""
    log "=== Stack Created ==="
    log "Control plane: $CP_PUBLIC_IP (private: $CP_PRIVATE_IP)"
    log "Worker:        $WK_PUBLIC_IP (private: $WK_PRIVATE_IP)"
    log "PostgreSQL:    Docker on $CP_PRIVATE_IP:5432"
    log "Redis:         Docker on $CP_PRIVATE_IP:6379"
    log "Storage:       ${STORAGE_ACCOUNT_NAME}.blob.core.windows.net/$STORAGE_CONTAINER"
    log "API key:       $API_KEY"
    log "Domain:        $DOMAIN → point DNS A record to $CP_PUBLIC_IP"
    log ""
    log "Next: bash deploy/azure/create-opencomputer-prod.sh deploy"
}

# ── Deploy ──
cmd_deploy() {
    load_state
    log "Building and deploying binaries..."

    cd "$PROJECT_ROOT"
    VERSION=$(cat VERSION 2>/dev/null || echo "dev")
    log "Version: $VERSION"

    log "Building server (amd64)..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/oc-server ./cmd/server/

    log "Building worker (amd64)..."
    GOOS=linux GOARCH=amd64 go build -ldflags="-X main.AgentVersion=$VERSION" -o /tmp/oc-worker ./cmd/worker/

    log "Building agent (amd64)..."
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-X main.Version=$VERSION" -o /tmp/oc-agent ./cmd/agent/

    # Deploy server
    log "Deploying to control plane ($CP_PUBLIC_IP)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        /tmp/oc-server "$ADMIN_USER@$CP_PUBLIC_IP:/tmp/opensandbox-server"
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" \
        'sudo cp /tmp/opensandbox-server /usr/local/bin/ && sudo chmod +x /usr/local/bin/opensandbox-server && sudo systemctl restart opensandbox-server 2>/dev/null || true'

    # Deploy worker + agent
    log "Deploying to worker ($WK_PUBLIC_IP)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        /tmp/oc-worker /tmp/oc-agent \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/"

    # Upload rootfs build scripts
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$PROJECT_ROOT/deploy/ec2/build-rootfs-docker.sh" \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/build-rootfs-docker.sh" 2>/dev/null || true
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$PROJECT_ROOT/deploy/firecracker/rootfs/Dockerfile.default" \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/Dockerfile.default" 2>/dev/null || true

    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" << 'DEPLOY'
set -euo pipefail
sudo systemctl stop opensandbox-worker 2>/dev/null || true
sudo cp /tmp/oc-worker /usr/local/bin/opensandbox-worker
sudo cp /tmp/oc-agent /usr/local/bin/osb-agent
sudo chmod +x /usr/local/bin/opensandbox-worker /usr/local/bin/osb-agent

# Build rootfs if not exists
if [ ! -f /data/firecracker/images/default.ext4 ]; then
    echo "Building rootfs image..."
    sudo mkdir -p /data/firecracker/images
    if [ -f /tmp/build-rootfs-docker.sh ]; then
        sudo bash /tmp/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default
    else
        echo "WARNING: No rootfs build script — upload deploy/ec2/build-rootfs-docker.sh"
    fi
fi

# Delete golden to rebuild with new agent version
sudo rm -rf /data/sandboxes/golden

sudo systemctl restart opensandbox-worker 2>/dev/null || true
DEPLOY

    log ""
    log "=== Deployed (v$VERSION) ==="
    log "Test: curl -s http://$CP_PUBLIC_IP:8080/api/sandboxes -H 'X-API-Key: $API_KEY'"
}

# ── Status ──
cmd_status() {
    load_state
    log "=== OpenComputer Production Stack ==="
    log ""
    log "Region:  $REGION"
    log "Domain:  $DOMAIN"
    log ""
    log "Control Plane ($CP_SIZE):"
    log "  Public:  ${CP_PUBLIC_IP:-?}"
    log "  Private: ${CP_PRIVATE_IP:-?}"
    az vm show -d --resource-group "$RG" --name "$CP_VM" --query "{status:powerState}" -o tsv 2>/dev/null || echo "  (not found)"
    log ""
    log "Worker ($WK_SIZE):"
    log "  Public:  ${WK_PUBLIC_IP:-?}"
    log "  Private: ${WK_PRIVATE_IP:-?}"
    az vm show -d --resource-group "$RG" --name "$WK_VM" --query "{status:powerState}" -o tsv 2>/dev/null || echo "  (not found)"
    log ""
    log "PostgreSQL: ${PG_HOST:-?} (managed)"
    log "Redis:      ${REDIS_HOST:-?} (managed)"
    log "Storage:    ${STORAGE_ACCOUNT_NAME:-?}.blob.core.windows.net/$STORAGE_CONTAINER"
    log "API Key:    ${API_KEY:-?}"
    log ""
    log "SSH:"
    log "  Control plane: ssh -i $SSH_KEY $ADMIN_USER@${CP_PUBLIC_IP:-?}"
    log "  Worker:        ssh -i $SSH_KEY $ADMIN_USER@${WK_PUBLIC_IP:-?}"
}

# ── SSH shortcuts ──
cmd_ssh_cp() { load_state; exec ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP"; }
cmd_ssh_wk() { load_state; exec ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP"; }

# ── Destroy ──
cmd_destroy() {
    log "This will DELETE the entire resource group '$RG' and ALL resources (VMs, Postgres, Redis, storage)."
    read -p "Type 'destroy' to confirm: " confirm
    if [ "$confirm" != "destroy" ]; then
        log "Aborted."
        exit 0
    fi
    az group delete --name "$RG" --yes --no-wait
    rm -f "$STATE_FILE"
    log "Resource group $RG deletion started (async). This takes ~5 minutes."
}

# ── Main ──
case "${1:-}" in
    create)  cmd_create ;;
    deploy)  cmd_deploy ;;
    status)  cmd_status ;;
    ssh-cp)  cmd_ssh_cp ;;
    ssh-wk)  cmd_ssh_wk ;;
    destroy) cmd_destroy ;;
    *)
        echo "Usage: $0 {create|deploy|status|ssh-cp|ssh-wk|destroy}"
        exit 1
        ;;
esac
