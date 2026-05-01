#!/usr/bin/env bash
#
# bootstrap-worker-identity.sh — one-time per-region setup for the worker
# UserAssigned managed identity. Workers use this identity to fetch the
# shared secrets-proxy CA from Azure Key Vault. Without this, freshly-
# launched workers generate per-worker CAs and live migration of any
# sandbox using a secret store will fail TLS substitution.
#
# Run ONCE per region. Idempotent — re-running is safe (skips work that's
# already done). The output is the resource ID of the identity; set it as
# OPENSANDBOX_AZURE_WORKER_IDENTITY_ID in the control plane's server.env.
#
# Usage:
#   bash bootstrap-worker-identity.sh <resource-group> <key-vault-name> [identity-name]
#
# Example:
#   bash bootstrap-worker-identity.sh opensandbox-prod opensandbox-dev-kv
#
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <resource-group> <key-vault-name> [identity-name]" >&2
  exit 1
fi

RG="$1"
KV="$2"
NAME="${3:-osb-worker-identity}"

LOCATION=$(az group show --name "$RG" --query location -o tsv)
echo "==> resource-group: $RG (location=$LOCATION)"
echo "==> key-vault: $KV"
echo "==> identity name: $NAME"

# 1. Create UserAssigned identity (idempotent — show first, create if missing).
if EXISTING=$(az identity show --resource-group "$RG" --name "$NAME" --query id -o tsv 2>/dev/null); then
  echo "==> identity already exists: $EXISTING"
  IDENTITY_ID="$EXISTING"
else
  echo "==> creating identity..."
  IDENTITY_ID=$(az identity create --resource-group "$RG" --name "$NAME" --location "$LOCATION" --query id -o tsv)
  echo "==> created: $IDENTITY_ID"
  # Wait for the identity's principalId to be queryable (eventual consistency).
  sleep 5
fi

PRINCIPAL_ID=$(az identity show --resource-group "$RG" --name "$NAME" --query principalId -o tsv)
echo "==> principalId: $PRINCIPAL_ID"

# 2. Grant "Key Vault Secrets Officer" on the regional KV (RBAC mode).
KV_ID=$(az keyvault show --name "$KV" --query id -o tsv)
echo "==> granting Key Vault Secrets Officer on $KV_ID..."
# az role assignment create is idempotent: returns an error if it already
# exists, but the role is granted either way. Grep the error to ignore it.
if ! az role assignment create \
    --assignee-object-id "$PRINCIPAL_ID" \
    --assignee-principal-type ServicePrincipal \
    --role "Key Vault Secrets Officer" \
    --scope "$KV_ID" \
    -o none 2>&1 | tee /tmp/_role_out; then
  if grep -q "RoleAssignmentExists" /tmp/_role_out; then
    echo "==> role already assigned (idempotent OK)"
  else
    echo "ERROR: role assignment failed" >&2
    cat /tmp/_role_out >&2
    exit 1
  fi
fi

echo ""
echo "===================================================================="
echo "Done. Set this in the control plane's /etc/opensandbox/server.env:"
echo ""
echo "OPENSANDBOX_AZURE_WORKER_IDENTITY_ID=$IDENTITY_ID"
echo ""
echo "Then restart opensandbox-server. New worker VMs will attach this"
echo "identity automatically. Existing workers need a manual roll to pick"
echo "it up (terminate, scaler relaunches with identity attached)."
echo "===================================================================="
