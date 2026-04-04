#!/usr/bin/env bash
set -euo pipefail

# setup-single-host.sh — Provision an Ubuntu 24.04 EC2 instance for OpenSandbox
#
# Runs BOTH control plane (server) and Firecracker worker on the same instance.
# This is the SINGLE source of truth for instance provisioning — both user_data
# (terraform) and manual setup call this script.
#
# Usage (manual):
#   ssh -i key.pem ubuntu@<IP> 'bash -s' < deploy/ec2/setup-single-host.sh
#
# Usage (terraform user_data):
#   Called automatically on first boot.
#
# What gets installed:
#   - Docker (for PostgreSQL + Redis containers)
#   - Go 1.24.1 (for building binaries on-instance)
#   - Firecracker v1.9.1 (microVM runtime)
#   - Firecracker kernel (from S3 quickstart bucket)
#   - Podman + e2fsprogs (for rootfs builds)
#   - PostgreSQL client (for seeding/debugging)
#   - sysctl tuning + IP forwarding for VM networking
#   - PostgreSQL + Redis containers
#   - systemd units for server + worker
#
# Supports both x86_64 and aarch64.

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH="amd64"; FC_ARCH="x86_64" ;;
  aarch64) GOARCH="arm64"; FC_ARCH="aarch64" ;;
  *)       echo "ERROR: Unsupported architecture: $ARCH"; exit 1 ;;
esac
echo "==> Detected architecture: $ARCH ($GOARCH)"

###############################################################################
# 1. System packages
###############################################################################
echo "==> Updating packages..."
export DEBIAN_FRONTEND=noninteractive
sudo apt-get update && sudo apt-get upgrade -y

echo "==> Installing build dependencies..."
sudo apt-get install -y e2fsprogs git podman uidmap slirp4netns postgresql-client

###############################################################################
# 2. Docker
###############################################################################
echo "==> Installing Docker..."
if ! command -v docker &>/dev/null; then
    curl -fsSL https://get.docker.com | sudo sh
    sudo usermod -aG docker "${USER}"
fi
sudo systemctl enable docker
sudo systemctl start docker
echo "    Docker $(docker --version)"

###############################################################################
# 3. Go
###############################################################################
echo "==> Installing Go..."
GO_VERSION="1.24.1"
if [ ! -d /usr/local/go ] || ! /usr/local/go/bin/go version 2>/dev/null | grep -q "$GO_VERSION"; then
    curl -fSL -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz"
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf /tmp/go.tar.gz
    rm -f /tmp/go.tar.gz
fi
# Make Go available system-wide
sudo tee /etc/profile.d/golang.sh > /dev/null << 'GOENV'
export GOROOT=/usr/local/go
export GOPATH=$HOME/go
export PATH=$GOROOT/bin:$GOPATH/bin:$PATH
GOENV
sudo chmod +x /etc/profile.d/golang.sh
export PATH=/usr/local/go/bin:$PATH
echo "    Go $(/usr/local/go/bin/go version)"

###############################################################################
# 4. Firecracker
###############################################################################
echo "==> Installing Firecracker..."
FC_VERSION="v1.9.1"
if ! command -v firecracker &>/dev/null; then
    FC_RELEASE="firecracker-${FC_VERSION}-${FC_ARCH}"
    FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/${FC_RELEASE}.tgz"
    cd /tmp
    curl -fSL -o firecracker.tgz "$FC_URL"
    tar xzf firecracker.tgz
    sudo cp "release-${FC_VERSION}-${FC_ARCH}/firecracker-${FC_VERSION}-${FC_ARCH}" /usr/local/bin/firecracker
    sudo chmod +x /usr/local/bin/firecracker
    rm -rf firecracker.tgz "release-${FC_VERSION}-${FC_ARCH}"
    cd /
fi
echo "    Firecracker $(firecracker --version 2>&1 | head -1)"

# Verify KVM access
if [ ! -e /dev/kvm ]; then
    echo "WARNING: /dev/kvm not found. Firecracker requires bare-metal or nested virt."
fi
sudo chmod 666 /dev/kvm 2>/dev/null || true

###############################################################################
# 5. Guest kernel — Ubuntu generic kernel with full module support
###############################################################################
echo "==> Setting up guest kernel..."
sudo mkdir -p /opt/opensandbox

# Install Ubuntu generic kernel (has virtio built-in, full module ecosystem).
# This gives us a kernel + matching modules that support Docker networking
# (bridge, veth, netfilter), vsock, overlay, virtio_mem, etc.
sudo apt-get install -y -qq linux-image-generic

GENERIC_VMLINUZ=$(ls -t /boot/vmlinuz-*-generic 2>/dev/null | head -1)
if [ -n "$GENERIC_VMLINUZ" ]; then
    sudo cp "$GENERIC_VMLINUZ" /opt/opensandbox/vmlinux
    sudo chmod 644 /opt/opensandbox/vmlinux
    GENERIC_KVER=$(basename "$GENERIC_VMLINUZ" | sed 's/vmlinuz-//')
    echo "    Guest kernel: $GENERIC_VMLINUZ ($GENERIC_KVER)"

    # Install full kernel modules for the guest
    sudo apt-get install -y -qq "linux-modules-$GENERIC_KVER" 2>/dev/null || true
    sudo apt-get install -y -qq "linux-modules-extra-$GENERIC_KVER" 2>/dev/null || true

    # Store kernel version for the rootfs build
    echo "$GENERIC_KVER" | sudo tee /opt/opensandbox/guest-kernel-version >/dev/null
    echo "    Modules installed for $GENERIC_KVER"
else
    echo "    WARNING: No generic kernel found, falling back to Firecracker kernel"
    if [ ! -f /opt/opensandbox/vmlinux ]; then
        case "$ARCH" in
          x86_64)  KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux-docker-5.10.bin" ;;
          aarch64) KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/aarch64/kernels/vmlinux-docker-5.10.bin" ;;
        esac
        sudo curl -fSL -o /opt/opensandbox/vmlinux "$KERNEL_URL"
        sudo chmod 644 /opt/opensandbox/vmlinux
    fi
fi

###############################################################################
# 6. KVM modules + sysctl tuning
###############################################################################
echo "==> Configuring KVM and sysctl..."
sudo modprobe kvm || echo "    kvm module not loadable (may be built-in)"
if [ "$ARCH" = "aarch64" ]; then
    sudo modprobe kvm_arm || true
else
    sudo modprobe kvm_intel || sudo modprobe kvm_amd || true
fi
sudo modprobe vhost_vsock || echo "    vhost_vsock module not loadable"

sudo tee /etc/modules-load.d/kvm.conf > /dev/null << 'MODULES'
kvm
vhost_vsock
MODULES

sudo tee /etc/sysctl.d/99-opensandbox.conf > /dev/null << 'SYSCTL'
net.ipv4.ip_forward = 1
net.ipv4.neigh.default.gc_thresh1 = 1024
net.ipv4.neigh.default.gc_thresh2 = 4096
net.ipv4.neigh.default.gc_thresh3 = 8192
fs.file-max = 1000000
fs.inotify.max_user_instances = 8192
fs.inotify.max_user_watches = 524288
SYSCTL
sudo sysctl --system

###############################################################################
# 7. Create directory structure
###############################################################################
echo "==> Creating directory structure..."
sudo mkdir -p /data/sandboxes /data/firecracker/images /data/checkpoints /etc/opensandbox

###############################################################################
# 8. PostgreSQL + Redis containers
###############################################################################
echo "==> Starting PostgreSQL + Redis containers..."
if ! docker ps --format '{{.Names}}' | grep -q '^postgres$'; then
    docker rm -f postgres 2>/dev/null || true
    docker run -d \
        --name postgres \
        --restart unless-stopped \
        -p 5432:5432 \
        -e POSTGRES_USER=opensandbox \
        -e POSTGRES_PASSWORD=opensandbox \
        -e POSTGRES_DB=opensandbox \
        -v pgdata:/var/lib/postgresql/data \
        postgres:16
    echo "    PostgreSQL started"
else
    echo "    PostgreSQL already running"
fi

if ! docker ps --format '{{.Names}}' | grep -q '^redis$'; then
    docker rm -f redis 2>/dev/null || true
    docker run -d \
        --name redis \
        --restart unless-stopped \
        -p 6379:6379 \
        redis:7-alpine
    echo "    Redis started"
else
    echo "    Redis already running"
fi

echo "    Waiting for PostgreSQL to be ready..."
for i in $(seq 1 30); do
    if docker exec postgres pg_isready -U opensandbox -q 2>/dev/null; then
        echo "    PostgreSQL ready"
        break
    fi
    sleep 1
done

###############################################################################
# 9. Systemd units
###############################################################################
echo "==> Installing systemd units..."
sudo tee /etc/systemd/system/opensandbox-worker.service > /dev/null << 'SVC'
[Unit]
Description=OpenSandbox Worker (Firecracker)
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
ExecStartPre=/sbin/modprobe inet_diag
ExecStartPre=/sbin/modprobe tcp_diag
ExecStartPre=/sbin/modprobe udp_diag
ExecStartPre=/sbin/modprobe unix_diag
ExecStartPre=/sbin/modprobe netlink_diag
ExecStart=/usr/local/bin/opensandbox-worker
Restart=always
RestartSec=5
EnvironmentFile=/etc/opensandbox/worker.env
KillMode=process
TimeoutStopSec=300
LimitNOFILE=1000000
LimitNPROC=65536

[Install]
WantedBy=multi-user.target
SVC

sudo tee /etc/systemd/system/opensandbox-server.service > /dev/null << 'SVC'
[Unit]
Description=OpenSandbox Server (Control Plane)
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
ExecStart=/usr/local/bin/opensandbox-server
Restart=always
RestartSec=5
EnvironmentFile=/etc/opensandbox/server.env

[Install]
WantedBy=multi-user.target
SVC

sudo systemctl daemon-reload

echo ""
echo "============================================"
echo " Instance setup complete! ($ARCH)"
echo ""
echo " Installed:"
echo "   - Firecracker $(firecracker --version 2>&1 | head -1)"
echo "   - Docker $(docker --version | head -1)"
echo "   - Podman $(podman --version)"
echo "   - Go $(/usr/local/go/bin/go version | awk '{print $3}')"
echo "   - Kernel: $(ls -lh /opt/opensandbox/vmlinux | awk '{print $5}')"
echo ""
echo " Next: run 'make deploy-dev' from your laptop"
echo "============================================"
