#!/usr/bin/env bash
# infisical-import-from-kv.sh — one-time seed: read every secret from an Azure
# Key Vault and push it to Infisical, classifying each as shared (env-wide) or
# cell-scoped. Idempotent — re-running updates existing secrets in place.
#
# After this completes, the diff script (infisical-diff.sh) should confirm
# Infisical's resolved view matches the source KV byte-for-byte. Only then is
# it safe to flip the Infisical → KV sync to push-authoritative.
#
# Usage:
#   scripts/infisical-import-from-kv.sh <azure_kv_name> <cell_id> [--dry-run]
#
# Example:
#   scripts/infisical-import-from-kv.sh osb-dev2-d656dc azure-us-west-2-b --dry-run
#   scripts/infisical-import-from-kv.sh osb-dev2-d656dc azure-us-west-2-b
#
# Requires:
#   INFISICAL_TOKEN env var (issued by `infisical login --method=universal-auth`)
#   az login + read access to the source KV
#   INFISICAL_PROJECT_ID env var (defaults to opencomputer)

set -euo pipefail

KV_NAME="${1:?usage: $0 <azure_kv_name> <cell_id> [--dry-run]}"
CELL_ID="${2:?usage: $0 <azure_kv_name> <cell_id> [--dry-run]}"
DRY_RUN=0
[ "${3:-}" = "--dry-run" ] && DRY_RUN=1

: "${INFISICAL_PROJECT_ID:=6f7fb48e-90bb-4fac-b9a2-c55f06ed00e7}"
: "${INFISICAL_ENV:=dev}"

if [ -z "${INFISICAL_TOKEN:-}" ]; then
  echo "ERROR: INFISICAL_TOKEN not set. Run:" >&2
  echo "  export INFISICAL_UNIVERSAL_AUTH_CLIENT_ID=..." >&2
  echo "  export INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET=..." >&2
  echo "  export INFISICAL_TOKEN=\$(infisical login --method=universal-auth --silent --plain)" >&2
  exit 1
fi

# Classify a KV secret name as either "shared" or "cell".
#
# Rules (ordered):
#   - Anything under the `worker-global-blob-*` umbrella → shared (Tigris is
#     a single global store; every cell reads the same bucket+creds).
#   - URLs/endpoints that point at the *edge* (cf-event-endpoint, halt-list,
#     api-key, all jwt/HMAC secrets) → shared.
#   - Everything else with worker-/server- prefix (cell-id, region, s3-*,
#     database-url, redis-url, capacity tuning) → cell-scoped.
classify() {
  local n="$1"
  case "$n" in
    worker-global-blob-*) echo "shared" ;;
    *-cf-event-endpoint|*-cf-event-secret|*-cf-admin-secret) echo "shared" ;;
    *-jwt-secret|*-session-jwt-secret|*-api-key) echo "shared" ;;
    *-secret-encryption-key|*-stripe-*|*-workos-*|*-sentry-dsn|*-segment-write-key) echo "shared" ;;
    *-halt-list-url) echo "shared" ;;
    pg-*) echo "cell" ;;  # pg-password is per-cell (each cell's own PG)
    *) echo "cell" ;;
  esac
}

infisical_path() {
  case "$1" in
    shared) echo "/shared" ;;
    cell)   echo "/cells/$CELL_ID" ;;
  esac
}

echo "=== import plan: $KV_NAME → Infisical project $INFISICAL_PROJECT_ID env=$INFISICAL_ENV ==="
[ "$DRY_RUN" -eq 1 ] && echo "    (DRY RUN — no writes)"
echo

NAMES=$(az keyvault secret list --vault-name "$KV_NAME" --query "[].name" -o tsv 2>/dev/null | sort)
SHARED_COUNT=0
CELL_COUNT=0
FAIL_COUNT=0

while IFS= read -r name; do
  [ -z "$name" ] && continue
  bucket=$(classify "$name")
  path=$(infisical_path "$bucket")
  value=$(az keyvault secret show --vault-name "$KV_NAME" --name "$name" --query value -o tsv 2>/dev/null)
  if [ -z "$value" ]; then
    echo "  SKIP   $name  (empty value)"
    continue
  fi
  printf "  %-7s %s → %s\n" "$bucket" "$name" "$path"
  if [ "$DRY_RUN" -eq 0 ]; then
    # `infisical secrets set` upserts by default; --type shared for non-personal.
    if ! infisical secrets set \
        --projectId="$INFISICAL_PROJECT_ID" \
        --env="$INFISICAL_ENV" \
        --path="$path" \
        --silent \
        "$name=$value" >/dev/null 2>&1; then
      echo "    FAILED to set $name"
      FAIL_COUNT=$((FAIL_COUNT+1))
      continue
    fi
  fi
  [ "$bucket" = "shared" ] && SHARED_COUNT=$((SHARED_COUNT+1)) || CELL_COUNT=$((CELL_COUNT+1))
done <<< "$NAMES"

echo
echo "=== summary ==="
echo "  shared:  $SHARED_COUNT secrets → /shared"
echo "  cell:    $CELL_COUNT secrets → /cells/$CELL_ID"
echo "  failed:  $FAIL_COUNT"
[ "$DRY_RUN" -eq 1 ] && echo "  (dry-run — re-run without --dry-run to apply)"
