#!/usr/bin/env bash
# download-kernel.sh — Download the Firecracker-compatible Linux kernel
#
# Usage:
#   ./scripts/download-kernel.sh [--dest /data/firecracker/vmlinux-arm64]
#
# Downloads the pre-built Firecracker kernel for ARM64.
# The kernel is from the Firecracker project's official releases,
# built with their microvm config optimized for fast boot.
#
# Default destination: bin/vmlinux-arm64

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Firecracker kernel version — matches their v1.7+ releases
# Using the 5.10 series which is the most tested with Firecracker
FC_VERSION="${FC_VERSION:-v1.9.1}"
ARCH="${ARCH:-aarch64}"  # aarch64 for ARM64 Graviton

# Kernel binary name from Firecracker releases
KERNEL_NAME="vmlinux-5.10.225"

# Default destination
DEST="${1:-$PROJECT_ROOT/bin/vmlinux-arm64}"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log() { echo -e "${GREEN}[download-kernel]${NC} $*"; }
warn() { echo -e "${YELLOW}[download-kernel]${NC} $*"; }
err() { echo -e "${RED}[download-kernel]${NC} $*" >&2; }

# Parse --dest flag
while [[ $# -gt 0 ]]; do
    case "$1" in
        --dest)
            DEST="$2"
            shift 2
            ;;
        --version)
            FC_VERSION="$2"
            shift 2
            ;;
        --arch)
            ARCH="$2"
            shift 2
            ;;
        *)
            DEST="$1"
            shift
            ;;
    esac
done

main() {
    log "Downloading Firecracker kernel for $ARCH..."
    log "  Firecracker version: $FC_VERSION"
    log "  Destination: $DEST"

    # Create destination directory
    mkdir -p "$(dirname "$DEST")"

    # Check if already exists
    if [ -f "$DEST" ]; then
        local size
        size=$(du -h "$DEST" | cut -f1)
        warn "Kernel already exists at $DEST ($size)"
        read -p "Overwrite? [y/N] " -r
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            log "Keeping existing kernel."
            return 0
        fi
    fi

    # Download URL from Firecracker GitHub releases
    local url="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/${KERNEL_NAME}.bin"

    log "  Downloading from: $url"

    if command -v curl &>/dev/null; then
        curl -fSL --progress-bar -o "$DEST" "$url" || {
            err "Download failed. Trying alternative kernel names..."
            try_alternative_kernels
            return $?
        }
    elif command -v wget &>/dev/null; then
        wget -q --show-progress -O "$DEST" "$url" || {
            err "Download failed. Trying alternative kernel names..."
            try_alternative_kernels
            return $?
        }
    else
        err "Neither curl nor wget found. Install one and retry."
        exit 1
    fi

    # Verify the kernel binary
    if [ ! -f "$DEST" ] || [ ! -s "$DEST" ]; then
        err "Downloaded file is empty or missing."
        rm -f "$DEST"
        exit 1
    fi

    # Check it looks like a kernel (ELF binary)
    local file_type
    file_type=$(file "$DEST" 2>/dev/null || echo "unknown")
    if echo "$file_type" | grep -q "ELF"; then
        log "  Verified: ELF binary"
    else
        warn "  Warning: file type is '$file_type' (expected ELF)"
    fi

    local size
    size=$(du -h "$DEST" | cut -f1)
    log "  Downloaded: $DEST ($size)"
    log "Done!"
}

# Try alternative kernel names from Firecracker releases
try_alternative_kernels() {
    local base_url="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"

    # Try common kernel names from different Firecracker versions
    local candidates=(
        "vmlinux-5.10.225.bin"
        "vmlinux-5.10.217.bin"
        "vmlinux-5.10.210.bin"
        "vmlinux-5.10.204.bin"
        "vmlinux-5.10.198.bin"
        "vmlinux-6.1.102.bin"
        "vmlinux-6.1.96.bin"
    )

    for kernel in "${candidates[@]}"; do
        local url="${base_url}/${kernel}"
        log "  Trying: $kernel"
        if curl -fSL --progress-bar -o "$DEST" "$url" 2>/dev/null; then
            if [ -s "$DEST" ]; then
                log "  Success: $kernel"
                return 0
            fi
        fi
    done

    # Last resort: list available assets from the release
    err "Could not find a kernel binary in Firecracker ${FC_VERSION}."
    err ""
    err "Try listing available assets:"
    err "  curl -s https://api.github.com/repos/firecracker-microvm/firecracker/releases/tags/${FC_VERSION} | jq -r '.assets[].name'"
    err ""
    err "Or download manually from:"
    err "  https://github.com/firecracker-microvm/firecracker/releases/tag/${FC_VERSION}"
    rm -f "$DEST"
    return 1
}

main
