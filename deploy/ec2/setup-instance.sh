#!/usr/bin/env bash
set -euo pipefail

# Provision a fresh Ubuntu 24.04 EC2 instance as an OpenSandbox worker.
# Run this ON the instance (ssh in first), or pipe via ssh:
#   ssh -i key.pem ubuntu@<IP> 'bash -s' < deploy/ec2/setup-instance.sh
#
# Supports both x86_64 (amd64) and aarch64 (arm64/Graviton) instances.
# Optimized for Firecracker microVMs on bare-metal Graviton (r7gd.metal).
#
# Prerequisites:
#   - Ubuntu 24.04 LTS AMI
#   - r7gd.metal (ARM64) or c7i.metal (x86_64) for production
#   - Security group: 443 (HTTPS), 8080 (HTTP), 9090 (gRPC), 9091 (metrics) open inbound
#   - SSH access

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH="amd64"; FC_ARCH="x86_64" ;;
  aarch64) GOARCH="arm64"; FC_ARCH="aarch64" ;;
  *)       echo "ERROR: Unsupported architecture: $ARCH"; exit 1 ;;
esac
echo "==> Detected architecture: $ARCH ($GOARCH)"

echo "==> Updating packages..."
sudo apt-get update && sudo apt-get upgrade -y

# -------------------------------------------------------------------
# Firecracker microVM runtime
# -------------------------------------------------------------------
echo "==> Installing Firecracker..."
FC_VERSION="v1.9.1"
FC_RELEASE="firecracker-${FC_VERSION}-${FC_ARCH}"
FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/${FC_RELEASE}.tgz"

cd /tmp
curl -fSL -o firecracker.tgz "$FC_URL"
tar xzf firecracker.tgz
sudo cp "release-${FC_VERSION}-${FC_ARCH}/firecracker-${FC_VERSION}-${FC_ARCH}" /usr/local/bin/firecracker
sudo chmod +x /usr/local/bin/firecracker
rm -rf firecracker.tgz "release-${FC_VERSION}-${FC_ARCH}"
cd /

echo "==> Verifying Firecracker..."
firecracker --version

# Verify KVM access (required for Firecracker)
if [ ! -e /dev/kvm ]; then
    echo "WARNING: /dev/kvm not found. Firecracker requires bare-metal or nested virt."
    echo "  For bare-metal instances (r7gd.metal), /dev/kvm should exist."
    echo "  For regular instances, enable nested virtualization in the AMI."
fi

# Ensure KVM permissions
sudo chmod 666 /dev/kvm 2>/dev/null || true

# -------------------------------------------------------------------
# Podman (kept as build tool for Dockerfile → ext4 conversion)
# -------------------------------------------------------------------
echo "==> Installing Podman (for template building)..."
sudo apt-get install -y podman uidmap slirp4netns

# -------------------------------------------------------------------
# System tools for ext4 image creation
# -------------------------------------------------------------------
echo "==> Installing ext4 and rootfs tools..."
sudo apt-get install -y e2fsprogs

# -------------------------------------------------------------------
# Redis
# -------------------------------------------------------------------
echo "==> Installing Redis..."
sudo apt-get install -y redis-server

# -------------------------------------------------------------------
# Caddy (custom build with Route53 DNS module for wildcard certs)
# -------------------------------------------------------------------
echo "==> Installing Go (needed for xcaddy)..."
GO_VERSION="1.23.6"
curl -sL "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" | sudo tar -C /usr/local -xzf -
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin

echo "==> Building Caddy with Route53 DNS module..."
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
xcaddy build --with github.com/caddy-dns/route53 --output /tmp/caddy-custom
sudo mv /tmp/caddy-custom /usr/local/bin/caddy
sudo chmod +x /usr/local/bin/caddy

echo "==> Verifying Caddy has Route53 module..."
caddy list-modules | grep route53 || { echo "ERROR: Caddy missing route53 module"; exit 1; }

echo "==> Installing Caddy config..."
sudo mkdir -p /etc/caddy
sudo cp /tmp/deploy-ec2/Caddyfile /etc/caddy/Caddyfile 2>/dev/null || \
  echo "    NOTE: Copy deploy/ec2/Caddyfile to /etc/caddy/Caddyfile manually"

echo "==> Installing Caddy systemd unit..."
sudo cp /tmp/deploy-ec2/caddy.service /etc/systemd/system/caddy.service 2>/dev/null || \
  echo "    NOTE: Copy deploy/ec2/caddy.service to /etc/systemd/system/ manually"

# -------------------------------------------------------------------
# NVMe instance storage (XFS with reflink for instant rootfs copies)
# -------------------------------------------------------------------
echo "==> Installing NVMe boot service..."
sudo apt-get install -y xfsprogs nvme-cli mdadm

sudo tee /usr/local/bin/opensandbox-nvme-setup.sh > /dev/null << 'NVME'
#!/usr/bin/env bash
set -euo pipefail
MOUNT_POINT="/data/sandboxes"
mkdir -p "$MOUNT_POINT"
if mountpoint -q "$MOUNT_POINT"; then
  echo "opensandbox-nvme: $MOUNT_POINT already mounted"
  exit 0
fi

# Find the root EBS device so we can exclude it.
ROOT_DEV=$(lsblk -no PKNAME $(findmnt -n -o SOURCE /) | head -1)

# Collect all NVMe instance store devices (skip the root EBS volume).
NVME_DEVS=()
for dev in /dev/nvme0n1 /dev/nvme1n1 /dev/nvme2n1 /dev/nvme3n1 /dev/nvme4n1; do
  [ -b "$dev" ] || continue
  # Skip if this is the root device
  [ "$(basename "$dev")" = "$ROOT_DEV" ] && continue
  NVME_DEVS+=("$dev")
done

if [ ${#NVME_DEVS[@]} -eq 0 ]; then
  echo "opensandbox-nvme: no NVMe instance storage found, using root disk"
  mkdir -p "$MOUNT_POINT/sandboxes" "$MOUNT_POINT/firecracker/images" "$MOUNT_POINT/checkpoints"
  exit 0
fi

echo "opensandbox-nvme: found ${#NVME_DEVS[@]} NVMe instance store device(s): ${NVME_DEVS[*]}"

if [ ${#NVME_DEVS[@]} -eq 1 ]; then
  # Single drive — format and mount directly
  DEV="${NVME_DEVS[0]}"
  echo "opensandbox-nvme: formatting $DEV as XFS with project quotas"
  mkfs.xfs -f "$DEV"
  mount -o prjquota "$DEV" "$MOUNT_POINT"
else
  # Multiple drives — RAID-0 for maximum throughput + capacity
  echo "opensandbox-nvme: creating RAID-0 across ${#NVME_DEVS[@]} devices"
  mdadm --create /dev/md0 --level=0 --raid-devices=${#NVME_DEVS[@]} "${NVME_DEVS[@]}" --force --run
  echo "opensandbox-nvme: formatting /dev/md0 as XFS with project quotas"
  mkfs.xfs -f /dev/md0
  mount -o prjquota /dev/md0 "$MOUNT_POINT"
fi

# Create directory structure
mkdir -p "$MOUNT_POINT/sandboxes"
mkdir -p "$MOUNT_POINT/firecracker/images"
mkdir -p "$MOUNT_POINT/checkpoints"

# Copy Firecracker assets from EBS to NVMe if available
SRC="/opt/opensandbox/firecracker"
DST="$MOUNT_POINT/firecracker"
if [ -d "$SRC" ] && [ ! -f "$DST/vmlinux-arm64" ]; then
  echo "opensandbox-nvme: copying Firecracker assets from $SRC to $DST"
  cp "$SRC/vmlinux-arm64" "$DST/"
  cp "$SRC/images/"* "$DST/images/"
  echo "opensandbox-nvme: assets copied ($(du -sh "$DST" | cut -f1))"
fi
echo "opensandbox-nvme: mounted at $MOUNT_POINT ($(df -h "$MOUNT_POINT" | tail -1 | awk '{print $2}') total)"
NVME
sudo chmod +x /usr/local/bin/opensandbox-nvme-setup.sh

sudo tee /etc/systemd/system/opensandbox-nvme.service > /dev/null << 'SVC'
[Unit]
Description=OpenSandbox NVMe Instance Storage Setup
DefaultDependencies=no
Before=opensandbox-worker.service
After=local-fs.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/opensandbox-nvme-setup.sh

[Install]
WantedBy=multi-user.target
SVC

# -------------------------------------------------------------------
# Firecracker kernel + base rootfs images
# -------------------------------------------------------------------
echo "==> Setting up Firecracker kernel and base images directory..."
sudo mkdir -p /data/firecracker/images

# Download kernel (will be populated by deploy script)
echo "    Kernel will be installed by deploy-worker.sh at /data/firecracker/vmlinux-arm64"
echo "    Base rootfs images will be installed at /data/firecracker/images/"

# -------------------------------------------------------------------
# IP forwarding + iptables for VM networking
# -------------------------------------------------------------------
echo "==> Configuring IP forwarding for Firecracker VMs..."
sudo tee /etc/sysctl.d/99-opensandbox.conf > /dev/null << 'SYSCTL'
# Enable IP forwarding for Firecracker VM networking
net.ipv4.ip_forward = 1
# Increase ARP cache for many TAP interfaces
net.ipv4.neigh.default.gc_thresh1 = 1024
net.ipv4.neigh.default.gc_thresh2 = 4096
net.ipv4.neigh.default.gc_thresh3 = 8192
# Increase max open files for many VMs
fs.file-max = 1000000
# Increase inotify limits
fs.inotify.max_user_instances = 8192
fs.inotify.max_user_watches = 524288
SYSCTL
sudo sysctl --system

# -------------------------------------------------------------------
# Dynamic worker identity (from EC2 IMDS at boot)
# -------------------------------------------------------------------
echo "==> Installing identity service..."
sudo tee /usr/local/bin/opensandbox-worker-identity.sh > /dev/null << 'IDENT'
#!/usr/bin/env bash
set -euo pipefail
TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/instance-id)
PRIVATE_IP=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/local-ipv4)
PUBLIC_IP=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/public-ipv4 || echo "")
SHORT_ID=$(echo "$INSTANCE_ID" | sed 's/^i-//' | cut -c1-8)
WORKER_ID="w-use2-${SHORT_ID}"
mkdir -p /etc/opensandbox
cat > /etc/opensandbox/worker-identity.env << EOF
OPENSANDBOX_WORKER_ID=${WORKER_ID}
OPENSANDBOX_HTTP_ADDR=http://${PUBLIC_IP:-$PRIVATE_IP}:8080
OPENSANDBOX_GRPC_ADVERTISE=${PRIVATE_IP}:9090
EOF
echo "opensandbox-identity: ${WORKER_ID} private=${PRIVATE_IP} public=${PUBLIC_IP:-none}"
IDENT
sudo chmod +x /usr/local/bin/opensandbox-worker-identity.sh

sudo tee /etc/systemd/system/opensandbox-identity.service > /dev/null << 'SVC'
[Unit]
Description=OpenSandbox Worker Identity (from EC2 IMDS)
After=network-online.target
Wants=network-online.target
Before=opensandbox-worker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/opensandbox-worker-identity.sh

[Install]
WantedBy=multi-user.target
SVC

# -------------------------------------------------------------------
# Worker systemd unit (Firecracker)
# -------------------------------------------------------------------
echo "==> Installing worker systemd unit..."
sudo tee /etc/systemd/system/opensandbox-worker.service > /dev/null << 'SVC'
[Unit]
Description=OpenSandbox Worker (Firecracker)
After=network-online.target opensandbox-nvme.service opensandbox-identity.service
Wants=network-online.target
Requires=opensandbox-nvme.service opensandbox-identity.service

[Service]
Type=simple
ExecStart=/usr/local/bin/opensandbox-worker
Restart=always
RestartSec=5
Environment=HOME=/root
Environment=OPENSANDBOX_MODE=worker
Environment=OPENSANDBOX_PORT=8080
Environment=OPENSANDBOX_REGION=use2
Environment=OPENSANDBOX_DATA_DIR=/data
Environment=OPENSANDBOX_SANDBOX_DOMAIN=workers.opensandbox.ai
Environment=OPENSANDBOX_FIRECRACKER_BIN=/usr/local/bin/firecracker
Environment=OPENSANDBOX_KERNEL_PATH=/data/firecracker/vmlinux-arm64
Environment=OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
Environment=OPENSANDBOX_SECRETS_ARN=arn:aws:secretsmanager:us-east-2:739940681129:secret:opensandbox/worker-vtN2Ez
EnvironmentFile=/etc/opensandbox/worker-identity.env
# Only signal the main process on stop (not Firecracker children)
# so graceful shutdown can hibernate VMs before they are killed
KillMode=process
TimeoutStopSec=300
# Raise limits for many concurrent VMs
LimitNOFILE=1000000
LimitNPROC=65536

[Install]
WantedBy=multi-user.target
SVC

sudo mkdir -p /etc/opensandbox /data/sandboxes /data/firecracker/images /data/checkpoints

# -------------------------------------------------------------------
# Enable services
# -------------------------------------------------------------------
echo "==> Enabling services..."
sudo systemctl daemon-reload
sudo systemctl enable opensandbox-nvme
sudo systemctl enable opensandbox-identity
sudo systemctl enable opensandbox-worker
sudo systemctl enable caddy 2>/dev/null || true

# -------------------------------------------------------------------
# Cleanup
# -------------------------------------------------------------------
echo "==> Cleaning up build tools..."
sudo apt-get clean
sudo rm -rf /usr/local/go $HOME/go

echo ""
echo "============================================"
echo " Instance setup complete! ($ARCH)"
echo ""
echo " Installed:"
echo "   - Firecracker $(firecracker --version | head -1)"
echo "   - Podman $(podman --version)"
echo "   - Redis"
echo ""
echo " Remaining steps:"
echo "   1. Deploy worker + agent binaries: ./deploy/ec2/deploy-worker.sh"
echo "   2. Download kernel: scp bin/vmlinux-arm64 to /data/firecracker/"
echo "   3. Build base rootfs images: sudo ./scripts/build-rootfs.sh all"
echo "   4. Copy worker.env.example to /etc/opensandbox/worker.env and fill in secrets"
echo "   5. Start services: sudo systemctl start opensandbox-worker"
echo "   6. Set up wildcard DNS: *.workers.opencomputer.dev -> this instance IP"
echo "============================================"
