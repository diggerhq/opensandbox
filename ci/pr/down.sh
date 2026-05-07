#!/usr/bin/env bash
# Tear down a PR's ephemeral test stack.
#
# Usage: ./down.sh <PR_NUM>
#
# Deletes: pr-<num>-* VMs (and their NICs/disks/IPs), pr-<num>-checkpoints
# storage container, and the pr_<num> Postgres database. Leaves persistent
# resources (data VM, KV, storage account) untouched.

set -euo pipefail

PR_NUM="${1:-}"
[[ -n "$PR_NUM" ]] || { echo "usage: $0 <PR_NUM>"; exit 1; }
[[ "$PR_NUM" =~ ^[0-9]+$ ]] || { echo "PR_NUM must be numeric"; exit 1; }

RG="opencomputer-ci"
KV="opencomputer-ci-kv"
SSH_KEY_PRIV="$HOME/.ssh/opencomputer-ci"
SSH_OPTS="-i $SSH_KEY_PRIV -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"

DATA_VM_PIP=$(az vm show -d -g "$RG" -n oc-ci-data --query publicIps -o tsv 2>/dev/null || echo "")
PG_PASS=$(az keyvault secret show --vault-name "$KV" --name pg-password --query value -o tsv 2>/dev/null || echo "")
STORAGE=$(az keyvault secret show --vault-name "$KV" --name storage-account-name --query value -o tsv 2>/dev/null || echo "")
STORAGE_KEY=$(az keyvault secret show --vault-name "$KV" --name worker-s3-secret-key --query value -o tsv 2>/dev/null || echo "")

DB_NAME="pr_$PR_NUM"
CONTAINER="pr-${PR_NUM}-checkpoints"

echo ">>> [1/3] delete VMs (and their NICs/disks/IPs)"
VMS=$(az vm list -g "$RG" --query "[?starts_with(name,'pr-${PR_NUM}-')].name" -o tsv 2>/dev/null || echo "")
if [[ -z "$VMS" ]]; then
  echo "    no pr-${PR_NUM}-* VMs found"
else
  for vm in $VMS; do
    echo "    $vm"
    osdisk=$(az vm show -g "$RG" -n "$vm" --query "storageProfile.osDisk.name" -o tsv 2>/dev/null || echo "")
    nic_ids=$(az vm show -g "$RG" -n "$vm" --query "networkProfile.networkInterfaces[].id" -o tsv 2>/dev/null || echo "")
    pip_name="${vm}-pip"
    az vm delete -g "$RG" -n "$vm" --yes -o none
    for nic_id in $nic_ids; do
      az network nic delete --ids "$nic_id" -o none 2>/dev/null || true
    done
    [[ -n "$osdisk" ]] && az disk delete -g "$RG" -n "$osdisk" --yes -o none 2>/dev/null || true
    az network public-ip delete -g "$RG" -n "$pip_name" -o none 2>/dev/null || true
  done
fi

echo ">>> [2/3] DROP DATABASE $DB_NAME"
if [[ -n "$DATA_VM_PIP" && -n "$PG_PASS" ]]; then
  ssh $SSH_OPTS azureuser@"$DATA_VM_PIP" \
    "PGPASSWORD='$PG_PASS' psql -h localhost -U postgres -d postgres -c \"DROP DATABASE IF EXISTS $DB_NAME WITH (FORCE);\""
fi

echo ">>> [3/3] delete container $CONTAINER"
if [[ -n "$STORAGE" && -n "$STORAGE_KEY" ]]; then
  az storage container delete --account-name "$STORAGE" --account-key "$STORAGE_KEY" -n "$CONTAINER" -o none 2>/dev/null || true
fi

echo "DONE. PR-$PR_NUM stack torn down."
