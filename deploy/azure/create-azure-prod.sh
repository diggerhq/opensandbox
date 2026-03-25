#!/usr/bin/env bash
# create-azure-prod.sh — Create OpenSandbox production stack on Azure
#
# Creates:
#   1. Resource group, VNet, NSG
#   2. Control plane VM (server + postgres + redis)
#   3. Worker VM (QEMU/KVM with nested virt)
#   4. Storage account (S3-compatible blob for checkpoints)
#
# Prerequisites:
#   - az CLI installed and logged in (brew install azure-cli && az login)
#   - SSH key at ~/.ssh/opensandbox-digger.pem
#
# Usage:
#   bash deploy/azure/create-azure-prod.sh create    # provision everything
#   bash deploy/azure/create-azure-prod.sh deploy    # build + deploy binaries
#   bash deploy/azure/create-azure-prod.sh status    # show IPs and status
#   bash deploy/azure/create-azure-prod.sh ssh-cp    # SSH to control plane
#   bash deploy/azure/create-azure-prod.sh ssh-wk    # SSH to worker
#   bash deploy/azure/create-azure-prod.sh destroy   # tear down everything

set -euo pipefail

# ── Configuration ──
REGION="westus2"
RG="opensandbox-prod"
VNET="osb-vnet"
SUBNET="osb-subnet"
NSG_CP="osb-nsg-controlplane"
NSG_WK="osb-nsg-worker"
CP_VM="osb-controlplane"
WK_VM="osb-worker-1"
CP_SIZE="Standard_D2s_v5"    # 2 vCPU, 8GB — control plane
WK_SIZE="Standard_D16s_v5"   # 16 vCPU, 64GB — worker (nested virt)
WK_DISK_SIZE=256              # GB, Premium SSD for /data
STORAGE_ACCOUNT="osbcheckpoints$(date +%s | tail -c 6)"  # must be globally unique
STORAGE_CONTAINER="checkpoints"
SSH_KEY="$HOME/.ssh/opensandbox-digger.pem"
SSH_PUB=$(ssh-keygen -y -f "$SSH_KEY" 2>/dev/null)
ADMIN_USER="azureuser"
DOMAIN="dev.opensandbox.ai"

# State file to persist resource names across commands
STATE_FILE="$HOME/.opensandbox-azure-state"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

log() { echo "[azure] $*"; }
err() { echo "[azure] ERROR: $*" >&2; }

save_state() { echo "$1=$2" >> "$STATE_FILE"; }
load_state() {
    if [ -f "$STATE_FILE" ]; then
        source "$STATE_FILE"
    fi
}

# ── Create ──
cmd_create() {
    log "Creating OpenSandbox stack in $REGION..."

    # Resource group
    log "Creating resource group $RG..."
    az group create --name "$RG" --location "$REGION" -o none

    # VNet + subnet
    log "Creating VNet..."
    az network vnet create \
        --resource-group "$RG" \
        --name "$VNET" \
        --address-prefix 10.100.0.0/16 \
        --subnet-name "$SUBNET" \
        --subnet-prefix 10.100.1.0/24 \
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
        --source-address-prefixes "10.100.0.0/16" \
        --access Allow --direction Inbound -o none
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_WK" \
        --name "AllowWorkerHTTP" --priority 300 \
        --destination-port-ranges 8081 --protocol Tcp \
        --source-address-prefixes "10.100.0.0/16" \
        --access Allow --direction Inbound -o none

    # Storage account (S3-compatible for checkpoints)
    log "Creating storage account..."
    # Use a deterministic name if we already have one
    load_state
    if [ -z "${STORAGE_ACCOUNT_NAME:-}" ]; then
        STORAGE_ACCOUNT_NAME="osbckpt$(echo $RG | md5sum | head -c 8)"
    fi
    az storage account create \
        --resource-group "$RG" \
        --name "$STORAGE_ACCOUNT_NAME" \
        --location "$REGION" \
        --sku Standard_LRS \
        --kind StorageV2 \
        --access-tier Hot \
        -o none
    save_state "STORAGE_ACCOUNT_NAME" "$STORAGE_ACCOUNT_NAME"

    # Get storage key
    STORAGE_KEY=$(az storage account keys list \
        --resource-group "$RG" \
        --account-name "$STORAGE_ACCOUNT_NAME" \
        --query "[0].value" -o tsv)
    save_state "STORAGE_KEY" "$STORAGE_KEY"

    # Create blob container
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

    # Attach data disk
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
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" 'bash -s' << 'CPSETUP'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update -qq
sudo apt-get install -y -qq docker.io jq curl
sudo systemctl enable --now docker

# Postgres (tuned)
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

# Wait for Postgres
for i in $(seq 1 30); do
    sudo docker exec postgres pg_isready -U opensandbox 2>/dev/null && break
    sleep 2
done

sudo mkdir -p /etc/opensandbox /usr/local/bin
echo "Control plane provisioned."
CPSETUP

    # ── Provision Worker ──
    log "Provisioning worker..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$SCRIPT_DIR/setup-azure-host.sh" "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/setup-azure-host.sh"

    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" "bash -s" << WKSETUP
set -euo pipefail

# Format and mount data disk
if ! mountpoint -q /data 2>/dev/null; then
    # Find the unformatted data disk (usually /dev/sdc)
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

# Run host setup script
sudo bash /tmp/setup-azure-host.sh
WKSETUP

    # ── Write env files ──
    log "Writing environment files..."

    # Generate API key
    API_KEY="osb_$(openssl rand -hex 32)"
    JWT_SECRET=$(openssl rand -hex 32)
    PG_PASSWORD=$(openssl rand -hex 16)
    save_state "API_KEY" "$API_KEY"
    save_state "JWT_SECRET" "$JWT_SECRET"
    save_state "PG_PASSWORD" "$PG_PASSWORD"

    # Control plane env
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" "sudo tee /etc/opensandbox/server.env > /dev/null" << CPENV
OPENSANDBOX_MODE=server
OPENSANDBOX_PORT=8080
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:$PG_PASSWORD@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://localhost:6379
OPENSANDBOX_JWT_SECRET=$JWT_SECRET
OPENSANDBOX_API_KEY=$API_KEY
OPENSANDBOX_REGION=westus2
OPENSANDBOX_SANDBOX_DOMAIN=$DOMAIN
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
OPENSANDBOX_WORKER_ID=w-azure-westus2-1
OPENSANDBOX_REGION=westus2
OPENSANDBOX_MAX_CAPACITY=100
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
WKENV

    # Open Postgres and Redis ports on control plane to VNet
    log "Opening Postgres/Redis to VNet..."
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_CP" \
        --name "AllowPostgresFromVNet" --priority 400 \
        --destination-port-ranges 5432 --protocol Tcp \
        --source-address-prefixes "10.100.0.0/16" \
        --access Allow --direction Inbound -o none
    az network nsg rule create \
        --resource-group "$RG" --nsg-name "$NSG_CP" \
        --name "AllowRedisFromVNet" --priority 500 \
        --destination-port-ranges 6379 --protocol Tcp \
        --source-address-prefixes "10.100.0.0/16" \
        --access Allow --direction Inbound -o none

    # Postgres needs to listen on all interfaces (not just localhost)
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" << 'PGFIX'
# Allow connections from VNet
sudo docker exec postgres bash -c "echo 'host all all 10.100.0.0/16 md5' >> /var/lib/postgresql/data/pg_hba.conf"
sudo docker exec postgres bash -c "echo \"listen_addresses = '*'\" >> /var/lib/postgresql/data/conf.d/listen.conf || true"
sudo docker exec postgres mkdir -p /var/lib/postgresql/data/conf.d 2>/dev/null || true
sudo docker exec postgres bash -c "echo \"listen_addresses = '*'\" > /var/lib/postgresql/data/conf.d/listen.conf"
sudo docker restart postgres
PGFIX

    log ""
    log "=== Stack Created ==="
    log "Control plane: $CP_PUBLIC_IP (private: $CP_PRIVATE_IP)"
    log "Worker:        $WK_PUBLIC_IP (private: $WK_PRIVATE_IP)"
    log "Storage:       $STORAGE_ACCOUNT_NAME.blob.core.windows.net/$STORAGE_CONTAINER"
    log "API key:       $API_KEY"
    log "Domain:        $DOMAIN → point DNS A record to $CP_PUBLIC_IP"
    log ""
    log "Next: bash deploy/azure/create-azure-prod.sh deploy"
}

# ── Deploy ──
cmd_deploy() {
    load_state
    log "Building and deploying binaries..."

    cd "$PROJECT_ROOT"

    # Build both binaries
    log "Building server (amd64)..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/opensandbox-server ./cmd/server/

    log "Building worker (amd64)..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/opensandbox-worker ./cmd/worker/

    log "Building agent (amd64)..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/osb-agent ./cmd/agent/

    # Deploy server to control plane
    log "Deploying to control plane ($CP_PUBLIC_IP)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        /tmp/opensandbox-server "$ADMIN_USER@$CP_PUBLIC_IP:/tmp/opensandbox-server"
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" \
        'sudo cp /tmp/opensandbox-server /usr/local/bin/ && sudo systemctl restart opensandbox-server'

    # Deploy worker + agent to worker
    log "Deploying to worker ($WK_PUBLIC_IP)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        /tmp/opensandbox-worker /tmp/osb-agent \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/"
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" << 'DEPLOY'
set -euo pipefail
sudo cp /tmp/opensandbox-worker /usr/local/bin/
sudo cp /tmp/osb-agent /usr/local/bin/

# Build rootfs if not exists
if [ ! -f /data/firecracker/images/default.ext4 ]; then
    echo "Building rootfs image..."
    sudo mkdir -p /data/firecracker/images
    sudo /usr/local/bin/osb-agent --version 2>/dev/null || true
    # Use build-rootfs-docker.sh if available
    if [ -f /tmp/build-rootfs-docker.sh ]; then
        sudo bash /tmp/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default
    else
        echo "WARNING: No rootfs build script found. Upload deploy/ec2/build-rootfs-docker.sh"
    fi
fi

sudo systemctl restart opensandbox-worker
DEPLOY

    # Upload rootfs build script
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$PROJECT_ROOT/deploy/ec2/build-rootfs-docker.sh" \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/build-rootfs-docker.sh"

    # Also upload Dockerfile
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$PROJECT_ROOT/deploy/firecracker/rootfs/Dockerfile.default" \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/Dockerfile.default" 2>/dev/null || true

    log ""
    log "=== Deployed ==="
    log "Server: sudo systemctl status opensandbox-server"
    log "Worker: sudo systemctl status opensandbox-worker"
    log ""
    log "Build rootfs on worker if not done:"
    log "  ssh -i $SSH_KEY $ADMIN_USER@$WK_PUBLIC_IP"
    log "  sudo bash /tmp/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default"
    log ""
    log "Test: curl -s http://$CP_PUBLIC_IP:8080/api/sandboxes -H 'X-API-Key: $API_KEY'"
}

# ── Status ──
cmd_status() {
    load_state
    log "=== OpenSandbox Azure Stack ==="
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
    log "Storage: ${STORAGE_ACCOUNT_NAME:-unknown}.blob.core.windows.net/$STORAGE_CONTAINER"
    log "API Key: ${API_KEY:-unknown}"
    log "Domain:  $DOMAIN → ${CP_PUBLIC_IP:-?}"
    log ""
    log "SSH:"
    log "  Control plane: ssh -i $SSH_KEY $ADMIN_USER@${CP_PUBLIC_IP:-?}"
    log "  Worker:        ssh -i $SSH_KEY $ADMIN_USER@${WK_PUBLIC_IP:-?}"
}

# ── SSH shortcuts ──
cmd_ssh_cp() {
    load_state
    exec ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP"
}

cmd_ssh_wk() {
    load_state
    exec ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP"
}

# ── Destroy ──
cmd_destroy() {
    log "This will DELETE the entire resource group '$RG' and all resources."
    read -p "Are you sure? (yes/no): " confirm
    if [ "$confirm" != "yes" ]; then
        log "Aborted."
        exit 0
    fi
    az group delete --name "$RG" --yes --no-wait
    rm -f "$STATE_FILE"
    log "Resource group $RG deletion started (async)."
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
