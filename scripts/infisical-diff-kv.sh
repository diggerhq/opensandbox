#!/usr/bin/env bash
# infisical-diff-kv.sh — verify Infisical's resolved set for a cell matches
# the source Azure Key Vault byte-for-byte. This is the safety gate before
# flipping the Infisical → KV sync to push-authoritative. After flip,
# re-running this verifies drift is zero.
#
# Resolved set = `/shared` ∪ `/cells/<cell_id>` from Infisical (in that
# precedence order; cell-scoped wins on name collision — though our import
# guarantees no collisions).
#
# Usage:
#   scripts/infisical-diff-kv.sh <azure_kv_name> <cell_id>
#
# Exits 0 if identical (proof Infisical can take over without changing
# observed behavior). Exits 1 if mismatched, printing each difference.

set -euo pipefail

KV_NAME="${1:?usage: $0 <azure_kv_name> <cell_id>}"
CELL_ID="${2:?usage: $0 <azure_kv_name> <cell_id>}"

: "${INFISICAL_PROJECT_ID:=6f7fb48e-90bb-4fac-b9a2-c55f06ed00e7}"
: "${INFISICAL_ENV:=dev}"
: "${INFISICAL_TOKEN:?INFISICAL_TOKEN not set — run infisical login first}"

# --- gather KV side ---
KV_DUMP=$(mktemp)
trap 'rm -f "$KV_DUMP" "$INF_DUMP"' EXIT
az keyvault secret list --vault-name "$KV_NAME" --query "[].name" -o tsv 2>/dev/null \
  | while IFS= read -r name; do
      [ -z "$name" ] && continue
      val=$(az keyvault secret show --vault-name "$KV_NAME" --name "$name" --query value -o tsv 2>/dev/null)
      printf '%s\t%s\n' "$name" "$val"
    done | sort > "$KV_DUMP"

# --- gather Infisical side ---
# `infisical export --format=dotenv` returns KEY=VALUE pairs for the current
# path. We resolve /shared and /cells/<cell_id> separately and union them.
# The dotenv quoting is what we need — values with spaces / special chars
# come through correctly.
INF_DUMP=$(mktemp)
parse_dotenv() {
  # Read KEY=VALUE lines from infisical's dotenv export. Infisical wraps every
  # value in single quotes (KEY='value'); we strip them. Double quotes too in
  # case the format changes upstream.
  while IFS='=' read -r key val; do
    [ -z "$key" ] && continue
    case "$key" in \#*) continue ;; esac
    val="${val#\'}"; val="${val%\'}"
    val="${val#\"}"; val="${val%\"}"
    printf '%s\t%s\n' "$key" "$val"
  done
}

{
  infisical export \
    --projectId="$INFISICAL_PROJECT_ID" \
    --env="$INFISICAL_ENV" \
    --path="/shared" \
    --format=dotenv 2>/dev/null | parse_dotenv
  infisical export \
    --projectId="$INFISICAL_PROJECT_ID" \
    --env="$INFISICAL_ENV" \
    --path="/cells/$CELL_ID" \
    --format=dotenv 2>/dev/null | parse_dotenv
} | sort > "$INF_DUMP"

# --- diff ---
KV_COUNT=$(wc -l < "$KV_DUMP")
INF_COUNT=$(wc -l < "$INF_DUMP")
echo "KV side       ($KV_NAME):           $KV_COUNT entries"
echo "Infisical side (env=$INFISICAL_ENV, /shared + /cells/$CELL_ID):  $INF_COUNT entries"
echo

if diff -u "$KV_DUMP" "$INF_DUMP" >/dev/null; then
  echo "✓ MATCH — Infisical's resolved set matches $KV_NAME byte-for-byte."
  exit 0
else
  echo "✗ MISMATCH:"
  diff -u "$KV_DUMP" "$INF_DUMP" | head -40
  exit 1
fi
