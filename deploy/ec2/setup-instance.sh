#!/usr/bin/env bash
set -euo pipefail

# Provision a fresh Ubuntu 24.04 EC2 instance as an OpenSandbox worker.
# Run this ON the instance (ssh in first), or pipe via ssh:
#   ssh -i key.pem ubuntu@<IP> 'bash -s' < deploy/ec2/setup-instance.sh
#
# Supports both x86_64 (amd64) and aarch64 (arm64/Graviton) instances.
#
# Prerequisites:
#   - Ubuntu 24.04 LTS AMI
#   - t3.medium or larger (x86) / r7gd.medium or larger (arm64)
#   - Security group: 443 (HTTPS), 9090 (gRPC) open inbound
#   - SSH access

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GOARCH="amd64" ;;
  aarch64) GOARCH="arm64" ;;
  *)       echo "ERROR: Unsupported architecture: $ARCH"; exit 1 ;;
esac
echo "==> Detected architecture: $ARCH ($GOARCH)"

echo "==> Updating packages..."
sudo apt-get update && sudo apt-get upgrade -y

# -------------------------------------------------------------------
# Podman (runtime will be configured after crun is built)
# -------------------------------------------------------------------
echo "==> Installing Podman..."
sudo apt-get install -y podman uidmap slirp4netns

# -------------------------------------------------------------------
# CRIU 4.2 (must be installed BEFORE crun so headers are available)
# -------------------------------------------------------------------
echo "==> Installing CRIU 4.2 from source..."
sudo apt-get install -y \
  make gcc pkg-config git \
  libprotobuf-dev libprotobuf-c-dev protobuf-c-compiler protobuf-compiler \
  libcap-dev libnl-3-dev libnet1-dev libbsd-dev \
  python3-protobuf python3-yaml iproute2 \
  libdrm-dev libnftables-dev uuid-dev libaio-dev

cd /tmp
rm -rf /tmp/criu
git clone --branch v4.2 --depth 1 https://github.com/checkpoint-restore/criu.git
cd criu
make -j"$(nproc)"
sudo make install-criu PREFIX=/usr/local
sudo make install-lib PREFIX=/usr/local
sudo ldconfig
cd / && rm -rf /tmp/criu

echo "==> Verifying CRIU..."
criu --version

# -------------------------------------------------------------------
# crun 1.26 (with CRIU support — requires CRIU headers from above)
# -------------------------------------------------------------------
echo "==> Installing crun from source (with CRIU support)..."
sudo apt-get install -y \
  libsystemd-dev libseccomp-dev \
  libyajl-dev go-md2man autoconf automake libtool

cd /tmp
rm -rf /tmp/crun
git clone --branch "1.26" --depth 1 https://github.com/containers/crun.git
cd crun
export PKG_CONFIG_PATH="/usr/local/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
./autogen.sh
./configure --with-criu
make -j"$(nproc)"
sudo make install
cd / && rm -rf /tmp/crun

echo "==> Verifying crun has CRIU support..."
crun --version | grep "+CRIU" || { echo "ERROR: crun built without CRIU"; exit 1; }

# -------------------------------------------------------------------
# Configure Podman to use crun
# -------------------------------------------------------------------
echo "==> Configuring Podman to use crun..."
sudo mkdir -p /etc/containers
sudo tee /etc/containers/containers.conf > /dev/null <<'EOF'
[engine]
runtime = "crun"
EOF

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
# NVMe instance storage (handled at boot by opensandbox-nvme.service)
# -------------------------------------------------------------------
echo "==> Installing NVMe boot service..."
sudo apt-get install -y xfsprogs nvme-cli

sudo tee /usr/local/bin/opensandbox-nvme-setup.sh > /dev/null << 'NVME'
#!/usr/bin/env bash
set -euo pipefail
MOUNT_POINT="/data/sandboxes"
mkdir -p "$MOUNT_POINT"
if mountpoint -q "$MOUNT_POINT"; then
  echo "opensandbox-nvme: $MOUNT_POINT already mounted"
  exit 0
fi
NVME_DEV=""
for dev in /dev/nvme1n1 /dev/nvme2n1 /dev/nvme3n1 /dev/nvme4n1; do
  if [ -b "$dev" ]; then
    NVME_DEV="$dev"
    break
  fi
done
if [ -z "$NVME_DEV" ]; then
  echo "opensandbox-nvme: no NVMe instance storage found, using root disk"
  exit 0
fi
echo "opensandbox-nvme: formatting $NVME_DEV as XFS with project quotas"
mkfs.xfs -f "$NVME_DEV"
mount -o prjquota "$NVME_DEV" "$MOUNT_POINT"
echo "opensandbox-nvme: mounted $NVME_DEV at $MOUNT_POINT"
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
# Worker systemd unit
# -------------------------------------------------------------------
echo "==> Installing worker systemd unit..."
sudo tee /etc/systemd/system/opensandbox-worker.service > /dev/null << 'SVC'
[Unit]
Description=OpenSandbox Worker
After=network-online.target opensandbox-nvme.service opensandbox-identity.service
Wants=network-online.target
Requires=opensandbox-nvme.service opensandbox-identity.service

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
Environment=HOME=/root
Environment=OPENSANDBOX_MODE=worker
Environment=OPENSANDBOX_PORT=8080
Environment=OPENSANDBOX_REGION=use2
Environment=OPENSANDBOX_DATA_DIR=/data/sandboxes
Environment=OPENSANDBOX_SANDBOX_DOMAIN=workers.opensandbox.ai
Environment=OPENSANDBOX_SECRETS_ARN=arn:aws:secretsmanager:us-east-2:739940681129:secret:opensandbox/worker-vtN2Ez
EnvironmentFile=/etc/opensandbox/worker-identity.env

[Install]
WantedBy=multi-user.target
SVC

sudo mkdir -p /etc/opensandbox /data/sandboxes

# -------------------------------------------------------------------
# Kernel modules for CRIU (loaded at boot)
# -------------------------------------------------------------------
echo "==> Configuring kernel modules for CRIU..."
sudo tee /etc/modules-load.d/opensandbox.conf > /dev/null <<EOF
inet_diag
tcp_diag
udp_diag
unix_diag
netlink_diag
EOF

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
echo " Remaining steps:"
echo "   1. Deploy the worker binary: ./deploy/ec2/deploy-worker.sh"
echo "   2. Copy Caddyfile to /etc/caddy/Caddyfile"
echo "   3. Start services: sudo systemctl start opensandbox-worker"
echo "   4. Set up wildcard DNS: *.workers.opensandbox.ai -> this instance IP"
echo "============================================"
