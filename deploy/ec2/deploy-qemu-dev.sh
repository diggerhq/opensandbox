#!/usr/bin/env bash
# deploy-qemu-dev.sh — Deploy QEMU-based dev environment on c5d.metal spot in AWS
#
# Uses existing VPC/SG/key infrastructure from the opensandbox AWS account.
# Launches a c5d.metal spot instance with NVMe formatted as XFS (reflink for qcow2).
# Reuses deploy/azure/setup-azure-host.sh for QEMU/KVM provisioning.
#
# Usage:
#   ./deploy/ec2/deploy-qemu-dev.sh [create|deploy|ssh|status|stop|start|destroy]
#
# Configuration (env vars):
#   AWS_REGION          — AWS region (default: us-east-2)
#   AWS_PROFILE         — AWS CLI profile (default: digger)
#   INSTANCE_TYPE       — EC2 instance type (default: c5d.metal)
#   KEY_NAME            — EC2 key pair name (default: opensandbox-digger)
#   SSH_KEY             — Path to SSH private key (default: ~/.ssh/opensandbox-digger.pem)
#   SECURITY_GROUP      — Security group ID (default: sg-01e14f56c4c50b6c5)
#   SUBNET_ID           — Subnet ID (default: subnet-0cbc1cca8b2bfa8bc, us-east-2b)
#   API_KEY             — API key for the server (default: test-dev-key)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# --- Defaults ---
AWS_REGION="${AWS_REGION:-us-east-2}"
AWS_PROFILE="${AWS_PROFILE:-digger}"
INSTANCE_TYPE="${INSTANCE_TYPE:-c5d.metal}"
KEY_NAME="${KEY_NAME:-opensandbox-digger}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/opensandbox-digger.pem}"
SECURITY_GROUP="${SECURITY_GROUP:-sg-01e14f56c4c50b6c5}"
SUBNET_ID="${SUBNET_ID:-subnet-0cbc1cca8b2bfa8bc}"
API_KEY="${API_KEY:-test-dev-key}"
PROJECT_TAG="opensandbox-qemu-dev"

STATE_FILE="$SCRIPT_DIR/.qemu-dev-state-${AWS_REGION}"

# --- Helpers ---
log()  { echo "$(date '+%H:%M:%S') [qemu-dev] $*"; }
err()  { echo "$(date '+%H:%M:%S') [qemu-dev] ERROR: $*" >&2; exit 1; }

aws_cmd() { aws --profile "$AWS_PROFILE" --region "$AWS_REGION" "$@"; }

save_state() {
    local key="$1" value="$2"
    if [ -f "$STATE_FILE" ] && grep -q "^${key}=" "$STATE_FILE" 2>/dev/null; then
        sed -i.bak "s|^${key}=.*|${key}=${value}|" "$STATE_FILE"
        rm -f "${STATE_FILE}.bak"
    else
        echo "${key}=${value}" >> "$STATE_FILE"
    fi
}

load_state() {
    local key="$1"
    if [ -f "$STATE_FILE" ]; then
        grep "^${key}=" "$STATE_FILE" 2>/dev/null | cut -d= -f2- || true
    fi
}

ssh_cmd() {
    local ip
    ip=$(load_state PUBLIC_IP)
    [ -n "$ip" ] || err "No instance IP found. Run 'create' first."
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -i "$SSH_KEY" "ubuntu@${ip}" "$@"
}

lookup_x86_ami() {
    echo "Looking up latest Ubuntu 24.04 x86_64 AMI in ${AWS_REGION}..." >&2
    local ami_id
    ami_id=$(aws_cmd ec2 describe-images \
        --owners 099720109477 \
        --filters \
            "Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*" \
            "Name=architecture,Values=x86_64" \
            "Name=state,Values=available" \
        --query 'Images | sort_by(@, &CreationDate) | [-1].ImageId' \
        --output text)
    if [ -z "$ami_id" ] || [ "$ami_id" = "None" ]; then
        err "Could not find Ubuntu 24.04 x86_64 AMI in ${AWS_REGION}"
    fi
    echo "$ami_id"
}

# --- Create ---
cmd_create() {
    local existing_instance
    existing_instance=$(load_state INSTANCE_ID)
    if [ -n "$existing_instance" ]; then
        local state
        state=$(aws_cmd ec2 describe-instances \
            --instance-ids "$existing_instance" \
            --query 'Reservations[0].Instances[0].State.Name' \
            --output text 2>/dev/null || echo "not-found")
        if [ "$state" = "running" ] || [ "$state" = "pending" ]; then
            log "Instance $existing_instance already exists (state: $state)"
            cmd_status
            return 0
        fi
    fi

    # Get AMI
    local ami_id
    ami_id=$(lookup_x86_ami)
    log "AMI: $ami_id"

    # Launch on-demand instance
    log "Launching ${INSTANCE_TYPE} on-demand instance..."
    local instance_id
    instance_id=$(aws_cmd ec2 run-instances \
        --image-id "$ami_id" \
        --instance-type "$INSTANCE_TYPE" \
        --key-name "$KEY_NAME" \
        --subnet-id "$SUBNET_ID" \
        --security-group-ids "$SECURITY_GROUP" \
        --block-device-mappings \
            'DeviceName=/dev/sda1,Ebs={VolumeSize=50,VolumeType=gp3,DeleteOnTermination=true}' \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${PROJECT_TAG}},{Key=Project,Value=opensandbox}]" \
        --query 'Instances[0].InstanceId' --output text)
    save_state INSTANCE_ID "$instance_id"
    log "Instance: $instance_id (on-demand)"

    # Wait for running
    log "Waiting for instance to be running..."
    aws_cmd ec2 wait instance-running --instance-ids "$instance_id"

    local public_ip
    public_ip=$(aws_cmd ec2 describe-instances \
        --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    save_state PUBLIC_IP "$public_ip"
    log "Public IP: $public_ip"

    # Wait for SSH (bare-metal takes a while)
    log "Waiting for SSH (bare-metal boot takes a few minutes)..."
    for i in $(seq 1 90); do
        if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes \
            -i "$SSH_KEY" "ubuntu@$public_ip" "echo ready" &>/dev/null; then
            log "SSH ready after ~$((i * 10))s"
            break
        fi
        if [ "$i" -eq 90 ]; then
            err "SSH not ready after 900s. Check instance console."
        fi
        sleep 10
    done

    # Format NVMe drives as XFS with reflink and mount at /data
    log "Setting up NVMe storage (XFS with reflink)..."
    ssh_cmd << 'SETUP_NVME'
set -euo pipefail

# Find all NVMe instance store drives (not the root EBS volume)
NVME_DRIVES=()
for dev in /dev/nvme{0,1,2,3,4,5,6,7}n1; do
    [ -b "$dev" ] || continue
    # Skip the root volume (has partitions or is mounted)
    if lsblk -no MOUNTPOINT "$dev" 2>/dev/null | grep -q '/'; then
        continue
    fi
    # Skip if any partition of this device is mounted
    if lsblk -no MOUNTPOINT "${dev}"* 2>/dev/null | grep -q '/'; then
        continue
    fi
    NVME_DRIVES+=("$dev")
done

echo "Found ${#NVME_DRIVES[@]} NVMe instance store drive(s): ${NVME_DRIVES[*]}"

if [ ${#NVME_DRIVES[@]} -eq 0 ]; then
    echo "WARNING: No NVMe instance store drives found, using root volume"
    sudo mkdir -p /data
    exit 0
fi

if [ ${#NVME_DRIVES[@]} -eq 1 ]; then
    # Single drive — format directly
    TARGET="${NVME_DRIVES[0]}"
    echo "Formatting $TARGET as XFS with reflink..."
    sudo mkfs.xfs -f -m reflink=1 "$TARGET"
else
    # Multiple drives — create mdadm RAID-0 stripe
    echo "Creating RAID-0 across ${#NVME_DRIVES[@]} drives..."
    sudo apt-get install -y -qq mdadm
    sudo mdadm --create /dev/md0 --level=0 --raid-devices=${#NVME_DRIVES[@]} "${NVME_DRIVES[@]}" --force --run
    sudo mdadm --detail --scan | sudo tee -a /etc/mdadm/mdadm.conf
    echo "Formatting /dev/md0 as XFS with reflink..."
    sudo mkfs.xfs -f -m reflink=1 /dev/md0
    TARGET="/dev/md0"
fi

sudo mkdir -p /data
sudo mount "$TARGET" /data

# Add to fstab (use UUID for reliability)
UUID=$(sudo blkid -s UUID -o value "$TARGET")
echo "UUID=$UUID /data xfs defaults,nofail 0 2" | sudo tee -a /etc/fstab

sudo mkdir -p /data/sandboxes /data/firecracker/images /data/checkpoints
echo "NVMe mounted at /data (XFS with reflink)"
df -h /data
SETUP_NVME

    # Sync code and run QEMU host setup
    log "Syncing codebase..."
    rsync -az --progress \
        -e "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i $SSH_KEY" \
        --exclude '.git' --exclude 'bin/' --exclude 'node_modules/' \
        --exclude '.claude/' --exclude '*.ext4' \
        "$PROJECT_ROOT/" "ubuntu@${public_ip}:~/opensandbox/"

    log "Running QEMU host setup (setup-azure-host.sh)..."
    ssh_cmd "cd ~/opensandbox && sudo bash deploy/azure/setup-azure-host.sh"

    log ""
    log "=== c5d.metal spot instance created ==="
    log "  Instance: $instance_id ($INSTANCE_TYPE, spot)"
    log "  IP:       $public_ip"
    log "  SSH:      $0 ssh"
    log "  Deploy:   $0 deploy"
    log ""
    log "Next: run '$0 deploy' to build and start services"
}

# --- Deploy ---
cmd_deploy() {
    local public_ip
    public_ip=$(load_state PUBLIC_IP)
    [ -n "$public_ip" ] || err "No instance found. Run 'create' first."

    # Verify reachable
    if ! ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes \
        -i "$SSH_KEY" "ubuntu@$public_ip" "echo ok" &>/dev/null; then
        err "Cannot reach $public_ip via SSH. Instance may be terminated (spot) or stopped."
    fi

    local branch
    branch=$(git -C "$PROJECT_ROOT" rev-parse --abbrev-ref HEAD)
    log "Deploying branch '$branch' to $public_ip..."

    # Sync code
    log "Syncing code..."
    rsync -az --progress \
        -e "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i $SSH_KEY" \
        --exclude '.git' --exclude 'bin/' --exclude 'node_modules/' \
        --exclude '.claude/' --exclude '*.ext4' \
        "$PROJECT_ROOT/" "ubuntu@${public_ip}:~/opensandbox/"

    # Build binaries on instance
    log "Building binaries on instance..."
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
sudo chmod +x /usr/local/bin/opensandbox-server /usr/local/bin/opensandbox-worker /usr/local/bin/osb-agent
echo "Binaries installed."
BUILD

    # Build rootfs if needed
    log "Building rootfs (if needed)..."
    ssh_cmd << 'ROOTFS'
set -euo pipefail
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
if [ ! -f /data/firecracker/images/default.ext4 ]; then
    echo "Building rootfs image..."
    cd ~/opensandbox
    sudo -E bash deploy/ec2/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default
else
    echo "Rootfs image already exists (delete /data/firecracker/images/default.ext4 to rebuild)."
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
    echo "Rootfs patched with kernel modules."
fi
ROOTFS

    # Setup env files
    log "Installing env files..."
    local private_ip
    private_ip=$(aws_cmd ec2 describe-instances \
        --instance-ids "$(load_state INSTANCE_ID)" \
        --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
    local sandbox_domain="${public_ip}.nip.io"

    ssh_cmd << ENVSETUP
set -euo pipefail
# Worker env
sudo tee /etc/opensandbox/worker.env > /dev/null << EOF
OPENSANDBOX_MODE=worker
OPENSANDBOX_VM_BACKEND=qemu
OPENSANDBOX_QEMU_BIN=qemu-system-x86_64
OPENSANDBOX_DATA_DIR=/data/sandboxes
OPENSANDBOX_KERNEL_PATH=/opt/opensandbox/vmlinux
OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
OPENSANDBOX_GRPC_ADVERTISE=${private_ip}:9090
OPENSANDBOX_HTTP_ADDR=http://${private_ip}:8081
OPENSANDBOX_JWT_SECRET=dev-jwt-secret
OPENSANDBOX_WORKER_ID=w-qemu-dev-1
OPENSANDBOX_REGION=use2
OPENSANDBOX_MAX_CAPACITY=10
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://localhost:6379
OPENSANDBOX_SANDBOX_DOMAIN=${sandbox_domain}
OPENSANDBOX_PORT=8081
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=1024
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=2
OPENSANDBOX_NATS_URL=
OPENSANDBOX_S3_BUCKET=opensandbox-checkpoints
OPENSANDBOX_S3_REGION=us-east-2
OPENSANDBOX_S3_ACCESS_KEY_ID=${S3_ACCESS_KEY_ID}
OPENSANDBOX_S3_SECRET_ACCESS_KEY=${S3_SECRET_ACCESS_KEY}
EOF

# Server env
sudo tee /etc/opensandbox/server.env > /dev/null << EOF
OPENSANDBOX_MODE=server
OPENSANDBOX_API_KEY=\$(echo -n "${API_KEY}" | sha256sum | cut -d' ' -f1)
OPENSANDBOX_JWT_SECRET=dev-jwt-secret
OPENSANDBOX_HTTP_ADDR=http://0.0.0.0:8080
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:opensandbox@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://localhost:6379
OPENSANDBOX_SANDBOX_DOMAIN=${sandbox_domain}
OPENSANDBOX_PORT=8080
OPENSANDBOX_REGION=use2
EOF
ENVSETUP

    # Start services + iptables
    log "Starting services..."
    ssh_cmd << 'RESTART'
set -euo pipefail
# iptables for VM networking (172.16.0.0/16 QEMU guest subnet)
DEFAULT_IFACE=$(ip route show default | awk '/default/ {print $5}' | head -1)
sudo iptables -t nat -C POSTROUTING -s 172.16.0.0/16 -o "$DEFAULT_IFACE" -j MASQUERADE 2>/dev/null || \
    sudo iptables -t nat -A POSTROUTING -s 172.16.0.0/16 -o "$DEFAULT_IFACE" -j MASQUERADE
sudo iptables -C FORWARD -s 172.16.0.0/16 -j ACCEPT 2>/dev/null || \
    sudo iptables -I FORWARD -s 172.16.0.0/16 -j ACCEPT
sudo iptables -C FORWARD -d 172.16.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
    sudo iptables -I FORWARD -d 172.16.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
sudo sysctl -w net.ipv4.ip_forward=1 > /dev/null
sudo sysctl -w net.ipv4.conf.all.route_localnet=1 > /dev/null

# Redirect port 80 -> 8080 so UI is accessible on standard HTTP port
sudo iptables -t nat -C PREROUTING -p tcp --dport 80 -j REDIRECT --to-port 8080 2>/dev/null || \
    sudo iptables -t nat -A PREROUTING -p tcp --dport 80 -j REDIRECT --to-port 8080

sudo systemctl daemon-reload
sudo systemctl restart opensandbox-server || true
sleep 2
sudo systemctl restart opensandbox-worker || true
echo "Services started."
RESTART

    # Wait for server + seed database
    log "Waiting for server..."
    for i in $(seq 1 30); do
        if ssh_cmd "curl -sf http://localhost:8080/health" 2>/dev/null; then
            break
        fi
        sleep 2
    done

    log "Seeding database..."
    ssh_cmd << SEED
set -euo pipefail
export PGPASSWORD=opensandbox
KEY_HASH=\$(echo -n "${API_KEY}" | sha256sum | cut -d' ' -f1)

# Wait for migrations
for i in \$(seq 1 15); do
    psql -h localhost -U opensandbox -d opensandbox -q -c 'SELECT 1 FROM orgs LIMIT 0' 2>/dev/null && break
    echo "Waiting for migrations..."
    sleep 2
done

# Create org
psql -h localhost -U opensandbox -d opensandbox -c "
    INSERT INTO orgs (id, name, slug) VALUES ('00000000-0000-0000-0000-000000000001', 'Dev Org', 'dev')
    ON CONFLICT DO NOTHING;
" 2>/dev/null || echo "DB seed: orgs insert failed (may already exist)"

# Create API key
psql -h localhost -U opensandbox -d opensandbox -c "
    INSERT INTO api_keys (id, org_id, key_hash, key_prefix, name)
    VALUES ('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000001', '\${KEY_HASH}', '$(echo -n "${API_KEY}" | cut -c1-8)', 'dev-key')
    ON CONFLICT DO NOTHING;
" 2>/dev/null || echo "DB seed: api_keys insert failed (may already exist)"

echo "DB seeded (org + API key)"
SEED

    log ""
    log "=== Deployment complete ==="
    log "  Server: http://${public_ip}:8080"
    log "  Worker: http://${public_ip}:8081"
    log "  API key: ${API_KEY}"
    log ""
    log "Test:"
    log "  curl -X POST http://${public_ip}:8080/api/sandboxes -H 'X-API-Key: ${API_KEY}'"
}

# --- SSH ---
cmd_ssh() {
    local public_ip
    public_ip=$(load_state PUBLIC_IP)
    [ -n "$public_ip" ] || err "No instance found. Run 'create' first."

    shift 2>/dev/null || true
    if [ $# -gt 0 ] && [ "$1" = "--" ]; then
        shift
        ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -i "$SSH_KEY" "ubuntu@$public_ip" "$@"
    else
        ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -i "$SSH_KEY" "ubuntu@$public_ip"
    fi
}

# --- Status ---
cmd_status() {
    local instance_id
    instance_id=$(load_state INSTANCE_ID)
    if [ -z "$instance_id" ]; then
        log "No QEMU dev environment found. Run '$0 create' to set one up."
        return 0
    fi

    local state public_ip
    state=$(aws_cmd ec2 describe-instances \
        --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].State.Name' \
        --output text 2>/dev/null || echo "not-found")

    # Refresh IP if running
    if [ "$state" = "running" ]; then
        public_ip=$(aws_cmd ec2 describe-instances \
            --instance-ids "$instance_id" \
            --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
        save_state PUBLIC_IP "$public_ip"
    else
        public_ip=$(load_state PUBLIC_IP)
    fi

    echo ""
    echo "  Instance:  $instance_id"
    echo "  Type:      $INSTANCE_TYPE"
    echo "  Region:    $AWS_REGION"
    echo "  State:     $state"
    echo "  Public IP: ${public_ip:-n/a}"
    echo ""
    if [ "$state" = "running" ]; then
        echo "  Server:  http://${public_ip}:8080"
        echo "  Worker:  http://${public_ip}:8081"
        echo "  SSH:     $0 ssh"
        echo "  Logs:    $0 ssh -- sudo journalctl -u opensandbox-worker -f"
    fi
    echo ""
}

# --- Stop (not applicable for spot, but keep for on-demand fallback) ---
cmd_stop() {
    local instance_id
    instance_id=$(load_state INSTANCE_ID)
    [ -n "$instance_id" ] || err "No instance found."

    log "WARNING: Spot instances cannot be stopped (only terminated)."
    log "To terminate, use: $0 destroy"
    log ""
    log "If this is an on-demand instance, stopping..."
    aws_cmd ec2 stop-instances --instance-ids "$instance_id" || true
}

# --- Start ---
cmd_start() {
    local instance_id
    instance_id=$(load_state INSTANCE_ID)
    [ -n "$instance_id" ] || err "No instance found."

    log "Starting instance $instance_id..."
    aws_cmd ec2 start-instances --instance-ids "$instance_id"
    aws_cmd ec2 wait instance-running --instance-ids "$instance_id"

    local public_ip
    public_ip=$(aws_cmd ec2 describe-instances \
        --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    save_state PUBLIC_IP "$public_ip"
    log "Instance running. Public IP: $public_ip"
}

# --- Destroy ---
cmd_destroy() {
    local instance_id
    instance_id=$(load_state INSTANCE_ID)
    if [ -z "$instance_id" ]; then
        log "No instance found."
        return 0
    fi

    echo "This will TERMINATE instance $instance_id (all NVMe data will be lost)."
    read -r -p "Are you sure? (y/N) " confirm
    if [[ ! "$confirm" =~ ^[yY]$ ]]; then
        echo "Cancelled."
        return 0
    fi

    log "Terminating instance $instance_id..."
    aws_cmd ec2 terminate-instances --instance-ids "$instance_id" > /dev/null
    rm -f "$STATE_FILE"
    log "Instance terminated. State file cleaned up."
}

# --- Stop old a1.metal ---
cmd_stop_old() {
    local old_instance_id="${1:-}"
    if [ -z "$old_instance_id" ]; then
        echo "Usage: $0 stop-old <instance-id>"
        echo "Example: $0 stop-old i-0e0e5d9a883add97f"
        exit 1
    fi

    log "Stopping old a1.metal instance $old_instance_id (EBS preserved)..."
    aws_cmd ec2 stop-instances --instance-ids "$old_instance_id"
    log "Instance stopping. EBS will be preserved (~\$8/mo)."
}

# --- Main ---
CMD="${1:-help}"
case "$CMD" in
    create)    cmd_create ;;
    deploy)    cmd_deploy ;;
    ssh)       shift; cmd_ssh "$@" ;;
    status)    cmd_status ;;
    stop)      cmd_stop ;;
    start)     cmd_start ;;
    destroy)   cmd_destroy ;;
    stop-old)  shift; cmd_stop_old "$@" ;;
    *)
        echo "Usage: $0 {create|deploy|ssh|status|stop|start|destroy|stop-old}"
        echo ""
        echo "Commands:"
        echo "  create     Launch c5d.metal spot instance with QEMU setup"
        echo "  deploy     Build and deploy code to instance"
        echo "  ssh        SSH into instance (use -- for remote commands)"
        echo "  status     Show instance status"
        echo "  stop       Stop instance (on-demand only; spot cannot be stopped)"
        echo "  start      Start a stopped instance"
        echo "  destroy    Terminate instance (NVMe data lost)"
        echo "  stop-old   Stop the old a1.metal ARM instance (preserves EBS)"
        echo ""
        echo "Environment:"
        echo "  AWS_PROFILE=$AWS_PROFILE  AWS_REGION=$AWS_REGION"
        echo "  INSTANCE_TYPE=$INSTANCE_TYPE  API_KEY=$API_KEY"
        echo ""
        echo "Quick start:"
        echo "  $0 create    # Launch c5d.metal spot (~\$1.40/hr)"
        echo "  $0 deploy    # Build + deploy QEMU backend"
        echo "  $0 ssh       # SSH into instance"
        ;;
esac
