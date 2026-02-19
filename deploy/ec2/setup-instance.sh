#!/usr/bin/env bash
set -euo pipefail

# Provision a fresh Ubuntu 24.04 EC2 instance as an OpenSandbox worker.
# Run this ON the instance (ssh in first), or pipe via ssh:
#   ssh -i key.pem ubuntu@<IP> 'bash -s' < deploy/ec2/setup-instance.sh
#
# Prerequisites:
#   - Ubuntu 24.04 LTS AMI
#   - t3.medium or larger
#   - Security group: 443 (HTTPS), 9090 (gRPC) open inbound
#   - SSH access

echo "==> Updating packages..."
sudo apt-get update && sudo apt-get upgrade -y

# -------------------------------------------------------------------
# Podman + crun (with CRIU support)
# -------------------------------------------------------------------
echo "==> Installing Podman..."
sudo apt-get install -y podman uidmap slirp4netns

echo "==> Installing crun from source (with CRIU support)..."
sudo apt-get install -y \
  make gcc pkg-config \
  libsystemd-dev libcap-dev libseccomp-dev \
  libyajl-dev go-md2man

CRUN_VERSION="1.26"
cd /tmp
git clone --branch "$CRUN_VERSION" --depth 1 https://github.com/containers/crun.git
cd crun
./autogen.sh
./configure --with-criu
make -j"$(nproc)"
sudo make install
cd / && rm -rf /tmp/crun

echo "==> Verifying crun has CRIU support..."
crun --version | grep "+CRIU" || { echo "ERROR: crun built without CRIU"; exit 1; }

# -------------------------------------------------------------------
# CRIU 4.2
# -------------------------------------------------------------------
echo "==> Installing CRIU 4.2 from source..."
sudo apt-get install -y \
  libprotobuf-dev libprotobuf-c-dev protobuf-c-compiler protobuf-compiler \
  libcap-dev libnl-3-dev libnet1-dev libbsd-dev \
  python3-protobuf python3-yaml iproute2 \
  libdrm-dev libnftables-dev pkg-config

cd /tmp
git clone --branch v4.2 --depth 1 https://github.com/checkpoint-restore/criu.git
cd criu
make -j"$(nproc)"
sudo make install PREFIX=/usr/local
cd / && rm -rf /tmp/criu

echo "==> Verifying CRIU..."
criu --version

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

echo "==> Configuring Redis..."
# Set a password — replace <REDIS_PASSWORD> with your actual password
# sudo sed -i 's/^# requirepass .*/requirepass <REDIS_PASSWORD>/' /etc/redis/redis.conf
# sudo systemctl restart redis-server
echo "    NOTE: Set a Redis password in /etc/redis/redis.conf manually"

# -------------------------------------------------------------------
# Caddy
# -------------------------------------------------------------------
echo "==> Installing Caddy..."
sudo apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt-get update
sudo apt-get install -y caddy
# Stop the default caddy service — we use our own unit
sudo systemctl stop caddy
sudo systemctl disable caddy

echo "==> Installing Caddy config..."
sudo mkdir -p /etc/caddy
sudo cp /tmp/deploy-ec2/Caddyfile /etc/caddy/Caddyfile 2>/dev/null || \
  echo "    NOTE: Copy deploy/ec2/Caddyfile to /etc/caddy/Caddyfile manually"

echo "==> Installing Caddy systemd unit..."
sudo cp /tmp/deploy-ec2/caddy.service /etc/systemd/system/caddy.service 2>/dev/null || \
  echo "    NOTE: Copy deploy/ec2/caddy.service to /etc/systemd/system/ manually"

# -------------------------------------------------------------------
# Worker setup
# -------------------------------------------------------------------
echo "==> Creating data directory..."
sudo mkdir -p /data/sandboxes

echo "==> Installing worker systemd unit..."
sudo cp /tmp/deploy-ec2/opensandbox-worker.service /etc/systemd/system/ 2>/dev/null || \
  echo "    NOTE: Copy deploy/ec2/opensandbox-worker.service to /etc/systemd/system/ manually"

echo "==> Creating env file directory..."
sudo mkdir -p /etc/opensandbox
if [ ! -f /etc/opensandbox/worker.env ]; then
  echo "    NOTE: Copy deploy/ec2/worker.env.example to /etc/opensandbox/worker.env and fill in secrets"
fi

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

# Load them now
sudo modprobe inet_diag tcp_diag udp_diag unix_diag netlink_diag

# -------------------------------------------------------------------
# Enable services
# -------------------------------------------------------------------
echo "==> Enabling services..."
sudo systemctl daemon-reload
sudo systemctl enable caddy
sudo systemctl enable opensandbox-worker

echo ""
echo "============================================"
echo " Instance setup complete!"
echo ""
echo " Remaining manual steps:"
echo "   1. Copy worker.env.example to /etc/opensandbox/worker.env and fill in secrets"
echo "   2. Copy Caddyfile to /etc/caddy/Caddyfile (if not already done)"
echo "   3. Deploy the worker binary: ./deploy/ec2/deploy-worker.sh"
echo "   4. Start services: sudo systemctl start caddy opensandbox-worker"
echo "   5. Set up wildcard DNS: *.workers.opensandbox.ai -> this instance IP"
echo "============================================"
