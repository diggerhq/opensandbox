#!/usr/bin/env bash
# build-rootfs.sh — Build ext4 rootfs images for Firecracker VMs
#
# Usage:
#   ./scripts/build-rootfs.sh [default|ubuntu|python|node|all]
#   IMAGES_DIR=/data/firecracker/images ./scripts/build-rootfs.sh all
#
# Requirements:
#   - podman (for building container images)
#   - mkfs.ext4, mount, umount (for ext4 conversion — requires root)
#   - osb-agent binary in bin/ (built via: make build-agent)
#
# The script:
#   1. Builds a container image from the Dockerfile
#   2. Injects osb-agent and init script
#   3. Exports the filesystem as tar
#   4. Converts tar → ext4 image
#   5. Shrinks the ext4 image to minimum size
#   6. Places the result in $IMAGES_DIR/{name}.ext4

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DOCKERFILE_DIR="$PROJECT_ROOT/deploy/firecracker/rootfs"
IMAGES_DIR="${IMAGES_DIR:-$PROJECT_ROOT/bin/images}"
AGENT_BIN="${AGENT_BIN:-$PROJECT_ROOT/bin/osb-agent}"
EXT4_SIZE_MB="${EXT4_SIZE_MB:-4096}"  # 4GB sparse (actual usage much less)

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log() { echo -e "${GREEN}[build-rootfs]${NC} $*"; }
warn() { echo -e "${YELLOW}[build-rootfs]${NC} $*"; }
err() { echo -e "${RED}[build-rootfs]${NC} $*" >&2; }

# Check prerequisites
check_prereqs() {
    local missing=()
    command -v podman &>/dev/null || missing+=(podman)
    command -v mkfs.ext4 &>/dev/null || missing+=(e2fsprogs)

    if [ ${#missing[@]} -gt 0 ]; then
        err "Missing prerequisites: ${missing[*]}"
        exit 1
    fi

    if [ ! -f "$AGENT_BIN" ]; then
        warn "osb-agent not found at $AGENT_BIN"
        warn "Building agent binary..."
        (cd "$PROJECT_ROOT" && make build-agent)
        if [ ! -f "$AGENT_BIN" ]; then
            err "Failed to build osb-agent. Run: make build-agent"
            exit 1
        fi
    fi

    # Check if running as root (needed for mount/umount)
    if [ "$(id -u)" -ne 0 ]; then
        err "This script requires root privileges for mount/umount operations."
        err "Run with: sudo $0 $*"
        exit 1
    fi
}

# Generate the init script
generate_init_script() {
    cat << 'INIT_EOF'
#!/bin/busybox sh
# OpenSandbox VM init script
# Runs as PID 1 inside the Firecracker microVM

# Mount virtual filesystems
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs tmpfs /tmp
mount -t tmpfs tmpfs /run

# Create device nodes if devtmpfs didn't
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

# Mount workspace from vdb
mkdir -p /workspace
mount /dev/vdb /workspace 2>/dev/null || {
    echo "init: warning: could not mount /dev/vdb, trying /dev/vdb1"
    mount /dev/vdb1 /workspace 2>/dev/null || echo "init: warning: workspace mount failed"
}

# Configure networking from kernel command line
# Format: ip=GUEST_IP::GATEWAY:NETMASK::IFACE:off osb.gateway=GATEWAY
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

# Set up DNS
echo "nameserver 8.8.8.8" > /etc/resolv.conf
echo "nameserver 8.8.4.4" >> /etc/resolv.conf

# Set hostname
hostname sandbox

# Start the agent
exec /usr/local/bin/osb-agent
INIT_EOF
}

# Build a single rootfs image
build_image() {
    local name="$1"
    local dockerfile="$DOCKERFILE_DIR/Dockerfile.${name}"

    if [ ! -f "$dockerfile" ]; then
        err "Dockerfile not found: $dockerfile"
        return 1
    fi

    log "Building $name rootfs image..."

    local tmpdir
    tmpdir=$(mktemp -d /tmp/osb-rootfs-XXXXXXXX)
    trap "rm -rf $tmpdir" RETURN

    local image_tag="localhost/opensandbox-rootfs/${name}:build"

    # Copy Dockerfile to build context
    cp "$dockerfile" "$tmpdir/Dockerfile"

    # Copy agent binary
    cp "$AGENT_BIN" "$tmpdir/osb-agent"
    chmod +x "$tmpdir/osb-agent"

    # Generate init script
    generate_init_script > "$tmpdir/init"
    chmod +x "$tmpdir/init"

    # Append agent/init injection to Dockerfile
    cat >> "$tmpdir/Dockerfile" << 'INJECT_EOF'

# --- OpenSandbox agent injection ---
COPY osb-agent /usr/local/bin/osb-agent
RUN chmod +x /usr/local/bin/osb-agent
COPY init /sbin/init
RUN chmod +x /sbin/init
RUN mkdir -p /workspace
INJECT_EOF

    # Build with podman
    log "  podman build..."
    if ! podman build -t "$image_tag" -f "$tmpdir/Dockerfile" "$tmpdir" 2>&1 | tail -5; then
        err "  podman build failed for $name"
        return 1
    fi

    # Create container and export filesystem
    log "  exporting filesystem..."
    local container_id
    container_id=$(podman create "$image_tag" /bin/true 2>/dev/null)
    podman export "$container_id" -o "$tmpdir/rootfs.tar"
    podman rm -f "$container_id" &>/dev/null || true

    # Convert tar to ext4
    log "  converting to ext4 (${EXT4_SIZE_MB}MB sparse)..."
    local ext4_path="$tmpdir/rootfs.ext4"

    # Create sparse file
    truncate -s "${EXT4_SIZE_MB}M" "$ext4_path"

    # Format as ext4
    mkfs.ext4 -q -F -L rootfs "$ext4_path"

    # Mount and populate
    local mnt_dir="$tmpdir/mnt"
    mkdir -p "$mnt_dir"
    mount -o loop "$ext4_path" "$mnt_dir"

    tar xf "$tmpdir/rootfs.tar" -C "$mnt_dir"

    # Ensure key directories
    for dir in proc sys dev dev/pts dev/shm tmp workspace run; do
        mkdir -p "$mnt_dir/$dir"
    done

    sync
    umount "$mnt_dir"

    # Shrink to minimum
    log "  shrinking ext4 image..."
    resize2fs -M "$ext4_path" 2>/dev/null || warn "  resize2fs -M failed (non-fatal)"

    # Get final size
    local final_size
    final_size=$(du -h "$ext4_path" | cut -f1)

    # Move to images directory
    mkdir -p "$IMAGES_DIR"
    cp "$ext4_path" "$IMAGES_DIR/${name}.ext4"

    # Clean up podman image
    podman rmi -f "$image_tag" &>/dev/null || true

    log "  Done: $IMAGES_DIR/${name}.ext4 ($final_size)"
}

# Main
main() {
    local targets=("${@:-all}")

    check_prereqs

    mkdir -p "$IMAGES_DIR"

    for target in "${targets[@]}"; do
        case "$target" in
            all)
                build_image default
                build_image ubuntu
                build_image python
                build_image node
                ;;
            default|ubuntu|python|node)
                build_image "$target"
                ;;
            *)
                err "Unknown target: $target"
                echo "Usage: $0 [default|ubuntu|python|node|all]"
                exit 1
                ;;
        esac
    done

    log ""
    log "All images built successfully:"
    ls -lh "$IMAGES_DIR"/*.ext4 2>/dev/null || true
}

main "$@"
