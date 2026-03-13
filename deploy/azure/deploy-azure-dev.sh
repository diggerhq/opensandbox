#!/usr/bin/env bash
# deploy-azure-dev.sh — Quick dev deployment on Azure with QEMU backend.
# Usage: ./deploy-azure-dev.sh [create|deploy|ssh|status|destroy]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# --- Defaults ---
AZURE_LOCATION="${AZURE_LOCATION:-eastus}"
AZURE_VM_SIZE="${AZURE_VM_SIZE:-Standard_D48as_v6}"
AZURE_RG="${AZURE_RG:-opensandbox-dev}"
AZURE_VM_NAME="${AZURE_VM_NAME:-opensandbox-dev}"
AZURE_IMAGE="${AZURE_IMAGE:-Canonical:ubuntu-24_04-lts:server:latest}"
AZURE_ADMIN_USER="${AZURE_ADMIN_USER:-ubuntu}"
AZURE_DATA_DISK_GB="${AZURE_DATA_DISK_GB:-500}"
OPENSANDBOX_API_KEY="${OPENSANDBOX_API_KEY:-opensandbox-dev}"
SSH_KEY_PATH="${SSH_KEY_PATH:-$HOME/.ssh/id_rsa}"

STATE_DIR="$SCRIPT_DIR"
STATE_FILE="$STATE_DIR/.dev-env-state-${AZURE_LOCATION}"

# --- Helpers ---
log() { echo "$(date '+%H:%M:%S') [azure-dev] $*"; }

save_state() { echo "$1=$2" >> "$STATE_FILE"; }
load_state() {
    if [ -f "$STATE_FILE" ]; then
        # shellcheck disable=SC1090
        source "$STATE_FILE"
    fi
}

ssh_cmd() {
    load_state
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -i "$SSH_KEY_PATH" "${AZURE_ADMIN_USER}@${VM_PUBLIC_IP}" "$@"
}

# --- Create ---
cmd_create() {
    log "Creating Azure dev environment in $AZURE_LOCATION..."

    # Clean state
    rm -f "$STATE_FILE"

    # Create resource group
    log "Creating resource group $AZURE_RG..."
    az group create --name "$AZURE_RG" --location "$AZURE_LOCATION" --output none
    save_state "RESOURCE_GROUP" "$AZURE_RG"

    # Create VM with public IP and data disk
    log "Creating VM $AZURE_VM_NAME ($AZURE_VM_SIZE)..."
    VM_INFO=$(az vm create \
        --resource-group "$AZURE_RG" \
        --name "$AZURE_VM_NAME" \
        --image "$AZURE_IMAGE" \
        --size "$AZURE_VM_SIZE" \
        --admin-username "$AZURE_ADMIN_USER" \
        --ssh-key-values "${SSH_KEY_PATH}.pub" \
        --os-disk-size-gb 100 \
        --data-disk-sizes-gb "$AZURE_DATA_DISK_GB" \
        --security-type Standard \
        --public-ip-sku Standard \
        --output json)

    VM_PUBLIC_IP=$(echo "$VM_INFO" | jq -r '.publicIpAddress')
    save_state "VM_PUBLIC_IP" "$VM_PUBLIC_IP"
    save_state "VM_NAME" "$AZURE_VM_NAME"
    log "VM created with public IP: $VM_PUBLIC_IP"

    # Open ports: SSH (22), API (8080), Worker SDK (8081)
    # SSH port is already opened by az vm create. Open API and worker ports.
    log "Opening ports 8080, 8081..."
    az vm open-port --resource-group "$AZURE_RG" --name "$AZURE_VM_NAME" \
        --port 8080,8081 --priority 1001 --output none

    # Wait for SSH
    log "Waiting for SSH readiness..."
    for i in $(seq 1 60); do
        if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o ConnectTimeout=5 -i "$SSH_KEY_PATH" \
            "${AZURE_ADMIN_USER}@${VM_PUBLIC_IP}" "echo ready" 2>/dev/null; then
            break
        fi
        sleep 5
    done

    # Format and mount data disk
    log "Setting up data disk..."
    ssh_cmd << 'SETUP_DISK'
set -euo pipefail
# Find the unformatted data disk (typically /dev/sdc on Azure)
DATA_DISK=""
for disk in /dev/sd{c,d,e}; do
    if [ -b "$disk" ] && ! blkid "$disk" > /dev/null 2>&1; then
        DATA_DISK="$disk"
        break
    fi
done
if [ -z "$DATA_DISK" ]; then
    # Try partitioned disk
    for disk in /dev/sd{c,d,e}1; do
        if [ -b "$disk" ] && ! blkid "$disk" > /dev/null 2>&1; then
            DATA_DISK="$disk"
            break
        fi
    done
fi
if [ -z "$DATA_DISK" ]; then
    echo "WARNING: No unformatted data disk found, using root volume"
    sudo mkdir -p /data
    exit 0
fi
echo "Formatting $DATA_DISK as ext4..."
sudo mkfs.ext4 -L opensandbox-data "$DATA_DISK"
sudo mkdir -p /data
echo "LABEL=opensandbox-data /data ext4 defaults,nofail 0 2" | sudo tee -a /etc/fstab
sudo mount /data
sudo mkdir -p /data/sandboxes /data/firecracker/images /data/checkpoints
SETUP_DISK

    # Sync code and run setup
    log "Syncing codebase..."
    rsync -az --progress \
        --exclude '.git' --exclude 'bin/' --exclude 'node_modules/' \
        --exclude '.dev-env-state*' --exclude '*.ext4' --exclude 'vendor/' \
        "$REPO_ROOT/" "${AZURE_ADMIN_USER}@${VM_PUBLIC_IP}:~/opensandbox/"

    log "Running host setup..."
    ssh_cmd "cd ~/opensandbox && sudo bash deploy/azure/setup-azure-host.sh"

    log "=== Azure dev environment created ==="
    log "  VM:       $AZURE_VM_NAME ($AZURE_VM_SIZE)"
    log "  IP:       $VM_PUBLIC_IP"
    log "  SSH:      ./deploy/azure/deploy-azure-dev.sh ssh"
    log "  Deploy:   ./deploy/azure/deploy-azure-dev.sh deploy"
    log ""
    log "Next: run './deploy/azure/deploy-azure-dev.sh deploy' to build and start services"
}

# --- Deploy ---
cmd_deploy() {
    load_state
    if [ -z "${VM_PUBLIC_IP:-}" ]; then
        log "ERROR: No VM found. Run 'create' first."
        exit 1
    fi

    log "Deploying to $VM_PUBLIC_IP..."

    # Sync code
    log "Syncing code..."
    rsync -az --progress \
        --exclude '.git' --exclude 'bin/' --exclude 'node_modules/' \
        --exclude '.dev-env-state*' --exclude '*.ext4' --exclude 'vendor/' \
        "$REPO_ROOT/" "${AZURE_ADMIN_USER}@${VM_PUBLIC_IP}:~/opensandbox/"

    # Build binaries on instance
    log "Building binaries..."
    ssh_cmd << 'BUILD'
set -euo pipefail
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
cd ~/opensandbox

echo "Building server..."
CGO_ENABLED=0 go build -o bin/opensandbox-server ./cmd/server/
echo "Building worker..."
CGO_ENABLED=0 go build -o bin/opensandbox-worker ./cmd/worker/
echo "Building agent..."
CGO_ENABLED=0 GOARCH=amd64 go build -o bin/osb-agent ./cmd/agent/

# Stop services before overwriting binaries (avoids "text file busy")
sudo systemctl stop opensandbox-worker 2>/dev/null || true
sudo systemctl stop opensandbox-server 2>/dev/null || true

sudo cp bin/opensandbox-server /usr/local/bin/
sudo cp bin/opensandbox-worker /usr/local/bin/
sudo cp bin/osb-agent /usr/local/bin/
echo "Binaries installed."
BUILD

    # Build rootfs if needed
    ssh_cmd << 'ROOTFS'
set -euo pipefail
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
if [ ! -f /data/firecracker/images/default.ext4 ]; then
    echo "Building rootfs image..."
    cd ~/opensandbox
    sudo -E bash deploy/ec2/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default
else
    echo "Rootfs image already exists."
fi

# Patch rootfs with guest kernel modules (vsock, overlay) and insmod symlink
GUEST_MODDIR="/opt/opensandbox/guest-modules"
if [ -d "$GUEST_MODDIR" ] && [ -f /data/firecracker/images/default.ext4 ]; then
    echo "Patching rootfs with guest kernel modules..."
    MNTDIR=$(mktemp -d)
    sudo mount -o loop /data/firecracker/images/default.ext4 "$MNTDIR"

    # Copy modules
    sudo mkdir -p "$MNTDIR/lib/modules/vsock"
    sudo cp "$GUEST_MODDIR"/*.ko "$MNTDIR/lib/modules/vsock/" 2>/dev/null || true

    # Create insmod symlink (busybox applet)
    if [ -f "$MNTDIR/bin/busybox" ] && [ ! -e "$MNTDIR/sbin/insmod" ]; then
        sudo ln -sf /bin/busybox "$MNTDIR/sbin/insmod"
    fi

    # Update init script: load modules early (before overlay mount)
    if ! grep -q 'lib/modules/vsock' "$MNTDIR/sbin/init" 2>/dev/null; then
        # Insert module loading after the first "mount -t devtmpfs" line
        sudo sed -i '/^mount -t devtmpfs devtmpfs \/dev$/a\
\
# Load kernel modules (needed for QEMU with modular kernel)\
if [ -d /lib/modules/vsock ]; then\
    for mod in /lib/modules/vsock/overlay.ko /lib/modules/vsock/vsock.ko /lib/modules/vsock/vmw_vsock_virtio_transport_common.ko /lib/modules/vsock/vmw_vsock_virtio_transport.ko; do\
        [ -f "$mod" ] \&\& insmod "$mod" 2>/dev/null || true\
    done\
    echo "init: kernel modules loaded"\
fi' "$MNTDIR/sbin/init"
    fi

    sudo umount "$MNTDIR"
    rmdir "$MNTDIR"
    echo "Rootfs patched."
fi
ROOTFS

    # Setup env files
    SANDBOX_DOMAIN="${VM_PUBLIC_IP}.nip.io"
    ssh_cmd << ENVSETUP
set -euo pipefail
# Worker env
sudo tee /etc/opensandbox/worker.env > /dev/null << EOF
OPENSANDBOX_MODE=worker
OPENSANDBOX_DATA_DIR=/data/sandboxes
OPENSANDBOX_KERNEL_PATH=/opt/opensandbox/vmlinux
OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
OPENSANDBOX_VM_BACKEND=qemu
OPENSANDBOX_QEMU_BIN=qemu-system-x86_64
OPENSANDBOX_JWT_SECRET=dev-jwt-secret-change-me
OPENSANDBOX_WORKER_ID=w-azure-1
OPENSANDBOX_REGION=azure-${AZURE_LOCATION}
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://localhost:6379
OPENSANDBOX_SANDBOX_DOMAIN=${SANDBOX_DOMAIN}
OPENSANDBOX_HTTP_ADDR=http://${VM_PUBLIC_IP}:8081
OPENSANDBOX_NATS_URL=
OPENSANDBOX_PORT=8081
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=1024
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=2
EOF

# Server env
sudo tee /etc/opensandbox/server.env > /dev/null << EOF
OPENSANDBOX_MODE=server
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_JWT_SECRET=dev-jwt-secret-change-me
OPENSANDBOX_API_KEY=\$(echo -n "${OPENSANDBOX_API_KEY}" | sha256sum | cut -d' ' -f1)
OPENSANDBOX_REDIS_URL=redis://localhost:6379
OPENSANDBOX_SANDBOX_DOMAIN=${SANDBOX_DOMAIN}
OPENSANDBOX_PORT=8080
OPENSANDBOX_REGION=azure-${AZURE_LOCATION}
EOF
ENVSETUP

    # Restart services
    ssh_cmd << 'RESTART'
set -euo pipefail
# Ensure iptables rules for VM networking
sudo iptables -t nat -C POSTROUTING -s 172.16.0.0/16 -o $(ip route show default | awk '/default/ {print $5}') -j MASQUERADE 2>/dev/null || \
    sudo iptables -t nat -A POSTROUTING -s 172.16.0.0/16 -o $(ip route show default | awk '/default/ {print $5}') -j MASQUERADE
sudo iptables -C FORWARD -s 172.16.0.0/16 -j ACCEPT 2>/dev/null || \
    sudo iptables -I FORWARD -s 172.16.0.0/16 -j ACCEPT
sudo iptables -C FORWARD -d 172.16.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
    sudo iptables -I FORWARD -d 172.16.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
sudo sysctl -w net.ipv4.ip_forward=1 > /dev/null
sudo sysctl -w net.ipv4.conf.all.route_localnet=1 > /dev/null

sudo systemctl daemon-reload
sudo systemctl restart opensandbox-server || true
sleep 2
sudo systemctl restart opensandbox-worker || true
RESTART

    # Wait for server to be ready and run migrations
    log "Waiting for server..."
    for i in $(seq 1 30); do
        if ssh_cmd "curl -sf http://localhost:8080/health" 2>/dev/null; then
            break
        fi
        sleep 2
    done

    # Seed database
    ssh_cmd << SEED
set -euo pipefail
export PGPASSWORD=opensandbox
KEY_HASH=\$(echo -n "${OPENSANDBOX_API_KEY}" | sha256sum | cut -d' ' -f1)

# Create org (UUID id, table is "orgs")
psql -h localhost -U opensandbox -d opensandbox -c "
    INSERT INTO orgs (id, name, slug) VALUES ('00000000-0000-0000-0000-000000000001', 'Dev Org', 'dev')
    ON CONFLICT DO NOTHING;
" 2>/dev/null || echo "DB seed: orgs table not ready yet"

# Create API key (UUID id, requires key_prefix)
psql -h localhost -U opensandbox -d opensandbox -c "
    INSERT INTO api_keys (id, org_id, key_hash, key_prefix, name)
    VALUES ('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000001', '\${KEY_HASH}', '$(echo -n "${OPENSANDBOX_API_KEY}" | cut -c1-8)', 'dev-key')
    ON CONFLICT DO NOTHING;
" 2>/dev/null || echo "DB seed: api_keys table not ready yet"

echo "DB seeded (org + API key)"
SEED

    log "=== Deployment complete ==="
    log "  Server: http://${VM_PUBLIC_IP}:8080"
    log "  Worker: http://${VM_PUBLIC_IP}:8081"
    log "  API key: ${OPENSANDBOX_API_KEY}"
    log ""
    log "Test: curl -X POST http://${VM_PUBLIC_IP}:8080/api/sandboxes -H 'X-API-Key: ${OPENSANDBOX_API_KEY}'"
}

# --- SSH ---
cmd_ssh() {
    load_state
    if [ -z "${VM_PUBLIC_IP:-}" ]; then
        log "ERROR: No VM found. Run 'create' first."
        exit 1
    fi
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -i "$SSH_KEY_PATH" "${AZURE_ADMIN_USER}@${VM_PUBLIC_IP}"
}

# --- Status ---
cmd_status() {
    load_state
    if [ -z "${VM_PUBLIC_IP:-}" ]; then
        log "No dev environment found."
        exit 0
    fi
    log "VM: $AZURE_VM_NAME  IP: $VM_PUBLIC_IP"
    az vm show --resource-group "$AZURE_RG" --name "$AZURE_VM_NAME" \
        --show-details --query "{Status:powerState, Location:location, Size:hardwareProfile.vmSize}" \
        --output table 2>/dev/null || log "VM not found"
}

# --- Destroy ---
cmd_destroy() {
    load_state
    if [ -z "${RESOURCE_GROUP:-}" ]; then
        log "No dev environment to destroy."
        exit 0
    fi

    log "WARNING: This will destroy ALL resources in resource group $RESOURCE_GROUP"
    read -p "Continue? (y/N) " confirm
    if [ "$confirm" != "y" ] && [ "$confirm" != "Y" ]; then
        log "Aborted."
        exit 0
    fi

    log "Deleting resource group $RESOURCE_GROUP..."
    az group delete --name "$RESOURCE_GROUP" --yes --no-wait
    rm -f "$STATE_FILE"
    log "Resource group deletion initiated (async). Resources will be cleaned up shortly."
}

# --- Main ---
CMD="${1:-help}"
case "$CMD" in
    create)  cmd_create ;;
    deploy)  cmd_deploy ;;
    ssh)     cmd_ssh ;;
    status)  cmd_status ;;
    destroy) cmd_destroy ;;
    *)
        echo "Usage: $0 {create|deploy|ssh|status|destroy}"
        echo ""
        echo "Commands:"
        echo "  create   Create Azure VM and infrastructure"
        echo "  deploy   Build and deploy code to VM"
        echo "  ssh      SSH into the VM"
        echo "  status   Show VM status"
        echo "  destroy  Delete all Azure resources"
        echo ""
        echo "Environment variables:"
        echo "  AZURE_LOCATION     Azure region (default: eastus)"
        echo "  AZURE_VM_SIZE      VM size (default: Standard_D48as_v6)"
        echo "  AZURE_DATA_DISK_GB Data disk size (default: 500)"
        ;;
esac
