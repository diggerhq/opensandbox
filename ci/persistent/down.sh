#!/usr/bin/env bash
# Tear down the entire opencomputer-ci RG. Big red button — only for full reset.
# Storage account contents (compat-corpus + checkpoints) and KV secrets are
# unrecoverable after this runs (KV soft-delete retains for 90 days; storage
# is gone). Confirm before invoking.

set -euo pipefail

RG="${RG:-opencomputer-ci}"

if ! az group exists --name "$RG" 2>/dev/null | grep -q true; then
  echo "RG $RG doesn't exist; nothing to do."
  exit 0
fi

echo ">>> About to delete EVERYTHING in resource group: $RG"
echo "    (KV: opencomputer-ci-kv goes into 90-day soft-delete)"
echo "    (Storage: data is permanently lost)"
read -r -p "Type 'yes' to proceed: " confirm
[[ "$confirm" == "yes" ]] || { echo "aborted."; exit 1; }

az group delete -n "$RG" --yes --no-wait
echo "delete initiated (running in background, takes ~5 min to complete)."
