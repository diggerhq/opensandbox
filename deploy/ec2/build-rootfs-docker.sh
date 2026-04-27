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

# Mount minimal virtual filesystems needed for setup
mount -t proc proc /proc
mount -t devtmpfs devtmpfs /dev

# Load kernel modules (needed for QEMU with modular kernel)
if [ -d /lib/modules/vsock ]; then
    for mod in /lib/modules/vsock/vsock.ko /lib/modules/vsock/vmw_vsock_virtio_transport_common.ko /lib/modules/vsock/vmw_vsock_virtio_transport.ko; do
        [ -f "$mod" ] && insmod "$mod" 2>/dev/null || true
    done
    echo "init: kernel modules loaded"
fi

# Load virtio-mem module (for dynamic memory scaling)
# Try modprobe first (handles signatures + deps), fall back to insmod.
# This is best-effort at boot — golden snapshot creation will fail hard if not loaded.
if command -v modprobe >/dev/null 2>&1; then
    modprobe virtio_mem 2>/dev/null && echo "init: virtio_mem loaded (modprobe)" || true
else
    for vmem in "/lib/modules/$(uname -r)/kernel/drivers/virtio/virtio_mem.ko" "/lib/modules/vsock/virtio_mem.ko"; do
        if [ -f "$vmem" ]; then
            insmod "$vmem" 2>/dev/null || true
            echo "init: virtio_mem loaded ($vmem)"
            break
        fi
    done
fi

# ── Mount workspace: data disk at /home/sandbox (persistent user data) ──
# /workspace is a symlink to /home/sandbox, so mount the real path.
mkdir -p /home/sandbox
if mount /dev/vdb /home/sandbox 2>/dev/null || mount /dev/vdb1 /home/sandbox 2>/dev/null; then
    chown sandbox:sandbox /home/sandbox 2>/dev/null
    echo "init: workspace mounted (/dev/vdb -> /home/sandbox)"
else
    echo "init: warning: no data disk found, /home/sandbox is ephemeral"
fi

# ── Mount virtual filesystems (in the final root) ──
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

# Debug: check for virtio-serial device
ls -la /dev/vport* /dev/virtio-ports/ 2>/dev/null || echo "init: no virtio-serial devices found"

# ── Cgroup v2: sandbox resource limits ──
# Agent (PID 1) stays in root cgroup (protected from user resource exhaustion).
# User processes are placed in /sys/fs/cgroup/sandbox/ by the agent's exec handler.
mkdir -p /sys/fs/cgroup
mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    # Enable controllers in root
    echo "+cpu +memory +pids" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null
    # Create sandbox cgroup
    mkdir -p /sys/fs/cgroup/sandbox
    # Defaults: 4096 pids, 90% of total memory, 90% of CPUs.
    # 4096 is enough headroom for typical dev tooling (npm install, go test,
    # multi-process build systems) while still bounding fork-bomb damage.
    # The previous default of 128 was below an interactive shell + LSP +
    # build agent working set, and customers hit "fork failed: resource
    # temporarily unavailable" inside otherwise-idle sandboxes.
    # CPU limit reserves 10% for the agent (PID 1) so it stays responsive
    # even under fork bomb / CPU exhaustion attacks.
    echo 4096 > /sys/fs/cgroup/sandbox/pids.max
    SANDBOX_MEM=$(awk '/MemTotal/{printf "%.0f", $2 * 1024 * 0.9}' /proc/meminfo)
    echo "$SANDBOX_MEM" > /sys/fs/cgroup/sandbox/memory.max 2>/dev/null
    # cpu.max: limit user processes to 80% of available CPUs.
    # This reserves 20% for the agent (PID 1) so it stays responsive
    # even under fork bomb / CPU saturation.
    NUM_CPUS=$(nproc)
    CPU_MAX=$(( 80000 * NUM_CPUS ))
    echo "$CPU_MAX 100000" > /sys/fs/cgroup/sandbox/cpu.max 2>/dev/null
    # cpu.weight: lower priority than agent
    echo 50 > /sys/fs/cgroup/sandbox/cpu.weight 2>/dev/null
    echo "init: cgroup sandbox ready (pids=4096, mem=${SANDBOX_MEM}, cpu=${CPU_MAX}/100000)"
else
    echo "init: warning: cgroup v2 not available"
fi

# Note: user commands run as root inside the VM. This is safe because:
# - Each VM is fully isolated (separate QEMU process, separate kernel)
# - cgroup v2 prevents processes inside a cgroup from modifying their own limits
# - The agent (PID 1) is in the root cgroup, user processes in /sandbox cgroup

exec /usr/local/bin/osb-agent
INIT_EOF
chmod +x "$TMPDIR/init"

# Copy claude-agent-wrapper source (for images that include it)
WRAPPER_DIR="$PROJECT_ROOT/scripts/claude-agent-wrapper"
if [ -d "$WRAPPER_DIR" ]; then
    mkdir -p "$TMPDIR/scripts"
    cp -r "$WRAPPER_DIR" "$TMPDIR/scripts/claude-agent-wrapper"
fi

# Append agent/init injection to Dockerfile
cat >> "$TMPDIR/Dockerfile" << 'INJECT_EOF'

# --- OpenSandbox agent injection ---
# Create sandbox user (UID 1000) for exec sessions — agent runs as root (PID 1),
# but user commands run as this non-root user for cgroup/security isolation.
RUN useradd -m -u 1000 -s /bin/bash sandbox && \
    echo 'sandbox ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers && \
    chown sandbox:sandbox /workspace 2>/dev/null || true
COPY osb-agent /usr/local/bin/osb-agent
RUN chmod +x /usr/local/bin/osb-agent
COPY init /sbin/init
RUN chmod +x /sbin/init
RUN mkdir -p /mnt/data /mnt/overlay
INJECT_EOF

# Build with Docker
log "Building container image..."
docker build --no-cache -t "osb-rootfs-${IMAGE_NAME}:build" -f "$TMPDIR/Dockerfile" "$TMPDIR"

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

# Ensure key directories exist (workspace is created by Dockerfile)
for dir in proc sys dev dev/pts dev/shm tmp run; do
    mkdir -p "$MNT_DIR/$dir"
done

# Inject guest kernel modules into rootfs.
# Copy the full /lib/modules/<kver> tree so all modules (Docker networking,
# vsock, overlay, virtio_mem, etc.) are available with correct dependencies.
GUEST_KVER_FILE="/opt/opensandbox/guest-kernel-version"
if [ -f "$GUEST_KVER_FILE" ]; then
    GUEST_KVER=$(cat "$GUEST_KVER_FILE")
elif ls -d /lib/modules/*-generic >/dev/null 2>&1; then
    GUEST_KVER=$(ls -d /lib/modules/*-generic | sort -V | tail -1 | xargs basename)
fi

if [ -n "${GUEST_KVER:-}" ] && [ -d "/lib/modules/$GUEST_KVER" ]; then
    log "Injecting kernel modules for $GUEST_KVER..."
    rm -rf "$MNT_DIR/lib/modules"/*
    mkdir -p "$MNT_DIR/lib/modules"
    cp -a "/lib/modules/$GUEST_KVER" "$MNT_DIR/lib/modules/"
    depmod -b "$MNT_DIR" "$GUEST_KVER" 2>/dev/null || log "depmod failed (non-fatal)"
    MOD_COUNT=$(find "$MNT_DIR/lib/modules/$GUEST_KVER" -name "*.ko*" | wc -l)
    log "Injected $MOD_COUNT modules for kernel $GUEST_KVER"
else
    log "WARNING: No guest kernel modules found — Docker networking and virtio_mem will not work"
fi

sync
umount "$MNT_DIR"

# Shrink to a usable floor — leave room for apt install, Docker, kernel modules etc.
# The ext4 is inside a qcow2 COW overlay, so unused space costs nothing on disk.
ROOTFS_MIN_MB="${ROOTFS_MIN_MB:-4096}"
log "Resizing ext4 to ${ROOTFS_MIN_MB}MB floor (sparse — actual disk usage stays low)..."
resize2fs "$EXT4_PATH" "${ROOTFS_MIN_MB}M" 2>/dev/null || log "resize2fs failed (non-fatal)"

# Place in output directory
mkdir -p "$IMAGES_DIR"
cp "$EXT4_PATH" "$IMAGES_DIR/${IMAGE_NAME}.ext4"

# Clean up Docker image
docker rmi -f "osb-rootfs-${IMAGE_NAME}:build" &>/dev/null || true

FINAL_SIZE=$(du -h "$IMAGES_DIR/${IMAGE_NAME}.ext4" | cut -f1)
log "Done: $IMAGES_DIR/${IMAGE_NAME}.ext4 ($FINAL_SIZE)"
