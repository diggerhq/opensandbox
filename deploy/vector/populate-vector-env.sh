#!/bin/bash
# populate-vector-env.sh — Fetch the Axiom platform-logs ingest credentials
# (token + dataset name) from Azure Key Vault via the VM's managed identity,
# write them to /etc/opensandbox/vector.env so Vector picks them up at start.
#
# Runs as a one-shot systemd unit (populate-vector-env.service) before
# vector.service. Idempotent on every boot.
#
# Required env (sourced from /etc/opensandbox/worker.env or server.env by
# the systemd unit's EnvironmentFile=):
#   SECRETS_VAULT_NAME       Key Vault name (e.g. opencomputer-prod-kv)
#
# Optional env (used to enrich Vector's host envelope for non-JSON lines):
#   OPENCOMPUTER_CELL_ID     e.g. eastus2-default
#   OPENSANDBOX_REGION       e.g. eastus2
#
# KV secrets fetched:
#   shared-axiom-platform-ingest-token  → AXIOM_PLATFORM_TOKEN   (required)
#   shared-axiom-platform-dataset       → AXIOM_PLATFORM_DATASET (required)
#
# Both stored under `shared-` so the same secret can be read by both
# worker and server hosts. If either is absent, the script exits 0 — Vector
# fails its healthcheck and events buffer to disk until the secret appears
# (don't break the worker boot path over a missing logging credential).
set -euo pipefail

VAULT_NAME="${SECRETS_VAULT_NAME:-}"
ENV_FILE=/etc/opensandbox/vector.env
TOKEN_SECRET=shared-axiom-platform-ingest-token
DATASET_SECRET=shared-axiom-platform-dataset

log() { logger -t populate-vector-env "$*"; echo "$*"; }

if [ -z "$VAULT_NAME" ]; then
    log "SECRETS_VAULT_NAME not set — skipping (Vector will start without a token)"
    exit 0
fi

# IMDS → AAD token for Key Vault
IMDS_RESP=$(curl -sf -H 'Metadata: true' \
    "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fvault.azure.net" \
    || true)
AAD_TOKEN=$(echo "$IMDS_RESP" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null)
if [ -z "$AAD_TOKEN" ]; then
    log "failed to acquire IMDS token (managed identity not attached?); skipping"
    exit 0
fi

# Helper: fetch one secret value or empty string.
kv_get() {
    local name=$1
    local resp
    resp=$(curl -sf -H "Authorization: Bearer $AAD_TOKEN" \
        "https://${VAULT_NAME}.vault.azure.net/secrets/${name}?api-version=7.4" \
        || true)
    echo "$resp" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("value",""))' 2>/dev/null
}

TOKEN_VALUE=$(kv_get "$TOKEN_SECRET")
if [ -z "$TOKEN_VALUE" ]; then
    log "secret $TOKEN_SECRET not found in $VAULT_NAME (or no access); skipping"
    exit 0
fi

DATASET_VALUE=$(kv_get "$DATASET_SECRET")
if [ -z "$DATASET_VALUE" ]; then
    log "secret $DATASET_SECRET not found in $VAULT_NAME (or no access); skipping — Vector won't have a dataset to ship to"
    exit 0
fi

# Auto-detect HOST_IP via the kernel's source-address selection (skips link-local).
HOST_IP=$(ip route get 8.8.8.8 2>/dev/null | awk '/src/ {for(i=1;i<NF;i++) if($i=="src") print $(i+1); exit}' || true)

install -d -m 0755 /etc/opensandbox
umask 077
cat > "${ENV_FILE}.tmp" <<EOF
AXIOM_PLATFORM_TOKEN=${TOKEN_VALUE}
AXIOM_PLATFORM_DATASET=${DATASET_VALUE}
OPENCOMPUTER_CELL_ID=${OPENCOMPUTER_CELL_ID:-unknown}
OPENSANDBOX_REGION=${OPENSANDBOX_REGION:-unknown}
OPENCOMPUTER_HOST_IP=${HOST_IP:-unknown}
EOF
chown root:root "${ENV_FILE}.tmp"
chmod 0600 "${ENV_FILE}.tmp"
mv -f "${ENV_FILE}.tmp" "$ENV_FILE"

log "populated $ENV_FILE (token+dataset from $VAULT_NAME, host_ip=${HOST_IP:-unknown})"
