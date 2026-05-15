#!/usr/bin/env bash
# Garbage-collect stale PR stacks (older than MAX_AGE_HOURS, default 24).
#
# Lists pr-*-server VMs, checks their creation time, and tears down any older
# than the threshold. Designed to run from a GitHub Actions scheduled workflow
# (daily). Safe to re-run; idempotent.
#
# Usage:   ./gc.sh             # default: stacks >24h old
#          MAX_AGE_HOURS=4 ./gc.sh   # tighter window

set -euo pipefail

RG="${RG:-opencomputer-ci}"
MAX_AGE_HOURS="${MAX_AGE_HOURS:-24}"
DOWN_SCRIPT="${DOWN_SCRIPT:-$(dirname "$0")/../pr/down.sh}"

[[ -x "$DOWN_SCRIPT" ]] || { echo "FATAL: $DOWN_SCRIPT not executable"; exit 1; }

echo ">>> scanning pr-*-server VMs in $RG (cutoff: ${MAX_AGE_HOURS}h)"

cutoff_epoch=$(($(date -u +%s) - MAX_AGE_HOURS * 3600))

# VM names don't contain whitespace, so simple word-splitting works (and is
# portable to macOS bash 3.2, which lacks mapfile).
VMS=$(az vm list -g "$RG" --query "[?starts_with(name,'pr-') && ends_with(name,'-server')].name" -o tsv)
if [[ -z "$VMS" ]]; then
  echo "no PR stacks found."
  exit 0
fi

KILLED=0
for vm in $VMS; do
  created=$(az vm show -g "$RG" -n "$vm" --query "timeCreated" -o tsv 2>/dev/null || echo "")
  if [[ -z "$created" ]]; then
    echo "  $vm: no timeCreated metadata, skipping"
    continue
  fi

  # Strip fractional seconds + parse to epoch (BSD/macOS and Linux tolerant).
  ts=$(echo "$created" | sed -E 's/\.[0-9]+//; s/\+.*//')
  if date --version >/dev/null 2>&1; then
    created_epoch=$(date -u -d "$ts" +%s)
  else
    created_epoch=$(date -u -j -f "%Y-%m-%dT%H:%M:%S" "$ts" +%s)
  fi

  age_h=$(( ($(date -u +%s) - created_epoch) / 3600 ))
  pr_num=$(echo "$vm" | sed -E 's/^pr-([0-9]+)-server$/\1/')

  if [[ "$created_epoch" -lt "$cutoff_epoch" ]]; then
    echo "  $vm (PR $pr_num, age=${age_h}h) — TEARING DOWN"
    if "$DOWN_SCRIPT" "$pr_num"; then
      KILLED=$((KILLED + 1))
    else
      echo "    down.sh failed for PR $pr_num — continuing"
    fi
  else
    echo "  $vm (PR $pr_num, age=${age_h}h) — keeping"
  fi
done

echo "DONE. tore down $KILLED stale stack(s)."
