#!/usr/bin/env bash
# build-rootfs-docker.sh — Build ext4 rootfs images using Docker (no Podman needed)
#
# Designed to run ON an EC2 instance (Amazon Linux 2023) where Podman isn't available.
# Uses Docker instead for the container build + export steps.
#
# Usage:
#   sudo ./deploy/ec2/build-rootfs-docker.sh AGENT_BIN IMAGES_DIR [IMAGE_NAME]
#
# Example:
#   sudo ./deploy/ec2/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default
#
# Requirements:
#   - Docker (installed by user_data)
#   - e2fsprogs (installed by user_data)
#   - Root privileges (for mount/umount)

set -euo pipefail

AGENT_BIN="${1:?Usage: $0 AGENT_BIN IMAGES_DIR [IMAGE_NAME]}"
IMAGES_DIR="${2:?Usage: $0 AGENT_BIN IMAGES_DIR [IMAGE_NAME]}"
IMAGE_NAME="${3:-default}"
EXT4_SIZE_MB="${EXT4_SIZE_MB:-4096}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DOCKERFILE_DIR="$PROJECT_ROOT/deploy/firecracker/rootfs"
DOCKERFILE="$DOCKERFILE_DIR/Dockerfile.${IMAGE_NAME}"

log() { echo "[build-rootfs-docker] $*"; }
err() { echo "[build-rootfs-docker] ERROR: $*" >&2; }

# Validate
if [ "$(id -u)" -ne 0 ]; then
    err "This script requires root privileges (for mount/umount)."
    err "Run with: sudo $0 $*"
    exit 1
fi

if [ ! -f "$AGENT_BIN" ]; then
    err "Agent binary not found: $AGENT_BIN"
    exit 1
fi

if [ ! -f "$DOCKERFILE" ]; then
    err "Dockerfile not found: $DOCKERFILE"
    exit 1
fi

command -v docker &>/dev/null || { err "Docker not found"; exit 1; }
command -v mkfs.ext4 &>/dev/null || { err "mkfs.ext4 not found (install e2fsprogs)"; exit 1; }

log "Building $IMAGE_NAME rootfs from $DOCKERFILE"

TMPDIR=$(mktemp -d /tmp/osb-rootfs-docker-XXXXXXXX)
trap 'rm -rf $TMPDIR' EXIT

# Copy Dockerfile to build context
cp "$DOCKERFILE" "$TMPDIR/Dockerfile"

# Copy agent binary
cp "$AGENT_BIN" "$TMPDIR/osb-agent"
chmod +x "$TMPDIR/osb-agent"

# Generate init script (same as scripts/build-rootfs.sh)
cat > "$TMPDIR/init" << 'INIT_EOF'
#!/bin/busybox sh
# OpenSandbox VM init script — PID 1 inside Firecracker microVM

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs tmpfs /tmp
mount -t tmpfs tmpfs /run

[ -c /dev/null ] || mknod -m 666 /dev/null c 1 3
[ -c /dev/zero ] || mknod -m 666 /dev/zero c 1 5
[ -c /dev/random ] || mknod -m 444 /dev/random c 1 8
[ -c /dev/urandom ] || mknod -m 444 /dev/urandom c 1 9
[ -c /dev/tty ] || mknod -m 666 /dev/tty c 5 0
[ -c /dev/console ] || mknod -m 600 /dev/console c 5 1
[ -d /dev/pts ] || mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
[ -d /dev/shm ] || mkdir -p /dev/shm
mount -t tmpfs tmpfs /dev/shm

mkdir -p /workspace
mount /dev/vdb /workspace 2>/dev/null || {
    echo "init: warning: could not mount /dev/vdb, trying /dev/vdb1"
    mount /dev/vdb1 /workspace 2>/dev/null || echo "init: warning: workspace mount failed"
}

for param in $(cat /proc/cmdline); do
    case "$param" in
        ip=*)
            IP_CONFIG="${param#ip=}"
            GUEST_IP=$(echo "$IP_CONFIG" | cut -d: -f1)
            GATEWAY=$(echo "$IP_CONFIG" | cut -d: -f3)
            NETMASK=$(echo "$IP_CONFIG" | cut -d: -f4)
            IFACE=$(echo "$IP_CONFIG" | cut -d: -f6)
            ;;
        osb.gateway=*)
            GATEWAY="${param#osb.gateway=}"
            ;;
    esac
done

if [ -n "$GUEST_IP" ] && [ -n "$IFACE" ]; then
    ip link set lo up
    ip addr add "${GUEST_IP}/30" dev "$IFACE"
    ip link set "$IFACE" up
    if [ -n "$GATEWAY" ]; then
        ip route add default via "$GATEWAY" dev "$IFACE"
    fi
fi

echo "nameserver 8.8.8.8" > /etc/resolv.conf
echo "nameserver 8.8.4.4" >> /etc/resolv.conf

hostname sandbox

exec /usr/local/bin/osb-agent
INIT_EOF
chmod +x "$TMPDIR/init"

# Append agent/init injection to Dockerfile
cat >> "$TMPDIR/Dockerfile" << 'INJECT_EOF'

# --- OpenSandbox agent injection ---
COPY osb-agent /usr/local/bin/osb-agent
RUN chmod +x /usr/local/bin/osb-agent
COPY init /sbin/init
RUN chmod +x /sbin/init
RUN mkdir -p /workspace
INJECT_EOF

# Build with Docker
log "Building container image..."
docker build -t "osb-rootfs-${IMAGE_NAME}:build" -f "$TMPDIR/Dockerfile" "$TMPDIR"

# Create container and export filesystem
log "Exporting filesystem..."
docker rm -f osb-rootfs-tmp 2>/dev/null || true
docker create --name osb-rootfs-tmp "osb-rootfs-${IMAGE_NAME}:build" /bin/true
docker export osb-rootfs-tmp -o "$TMPDIR/rootfs.tar"
docker rm -f osb-rootfs-tmp

# Convert tar to ext4
log "Converting to ext4 (${EXT4_SIZE_MB}MB sparse)..."
EXT4_PATH="$TMPDIR/rootfs.ext4"
truncate -s "${EXT4_SIZE_MB}M" "$EXT4_PATH"
mkfs.ext4 -q -F -L rootfs "$EXT4_PATH"

MNT_DIR="$TMPDIR/mnt"
mkdir -p "$MNT_DIR"
mount -o loop "$EXT4_PATH" "$MNT_DIR"

tar xf "$TMPDIR/rootfs.tar" -C "$MNT_DIR"

# Ensure key directories exist
for dir in proc sys dev dev/pts dev/shm tmp workspace run; do
    mkdir -p "$MNT_DIR/$dir"
done

sync
umount "$MNT_DIR"

# Shrink to minimum size
log "Shrinking ext4 image..."
resize2fs -M "$EXT4_PATH" 2>/dev/null || log "resize2fs -M failed (non-fatal)"

# Place in output directory
mkdir -p "$IMAGES_DIR"
cp "$EXT4_PATH" "$IMAGES_DIR/${IMAGE_NAME}.ext4"

# Clean up Docker image
docker rmi -f "osb-rootfs-${IMAGE_NAME}:build" &>/dev/null || true

FINAL_SIZE=$(du -h "$IMAGES_DIR/${IMAGE_NAME}.ext4" | cut -f1)
log "Done: $IMAGES_DIR/${IMAGE_NAME}.ext4 ($FINAL_SIZE)"
