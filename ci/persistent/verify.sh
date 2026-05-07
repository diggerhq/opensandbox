#!/usr/bin/env bash
# Smoke-test the persistent layer: SSH to data VM, prove Postgres + Redis are up
# and listening on the data subnet, prove KV is reachable from the worker identity.

set -euo pipefail

RG="${RG:-opencomputer-ci}"
KV="${KV:-opencomputer-ci-kv}"
DATA_VM="oc-ci-data"

DATA_VM_PIP=$(az vm show -d -g "$RG" -n "$DATA_VM" --query publicIps -o tsv)
DATA_VM_PRIVATE=$(az vm show -d -g "$RG" -n "$DATA_VM" --query privateIps -o tsv)

echo ">>> data VM:  public=$DATA_VM_PIP  private=$DATA_VM_PRIVATE"

PG_PASS=$(az keyvault secret show --vault-name "$KV" --name pg-password --query value -o tsv)
REDIS_PASS=$(az keyvault secret show --vault-name "$KV" --name redis-password --query value -o tsv)

echo ">>> SSH check"
ssh -i ~/.ssh/opencomputer-ci -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 azureuser@"$DATA_VM_PIP" "uptime" || {
  echo "SSH failed — VM may still be cloud-init'ing. Wait 1-2 min and re-run."
  exit 1
}

echo ">>> postgres check (psql via SSH tunnel)"
ssh -i ~/.ssh/opencomputer-ci -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null azureuser@"$DATA_VM_PIP" "PGPASSWORD='$PG_PASS' psql -h localhost -U osbciuser -d postgres -c 'SELECT version();'"

echo ">>> redis check"
ssh -i ~/.ssh/opencomputer-ci -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null azureuser@"$DATA_VM_PIP" "redis-cli -a '$REDIS_PASS' ping"

echo ">>> postgres listening on 0.0.0.0:5432?"
ssh -i ~/.ssh/opencomputer-ci -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null azureuser@"$DATA_VM_PIP" "ss -tln | grep ':5432' || echo 'NOT LISTENING'"

echo ">>> redis listening on 0.0.0.0:6379?"
ssh -i ~/.ssh/opencomputer-ci -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null azureuser@"$DATA_VM_PIP" "ss -tln | grep ':6379' || echo 'NOT LISTENING'"

echo
echo "all checks passed."
