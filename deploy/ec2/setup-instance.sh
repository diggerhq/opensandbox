#!/usr/bin/env bash
set -euo pipefail

# Provision a fresh Ubuntu 24.04 EC2 instance as an OpenSandbox worker (QEMU backend).
# Run this ON the instance (ssh in first), or pipe via ssh:
#   ssh -i key.pem ubuntu@<IP> 'bash -s' < deploy/ec2/setup-instance.sh
#
# Supports both x86_64 (amd64) and aarch64 (arm64/Graviton) instances.
# Optimized for bare-metal Graviton (r7gd.metal) or x86 (c7i.metal).
#
# Prerequisites:
#   - Ubuntu 24.04 LTS AMI
#   - Bare-metal instance for KVM (r7gd.metal, c7i.metal)
#   - Security group: 8080 (HTTP), 9090 (gRPC), 9091 (metrics) open from VPC only
#   - SSH access

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH="amd64"; QEMU_PKG="qemu-system-x86" ;;
  aarch64) GOARCH="arm64";  QEMU_PKG="qemu-system-arm" ;;
  *)       echo "ERROR: Unsupported architecture: $ARCH"; exit 1 ;;
esac
echo "==> Detected architecture: $ARCH ($GOARCH)"

export DEBIAN_FRONTEND=noninteractive

echo "==> Updating packages..."
sudo apt-get update -qq && sudo apt-get upgrade -y -qq

# -------------------------------------------------------------------
# QEMU hypervisor
# -------------------------------------------------------------------
echo "==> Installing QEMU..."
sudo apt-get install -y -qq $QEMU_PKG qemu-utils

# -------------------------------------------------------------------
# Guest kernel for QEMU VMs
# -------------------------------------------------------------------
echo "==> Setting up guest kernel..."
KERNEL_DIR="/opt/opensandbox"
sudo mkdir -p "$KERNEL_DIR"

# Install generic kernel package (has VIRTIO_BLK, VIRTIO_NET, VIRTIO_PCI built-in)
sudo apt-get install -y -qq linux-image-generic

# Find the generic kernel and copy it
GENERIC_VMLINUZ=$(ls -t /boot/vmlinuz-*-generic 2>/dev/null | head -1)
if [ -n "$GENERIC_VMLINUZ" ]; then
    sudo cp "$GENERIC_VMLINUZ" "$KERNEL_DIR/vmlinux"
    sudo chmod 644 "$KERNEL_DIR/vmlinux"
    GENERIC_KVER=$(basename "$GENERIC_VMLINUZ" | sed 's/vmlinuz-//')
    echo "    Guest kernel: $GENERIC_VMLINUZ ($GENERIC_KVER)"

    # Extract vsock, overlay, and virtio_mem modules for the guest rootfs
    MODDIR="/lib/modules/$GENERIC_KVER"
    GUEST_MODDIR="$KERNEL_DIR/guest-modules"
    sudo mkdir -p "$GUEST_MODDIR"
    for mod in \
        "$MODDIR/kernel/fs/overlayfs/overlay.ko"* \
        "$MODDIR/kernel/net/vmw_vsock/vsock.ko"* \
        "$MODDIR/kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko"* \
        "$MODDIR/kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko"* \
        "$MODDIR/kernel/drivers/virtio/virtio_mem.ko"*; do
        [ -f "$mod" ] || continue
        base=$(basename "$mod")
        if [[ "$base" == *.zst ]]; then
            sudo zstd -d "$mod" -o "$GUEST_MODDIR/${base%.zst}" 2>/dev/null
        else
            sudo cp "$mod" "$GUEST_MODDIR/"
        fi
    done
    echo "    Guest modules extracted to $GUEST_MODDIR:"
    ls "$GUEST_MODDIR/"
else
    echo "WARNING: No generic kernel found. Guest VMs may not boot correctly."
fi

# -------------------------------------------------------------------
# Podman (for Dockerfile -> ext4 rootfs conversion)
# -------------------------------------------------------------------
echo "==> Installing Podman (for template building)..."
sudo apt-get install -y -qq podman uidmap slirp4netns

# -------------------------------------------------------------------
# Docker (for rootfs building via build-rootfs-docker.sh)
# -------------------------------------------------------------------
echo "==> Installing Docker..."
if ! command -v docker &>/dev/null; then
    curl -fsSL https://get.docker.com | sudo sh
fi
sudo systemctl enable --now docker

# -------------------------------------------------------------------
# System tools for ext4 image creation and storage
# -------------------------------------------------------------------
echo "==> Installing system tools..."
sudo apt-get install -y -qq e2fsprogs xfsprogs nvme-cli mdadm jq curl zstd

# -------------------------------------------------------------------
# KVM + vhost-vsock
# -------------------------------------------------------------------
echo "==> Loading kernel modules..."
sudo modprobe kvm || true
case "$ARCH" in
  x86_64)  sudo modprobe kvm_intel 2>/dev/null || sudo modprobe kvm_amd 2>/dev/null || true ;;
  aarch64) ;; # KVM is built-in on ARM64
esac
sudo modprobe vhost_vsock || true

# Persist modules across reboots
sudo tee /etc/modules-load.d/kvm.conf > /dev/null << 'EOF'
kvm
vhost_vsock
EOF

# Ensure /dev/kvm and /dev/vhost-vsock are accessible
sudo chmod 666 /dev/kvm 2>/dev/null || true
sudo chmod 666 /dev/vhost-vsock 2>/dev/null || true

# Udev rules for persistent permissions
sudo tee /etc/udev/rules.d/99-opensandbox.rules > /dev/null << 'EOF'
KERNEL=="kvm", GROUP="kvm", MODE="0666"
KERNEL=="vhost-vsock", MODE="0666"
EOF

# -------------------------------------------------------------------
# NVMe instance storage (XFS with reflink for instant rootfs copies)
# -------------------------------------------------------------------
echo "==> Installing NVMe boot service..."

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
  mkdir -p "$MOUNT_POINT/sandboxes" "$MOUNT_POINT/images" "$MOUNT_POINT/checkpoints"
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
mkdir -p "$MOUNT_POINT/images"
mkdir -p "$MOUNT_POINT/checkpoints"

# Copy QEMU assets from EBS to NVMe if available
SRC="/opt/opensandbox"
if [ -f "$SRC/vmlinux" ] && [ ! -f "$MOUNT_POINT/vmlinux" ]; then
  echo "opensandbox-nvme: copying QEMU assets from $SRC to $MOUNT_POINT"
  cp "$SRC/vmlinux" "$MOUNT_POINT/"
  [ -d "$SRC/guest-modules" ] && cp -r "$SRC/guest-modules" "$MOUNT_POINT/"
  # Copy base images if they exist (legacy path: /data/firecracker/images)
  if [ -d "/data/firecracker/images" ] && ls /data/firecracker/images/*.ext4 &>/dev/null; then
    mkdir -p "$MOUNT_POINT/images"
    cp /data/firecracker/images/*.ext4 "$MOUNT_POINT/images/"
  fi
  echo "opensandbox-nvme: assets copied ($(du -sh "$MOUNT_POINT" | cut -f1))"
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
# IP forwarding + sysctl tuning for VM networking
# -------------------------------------------------------------------
echo "==> Configuring sysctl for QEMU VMs..."
sudo tee /etc/sysctl.d/99-opensandbox.conf > /dev/null << 'SYSCTL'
# IP forwarding for VM networking
net.ipv4.ip_forward = 1
net.ipv4.conf.all.route_localnet = 1
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
sudo sysctl --system -q

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
OPENSANDBOX_MACHINE_ID=${INSTANCE_ID}
OPENSANDBOX_HTTP_ADDR=http://${PRIVATE_IP}:8080
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
# Worker systemd unit (QEMU backend)
# -------------------------------------------------------------------
echo "==> Installing worker systemd unit..."
sudo tee /etc/systemd/system/opensandbox-worker.service > /dev/null << 'SVC'
[Unit]
Description=OpenSandbox Worker (QEMU backend)
After=network-online.target opensandbox-nvme.service opensandbox-identity.service
Wants=network-online.target
Requires=opensandbox-nvme.service opensandbox-identity.service

[Service]
Type=simple
ExecStartPre=/sbin/modprobe vhost_vsock
ExecStart=/usr/local/bin/opensandbox-worker
Restart=always
RestartSec=5
Environment=HOME=/root
Environment=OPENSANDBOX_MODE=worker
Environment=OPENSANDBOX_PORT=8080
Environment=OPENSANDBOX_REGION=use2
Environment=OPENSANDBOX_DATA_DIR=/data
Environment=OPENSANDBOX_SANDBOX_DOMAIN=workers.opensandbox.ai
Environment=OPENSANDBOX_QEMU_BIN=qemu-system-x86_64
Environment=OPENSANDBOX_KERNEL_PATH=/opt/opensandbox/vmlinux
Environment=OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
Environment=OPENSANDBOX_SECRETS_ARN=arn:aws:secretsmanager:us-east-2:739940681129:secret:opensandbox/worker-vtN2Ez
EnvironmentFile=/etc/opensandbox/worker-identity.env
# Only signal the main process on stop (not QEMU children)
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

# -------------------------------------------------------------------
# Cleanup
# -------------------------------------------------------------------
echo "==> Cleaning up..."
sudo apt-get clean

# Determine QEMU binary name for display
QEMU_BIN="qemu-system-x86_64"
if [ "$ARCH" = "aarch64" ]; then
  QEMU_BIN="qemu-system-aarch64"
fi

echo ""
echo "============================================"
echo " Instance setup complete! ($ARCH)"
echo ""
echo " Installed:"
echo "   - QEMU: $($QEMU_BIN --version | head -1)"
echo "   - Podman: $(podman --version)"
echo "   - Docker: $(docker --version)"
echo "   - KVM: $(ls -la /dev/kvm 2>/dev/null || echo 'not available')"
echo "   - VSOCK: $(ls -la /dev/vhost-vsock 2>/dev/null || echo 'not available')"
echo ""
echo " Remaining steps:"
echo "   1. Deploy worker + agent binaries: ./deploy/ec2/deploy-worker.sh"
echo "   2. Build base rootfs images: sudo bash deploy/ec2/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default"
echo "   3. Start services: sudo systemctl start opensandbox-worker"
echo "============================================"
