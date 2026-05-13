#!/usr/bin/env bash
# install.sh — Install Vector on a host and wire it to a role-specific config.
#
# Usage (run as root on the target host):
#   ./install.sh worker         # for worker VMs (reads journald)
#   ./install.sh control-plane  # for the control plane host (reads docker logs)
#   ./install.sh dev-host       # single VM running both server + worker as systemd
#
# Prereqs on the host:
#   - /etc/opensandbox/vector.env exists with:
#       AXIOM_PLATFORM_TOKEN=...        (required)
#       AXIOM_PLATFORM_DATASET=...      (optional; default oc-platform-logs)
#       OPENSANDBOX_CELL_ID=...         (recommended)
#       OPENSANDBOX_HOST_IP=...         (recommended; auto-detected if absent)
#
# The role configs themselves expect either:
#   - opensandbox-worker.service in journald (worker role), or
#   - a docker container named opensandbox-server (control-plane role).
set -euo pipefail

ROLE="${1:-}"
case "$ROLE" in
    worker|control-plane|dev-host) ;;
    *)
        echo "usage: $0 worker|control-plane|dev-host" >&2
        exit 2
        ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_SRC="$SCRIPT_DIR/${ROLE}.yaml"
if [ ! -f "$CONFIG_SRC" ]; then
    echo "config not found: $CONFIG_SRC" >&2
    exit 1
fi

echo "=== Installing Vector (role: $ROLE) ==="

# --- Install Vector via official setup script ---
# setup.vector.dev configures the right apt repo + key for the host's distro.
# (Vector moved off repositories.timber.io after the Datadog acquisition; the
# old URL no longer resolves.)
if ! command -v vector &>/dev/null; then
    echo "Installing Vector..."
    bash -c "$(curl -fsSL https://setup.vector.dev)"
    apt-get install -y -qq vector
fi
vector --version

# --- Drop the role config ---
mkdir -p /etc/vector /var/lib/vector
install -m 0644 "$CONFIG_SRC" /etc/vector/vector.yaml
chown -R vector:vector /var/lib/vector

# --- Wire env file into the systemd unit ---
# The packaged systemd unit is /lib/systemd/system/vector.service. Use a
# drop-in instead of editing the package file so a Vector upgrade doesn't
# clobber our changes.
mkdir -p /etc/systemd/system/vector.service.d
cat > /etc/systemd/system/vector.service.d/override.conf <<'EOF'
[Service]
EnvironmentFile=-/etc/opensandbox/worker.env
EnvironmentFile=-/etc/opensandbox/vector.env
# Vector needs to read journald (workers) and /var/lib/docker (control plane).
# The packaged unit runs as the 'vector' user — add it to the right groups.
SupplementaryGroups=systemd-journal docker
EOF

# --- Auto-detect HOST_IP if not provisioned ---
# Vector enriches log lines with OPENSANDBOX_HOST_IP from the env file. If the
# operator didn't set it, fill it in from the primary interface so the field
# isn't "unknown" in Axiom.
if ! grep -q '^OPENSANDBOX_HOST_IP=' /etc/opensandbox/vector.env 2>/dev/null; then
    # Take the source IP that the kernel would use for outbound traffic.
    # Filtering by `scope global` isn't enough on Azure: 169.254.169.253
    # (IMDS) is also advertised as scope global on `lo`. ip route get gives
    # the actual primary interface address (10.x on Azure, 172.x on EC2).
    HOST_IP=$(ip route get 8.8.8.8 2>/dev/null | awk '/src/ {for(i=1;i<NF;i++) if($i=="src") print $(i+1); exit}')
    if [ -n "$HOST_IP" ]; then
        mkdir -p /etc/opensandbox
        echo "OPENSANDBOX_HOST_IP=$HOST_IP" >> /etc/opensandbox/vector.env
        echo "Detected HOST_IP=$HOST_IP"
    fi
fi

# --- Start ---
systemctl daemon-reload
systemctl enable --now vector
sleep 2
systemctl status vector --no-pager -l | head -20

echo
echo "=== Done ==="
echo "Check vector status:    systemctl status vector"
echo "Tail vector logs:       journalctl -u vector -f"
echo "Confirm Axiom ingest:   look for events in dataset \${AXIOM_PLATFORM_DATASET:-oc-platform-logs}"
