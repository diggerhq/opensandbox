#!/usr/bin/env bash
# setup-azure-host.sh — Provision an Azure VM for OpenSandbox with QEMU backend.
# Run as root on a fresh Ubuntu 24.04 x86_64 instance.
set -euo pipefail

echo "=== OpenSandbox Azure Host Setup ==="

# Architecture detection
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  GOARCH="amd64"; KERNEL_ARCH="x86_64" ;;
    aarch64) GOARCH="arm64";  KERNEL_ARCH="aarch64" ;;
    *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac
echo "Architecture: $ARCH (Go: $GOARCH)"

# --- System packages ---
echo "Installing system packages..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get upgrade -y -qq
apt-get install -y -qq \
    qemu-system-x86 qemu-utils \
    e2fsprogs git podman uidmap slirp4netns \
    postgresql-client jq curl zstd

# --- Docker ---
echo "Installing Docker..."
if ! command -v docker &>/dev/null; then
    curl -fsSL https://get.docker.com | sh
fi
systemctl enable --now docker

# --- Go ---
GO_VERSION="1.24.1"
if [ ! -d "/usr/local/go" ] || ! /usr/local/go/bin/go version 2>/dev/null | grep -q "$GO_VERSION"; then
    echo "Installing Go $GO_VERSION..."
    rm -rf /usr/local/go
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" | tar -C /usr/local -xzf -
fi
cat > /etc/profile.d/golang.sh << 'EOF'
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
EOF
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
echo "Go: $(/usr/local/go/bin/go version)"

# --- Kernel for QEMU ---
# Use the same kernel as Firecracker — vmlinux-docker-5.10.bin from AWS S3 quickstart.
# This kernel has CONFIG_VIRTIO_VSOCKETS, CONFIG_VIRTIO_BLK, CONFIG_VIRTIO_NET enabled.
echo "Downloading guest kernel..."
KERNEL_DIR="/opt/opensandbox"
mkdir -p "$KERNEL_DIR"
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/${KERNEL_ARCH}/kernels/vmlinux-docker-5.10.bin"
if [ ! -f "$KERNEL_DIR/vmlinux" ]; then
    curl -fsSL "$KERNEL_URL" -o "$KERNEL_DIR/vmlinux"
    chmod 644 "$KERNEL_DIR/vmlinux"
    echo "Kernel downloaded: $KERNEL_DIR/vmlinux"
else
    echo "Kernel already exists: $KERNEL_DIR/vmlinux"
fi

# --- KVM + vhost-vsock ---
echo "Loading kernel modules..."
modprobe kvm || true

# Load architecture-specific KVM module
case "$ARCH" in
    x86_64)
        modprobe kvm_intel 2>/dev/null || modprobe kvm_amd 2>/dev/null || true
        ;;
    aarch64)
        # KVM is built-in on ARM64, no separate module needed
        ;;
esac

modprobe vhost_vsock || true

# Persist modules across reboots
cat > /etc/modules-load.d/kvm.conf << 'EOF'
kvm
vhost_vsock
EOF

# Ensure /dev/kvm and /dev/vhost-vsock are accessible
chmod 666 /dev/kvm 2>/dev/null || true
chmod 666 /dev/vhost-vsock 2>/dev/null || true

# Add udev rule for persistent permissions
cat > /etc/udev/rules.d/99-opensandbox.rules << 'EOF'
KERNEL=="kvm", GROUP="kvm", MODE="0666"
KERNEL=="vhost-vsock", MODE="0666"
EOF

# --- sysctl tuning ---
cat > /etc/sysctl.d/99-opensandbox.conf << 'EOF'
# IP forwarding for VM networking
net.ipv4.ip_forward = 1
net.ipv4.conf.all.route_localnet = 1

# ARP table thresholds (many VMs = many ARP entries)
net.ipv4.neigh.default.gc_thresh1 = 1024
net.ipv4.neigh.default.gc_thresh2 = 4096
net.ipv4.neigh.default.gc_thresh3 = 8192

# File and inotify limits
fs.file-max = 1000000
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 8192
EOF
sysctl --system -q

# --- Directory structure ---
mkdir -p /data/sandboxes /data/firecracker/images /data/checkpoints /etc/opensandbox

# --- PostgreSQL + Redis containers ---
echo "Starting PostgreSQL and Redis..."
if ! docker ps --format '{{.Names}}' | grep -q '^opensandbox-postgres$'; then
    docker run -d --name opensandbox-postgres \
        --restart unless-stopped \
        -p 5432:5432 \
        -e POSTGRES_USER=opensandbox \
        -e POSTGRES_PASSWORD=opensandbox \
        -e POSTGRES_DB=opensandbox \
        -v /data/postgres:/var/lib/postgresql/data \
        postgres:16
fi

if ! docker ps --format '{{.Names}}' | grep -q '^opensandbox-redis$'; then
    docker run -d --name opensandbox-redis \
        --restart unless-stopped \
        -p 6379:6379 \
        redis:7-alpine
fi

# Wait for PostgreSQL
echo "Waiting for PostgreSQL..."
for i in $(seq 1 30); do
    if PGPASSWORD=opensandbox psql -h localhost -U opensandbox -d opensandbox -c '\q' 2>/dev/null; then
        break
    fi
    sleep 2
done

# --- systemd units ---
echo "Installing systemd units..."

cat > /etc/systemd/system/opensandbox-worker.service << 'EOF'
[Unit]
Description=OpenSandbox Worker (QEMU backend)
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/sbin/modprobe vhost_vsock
EnvironmentFile=/etc/opensandbox/worker.env
ExecStart=/usr/local/bin/opensandbox-worker
Restart=on-failure
RestartSec=5
LimitNOFILE=1000000
LimitNPROC=unlimited
KillMode=mixed
TimeoutStopSec=180

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/opensandbox-server.service << 'EOF'
[Unit]
Description=OpenSandbox Server
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/opensandbox/server.env
ExecStart=/usr/local/bin/opensandbox-server
Restart=on-failure
RestartSec=5
LimitNOFILE=1000000

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable opensandbox-worker opensandbox-server

echo "=== Azure host setup complete ==="
echo "  QEMU: $(qemu-system-x86_64 --version | head -1)"
echo "  Go:   $(/usr/local/go/bin/go version)"
echo "  KVM:  $(ls -la /dev/kvm 2>/dev/null || echo 'not available')"
echo "  VSOCK: $(ls -la /dev/vhost-vsock 2>/dev/null || echo 'not available')"
