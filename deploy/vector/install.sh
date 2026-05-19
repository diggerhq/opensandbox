#!/usr/bin/env bash
# install.sh — Install Vector on a host and wire it to a role-specific config.
#
# Usage (run as root on the target host):
#   ./install.sh worker         # for prod worker VMs (reads journald)
#   ./install.sh control-plane  # for prod control plane VM (reads journald)
#   ./install.sh dev-host       # single VM running both server + worker as systemd
#
# Two ways the env file (with the Axiom token) reaches the host:
#
#   1. Production (Azure VMs with managed identity to KV): populate-vector-env.service
#      is installed alongside vector.service. At each boot it fetches the
#      `shared-axiom-platform-ingest-token` secret from $SECRETS_VAULT_NAME and
#      writes /etc/opensandbox/vector.env. Vector starts after. No human in the
#      loop, no static token on disk.
#
#   2. Operator-managed (dev hosts, ad-hoc): the operator writes
#      /etc/opensandbox/vector.env manually before running this script (or any
#      other way). The KV oneshot finds no SECRETS_VAULT_NAME and exits clean,
#      and Vector reads whatever the operator put there.
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

# --- Install the KV → env-file populator (oneshot, runs Before=vector.service) ---
# Plus a companion wait unit that polls for worker.env asynchronously when
# the main populator races ahead of cloud-init on Azure first boot. All
# four files are tracked in this dir so they roll atomically with config
# changes.
install -m 0755 "$SCRIPT_DIR/populate-vector-env.sh" /usr/local/bin/populate-vector-env.sh
install -m 0755 "$SCRIPT_DIR/populate-vector-env-wait.sh" /usr/local/bin/populate-vector-env-wait.sh
install -m 0644 "$SCRIPT_DIR/populate-vector-env.service" \
    /etc/systemd/system/populate-vector-env.service
install -m 0644 "$SCRIPT_DIR/populate-vector-env-wait.service" \
    /etc/systemd/system/populate-vector-env-wait.service

# --- Wire env file + KV oneshot into the vector.service unit ---
# Use a drop-in instead of editing the package file so a Vector upgrade
# doesn't clobber our changes.
mkdir -p /etc/systemd/system/vector.service.d
cat > /etc/systemd/system/vector.service.d/override.conf <<'EOF'
[Unit]
# Soft dependency: if the KV fetch fails, vector still starts so we
# preserve whatever state was on disk from a previous boot.
Wants=populate-vector-env.service
After=populate-vector-env.service

[Service]
EnvironmentFile=-/etc/opensandbox/worker.env
EnvironmentFile=-/etc/opensandbox/server.env
EnvironmentFile=-/etc/opensandbox/vector.env
# Vector needs to read journald — add the user to the right group.
SupplementaryGroups=systemd-journal
EOF

# --- Auto-detect HOST_IP if not provisioned ---
# Vector enriches log lines with OPENCOMPUTER_HOST_IP from the env file. If the
# operator didn't set it (and the KV oneshot hasn't run yet), fill it in from
# the primary interface so the field isn't "unknown" in Axiom.
if [ -f /etc/opensandbox/vector.env ] && \
   ! grep -q '^OPENCOMPUTER_HOST_IP=' /etc/opensandbox/vector.env 2>/dev/null; then
    # `ip route get` returns the kernel's chosen source IP for outbound — bypasses
    # Azure's 169.254.169.253 IMDS address which is also scope=global on lo.
    HOST_IP=$(ip route get 8.8.8.8 2>/dev/null | \
        awk '/src/ {for(i=1;i<NF;i++) if($i=="src") print $(i+1); exit}')
    if [ -n "$HOST_IP" ]; then
        echo "OPENCOMPUTER_HOST_IP=$HOST_IP" >> /etc/opensandbox/vector.env
        echo "Detected HOST_IP=$HOST_IP"
    fi
fi

# --- Start ---
systemctl daemon-reload
# The wait unit is started imperatively by populate-vector-env.sh only
# when role env is missing — don't enable it for boot, just have it
# present so the populator can `systemctl start` it on demand.
systemctl enable populate-vector-env.service vector.service

# In a Packer image bake, systemd may not actually run units (the image is
# captured before reboot). Skip the start in that case so packer's
# deprovision step doesn't trip over a started service. PACKER_BUILD is set
# by the Packer provisioner.
if [ -z "${PACKER_BUILD:-}" ]; then
    systemctl restart populate-vector-env.service 2>&1 || true
    systemctl restart vector
    sleep 2
    systemctl status vector --no-pager -l | head -15
fi

echo
echo "=== Done ==="
echo "Check vector status:    systemctl status vector"
echo "Tail vector logs:       journalctl -u vector -f"
echo "Confirm Axiom ingest:   look for events in dataset \${AXIOM_PLATFORM_DATASET}"
