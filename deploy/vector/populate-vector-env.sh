#!/bin/bash
# populate-vector-env.sh — Fetch the Axiom platform ingest credentials
# (token + dataset name) from Azure Key Vault via the VM's managed identity,
# write them to /etc/opensandbox/vector.env so Vector picks them up at start.
#
# Runs as a one-shot systemd unit (populate-vector-env.service) before
# vector.service. Idempotent on every boot.
#
# Required env (sourced from /etc/opensandbox/worker.env or server.env by
# the systemd unit's EnvironmentFile=). This is the same var the
# autoscaler bakes into prod worker.env at VM-create time, so the
# populator picks it up automatically without any extra plumbing.
#   OPENSANDBOX_AZURE_KEY_VAULT_NAME   Key Vault name (e.g. opencomputer-prod-kv)
#
# Optional env (used to enrich Vector's host envelope for non-JSON lines):
#   OPENCOMPUTER_CELL_ID     e.g. eastus2-default
#   OPENSANDBOX_REGION       e.g. eastus2
#
# KV secrets fetched:
#   shared-axiom-platform-ingest-token         → AXIOM_PLATFORM_TOKEN          (required — logs)
#   shared-axiom-platform-dataset              → AXIOM_PLATFORM_DATASET        (required — logs)
#   shared-axiom-platform-metrics-ingest-token → AXIOM_PLATFORM_METRICS_TOKEN  (optional — metrics)
#   shared-axiom-platform-metrics-dataset      → AXIOM_PLATFORM_METRICS_DATASET (optional — metrics)
#
# Logs secrets are hard-required: missing → exit 0 without writing env file,
# Vector fails healthcheck and buffers to disk until the secret appears
# (don't break the worker boot path over a logging credential).
#
# Metrics secrets are soft-optional: missing → env file still gets written
# with logs creds set, metrics ones empty. The metrics sink fails healthcheck
# and buffers to disk independently. Lets operators roll out the metrics
# dataset asynchronously without coordinating with the worker fleet.
set -euo pipefail

VAULT_NAME="${OPENSANDBOX_AZURE_KEY_VAULT_NAME:-}"
ENV_FILE=/etc/opensandbox/vector.env
TOKEN_SECRET=shared-axiom-platform-ingest-token
DATASET_SECRET=shared-axiom-platform-dataset
METRICS_TOKEN_SECRET=shared-axiom-platform-metrics-ingest-token
METRICS_DATASET_SECRET=shared-axiom-platform-metrics-dataset

log() { logger -t populate-vector-env "$*"; echo "$*"; }

if [ -z "$VAULT_NAME" ]; then
    log "OPENSANDBOX_AZURE_KEY_VAULT_NAME not set — skipping (Vector will start without a token)"
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

# Metrics creds are soft-optional. Empty values land in the env file and the
# axiom_metrics sink in Vector fails its healthcheck; the logs sink keeps
# working. Lets operators provision the metrics dataset on their own schedule
# without coordinating with worker boots.
METRICS_TOKEN_VALUE=$(kv_get "$METRICS_TOKEN_SECRET")
METRICS_DATASET_VALUE=$(kv_get "$METRICS_DATASET_SECRET")
if [ -z "$METRICS_TOKEN_VALUE" ] || [ -z "$METRICS_DATASET_VALUE" ]; then
    log "metrics secrets ($METRICS_TOKEN_SECRET / $METRICS_DATASET_SECRET) missing — metrics sink will buffer to disk until configured"
fi

# Auto-detect HOST_IP via the kernel's source-address selection (skips link-local).
HOST_IP=$(ip route get 8.8.8.8 2>/dev/null | awk '/src/ {for(i=1;i<NF;i++) if($i=="src") print $(i+1); exit}' || true)

install -d -m 0755 /etc/opensandbox
umask 077
cat > "${ENV_FILE}.tmp" <<EOF
AXIOM_PLATFORM_TOKEN=${TOKEN_VALUE}
AXIOM_PLATFORM_DATASET=${DATASET_VALUE}
AXIOM_PLATFORM_METRICS_TOKEN=${METRICS_TOKEN_VALUE}
AXIOM_PLATFORM_METRICS_DATASET=${METRICS_DATASET_VALUE}
OPENCOMPUTER_CELL_ID=${OPENCOMPUTER_CELL_ID:-unknown}
OPENSANDBOX_REGION=${OPENSANDBOX_REGION:-unknown}
OPENCOMPUTER_HOST_IP=${HOST_IP:-unknown}
EOF
chown root:root "${ENV_FILE}.tmp"
chmod 0600 "${ENV_FILE}.tmp"
mv -f "${ENV_FILE}.tmp" "$ENV_FILE"

metrics_status="absent"
if [ -n "$METRICS_TOKEN_VALUE" ] && [ -n "$METRICS_DATASET_VALUE" ]; then
    metrics_status="present"
fi
log "populated $ENV_FILE (logs token+dataset from $VAULT_NAME, metrics=$metrics_status, host_ip=${HOST_IP:-unknown})"
