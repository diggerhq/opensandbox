#!/usr/bin/env bash
# Bring up the persistent layer for the opencomputer-ci test stack.
# Idempotent: safe to re-run. Existing resources are skipped or updated in place.
#
# Creates: VNet+subnets+NSGs, two managed identities, Key Vault, storage account
# (with checkpoints/ and compat-corpus/ containers), and a single B4ms VM running
# Postgres 16 + Redis 7 via systemd. Generates secrets and stores them in KV.
#
# Run once. To tear down: ./down.sh.

set -euo pipefail

RG="${RG:-opencomputer-ci}"
LOCATION="${LOCATION:-centralus}"
VNET="oc-ci-vnet"
KV="${KV:-opencomputer-ci-kv}"
STORAGE="${STORAGE:-occiblob$(openssl rand -hex 2)}"  # globally unique
DATA_VM="oc-ci-data"
DATA_IDENTITY="osb-ci-data-identity"
WORKER_IDENTITY="osb-ci-worker-identity"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/opencomputer-ci.pub}"

[[ -f "$SSH_KEY" ]] || { echo "FATAL: SSH key $SSH_KEY not found"; exit 1; }

ME=$(az ad signed-in-user show --query id -o tsv)
SUB=$(az account show --query id -o tsv)
echo ">>> subscription=$SUB me=$ME rg=$RG region=$LOCATION"

echo ">>> [1/9] resource group"
az group create -n "$RG" -l "$LOCATION" --tags purpose=ci -o none

echo ">>> [2/9] vnet + subnets + nsgs"
az network vnet create -g "$RG" -n "$VNET" \
  --address-prefix 10.210.0.0/16 \
  --subnet-name data --subnet-prefix 10.210.1.0/24 \
  -o none
az network vnet subnet show -g "$RG" --vnet-name "$VNET" -n compute -o none 2>/dev/null \
  || az network vnet subnet create -g "$RG" --vnet-name "$VNET" \
       -n compute --address-prefix 10.210.2.0/24 -o none

az network nsg create -g "$RG" -n oc-ci-nsg-data -o none
az network nsg rule create -g "$RG" --nsg-name oc-ci-nsg-data -n allow-pg-compute \
  --priority 100 --source-address-prefixes 10.210.2.0/24 --destination-port-ranges 5432 \
  --protocol Tcp --access Allow --direction Inbound -o none 2>/dev/null || true
az network nsg rule create -g "$RG" --nsg-name oc-ci-nsg-data -n allow-redis-compute \
  --priority 110 --source-address-prefixes 10.210.2.0/24 --destination-port-ranges 6379 \
  --protocol Tcp --access Allow --direction Inbound -o none 2>/dev/null || true
az network nsg rule create -g "$RG" --nsg-name oc-ci-nsg-data -n allow-ssh \
  --priority 200 --destination-port-ranges 22 \
  --protocol Tcp --access Allow --direction Inbound -o none 2>/dev/null || true
az network vnet subnet update -g "$RG" --vnet-name "$VNET" -n data \
  --network-security-group oc-ci-nsg-data -o none

az network nsg create -g "$RG" -n oc-ci-nsg-compute -o none
az network nsg rule create -g "$RG" --nsg-name oc-ci-nsg-compute -n allow-ssh \
  --priority 100 --destination-port-ranges 22 \
  --protocol Tcp --access Allow --direction Inbound -o none 2>/dev/null || true
az network nsg rule create -g "$RG" --nsg-name oc-ci-nsg-compute -n allow-http \
  --priority 110 --destination-port-ranges 8080 8081 9090 \
  --protocol Tcp --access Allow --direction Inbound -o none 2>/dev/null || true
az network vnet subnet update -g "$RG" --vnet-name "$VNET" -n compute \
  --network-security-group oc-ci-nsg-compute -o none

echo ">>> [3/9] managed identities"
az identity create -g "$RG" -n "$DATA_IDENTITY" -o none
az identity create -g "$RG" -n "$WORKER_IDENTITY" -o none
DATA_PRINCIPAL=$(az identity show -g "$RG" -n "$DATA_IDENTITY" --query principalId -o tsv)
WORKER_PRINCIPAL=$(az identity show -g "$RG" -n "$WORKER_IDENTITY" --query principalId -o tsv)
DATA_IDENTITY_ID=$(az identity show -g "$RG" -n "$DATA_IDENTITY" --query id -o tsv)
WORKER_IDENTITY_ID=$(az identity show -g "$RG" -n "$WORKER_IDENTITY" --query id -o tsv)

echo ">>> [4/9] key vault (RBAC mode)"
az keyvault show -n "$KV" -o none 2>/dev/null \
  || az keyvault create -g "$RG" -n "$KV" --enable-rbac-authorization true -o none
KV_SCOPE="/subscriptions/$SUB/resourceGroups/$RG/providers/Microsoft.KeyVault/vaults/$KV"

az role assignment create --assignee-object-id "$ME" --assignee-principal-type User \
  --role "Key Vault Administrator" --scope "$KV_SCOPE" -o none 2>/dev/null || true
az role assignment create --assignee-object-id "$DATA_PRINCIPAL" --assignee-principal-type ServicePrincipal \
  --role "Key Vault Secrets User" --scope "$KV_SCOPE" -o none 2>/dev/null || true
az role assignment create --assignee-object-id "$WORKER_PRINCIPAL" --assignee-principal-type ServicePrincipal \
  --role "Key Vault Secrets User" --scope "$KV_SCOPE" -o none 2>/dev/null || true

echo ">>> waiting 45s for RBAC propagation..."
sleep 45

echo ">>> [5/9] generating + storing secrets"
gen_or_keep() {
  local name="$1" gen_cmd="$2"
  if az keyvault secret show --vault-name "$KV" --name "$name" -o none 2>/dev/null; then
    echo "    $name (kept)"
  else
    local val
    val=$(eval "$gen_cmd")
    az keyvault secret set --vault-name "$KV" --name "$name" --value "$val" -o none
    echo "    $name (generated)"
  fi
}
gen_or_keep "pg-password"                 "openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | head -c 32"
gen_or_keep "redis-password"              "openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | head -c 32"
gen_or_keep "server-api-key"              "echo osb_ci_\$(openssl rand -hex 32)"
gen_or_keep "server-jwt-secret"           "openssl rand -hex 32"
gen_or_keep "worker-jwt-secret"           "az keyvault secret show --vault-name $KV --name server-jwt-secret --query value -o tsv"
gen_or_keep "server-secret-encryption-key" "openssl rand -hex 32"

PG_PASS=$(az keyvault secret show --vault-name "$KV" --name pg-password --query value -o tsv)
REDIS_PASS=$(az keyvault secret show --vault-name "$KV" --name redis-password --query value -o tsv)

echo ">>> [6/9] storage account + containers"
if ! az storage account show -g "$RG" -n "$STORAGE" -o none 2>/dev/null; then
  EXISTING=$(az keyvault secret show --vault-name "$KV" --name storage-account-name --query value -o tsv 2>/dev/null || echo "")
  if [[ -n "$EXISTING" ]]; then STORAGE="$EXISTING"; fi
fi
if ! az storage account show -g "$RG" -n "$STORAGE" -o none 2>/dev/null; then
  az storage account create -g "$RG" -n "$STORAGE" -l "$LOCATION" \
    --sku Standard_LRS --kind StorageV2 -o none
  az keyvault secret set --vault-name "$KV" --name storage-account-name --value "$STORAGE" -o none
fi
STORAGE_KEY=$(az storage account keys list -g "$RG" -n "$STORAGE" --query "[0].value" -o tsv)
az storage container create --account-name "$STORAGE" --account-key "$STORAGE_KEY" -n checkpoints -o none 2>/dev/null || true
az storage container create --account-name "$STORAGE" --account-key "$STORAGE_KEY" -n compat-corpus -o none 2>/dev/null || true
az keyvault secret set --vault-name "$KV" --name worker-s3-access-key --value "$STORAGE" -o none
az keyvault secret set --vault-name "$KV" --name worker-s3-secret-key --value "$STORAGE_KEY" -o none

echo ">>> [7/9] data VM cloud-init"
CLOUD_INIT=$(mktemp)
trap "rm -f $CLOUD_INIT" EXIT
cat > "$CLOUD_INIT" <<EOF
#cloud-config
package_update: true
packages:
  - postgresql-16
  - postgresql-contrib-16
  - redis-server
runcmd:
  - bash -c "echo \"listen_addresses = '*'\" >> /etc/postgresql/16/main/postgresql.conf"
  - bash -c "echo 'host all all 10.210.0.0/16 md5' >> /etc/postgresql/16/main/pg_hba.conf"
  - sudo -u postgres psql -c "ALTER USER postgres WITH PASSWORD '${PG_PASS}';"
  - sudo -u postgres psql -c "CREATE USER osbciuser WITH PASSWORD '${PG_PASS}' CREATEDB;"
  - sudo -u postgres psql -c "GRANT pg_read_all_data, pg_write_all_data TO osbciuser;"
  - sed -i 's/^bind 127.0.0.1.*/bind 0.0.0.0/' /etc/redis/redis.conf
  - sed -i 's|^# requirepass.*|requirepass ${REDIS_PASS}|' /etc/redis/redis.conf
  - sed -i 's/^protected-mode yes/protected-mode no/' /etc/redis/redis.conf
  - systemctl restart postgresql
  - systemctl restart redis-server
  - systemctl enable postgresql redis-server
EOF

echo ">>> [8/9] data VM"
if ! az vm show -g "$RG" -n "$DATA_VM" -o none 2>/dev/null; then
  az vm create -g "$RG" -n "$DATA_VM" \
    --image Ubuntu2404 \
    --size Standard_B4ms \
    --vnet-name "$VNET" --subnet data \
    --public-ip-address "${DATA_VM}-pip" \
    --public-ip-sku Standard \
    --admin-username azureuser \
    --ssh-key-values "$SSH_KEY" \
    --assign-identity "$DATA_IDENTITY_ID" \
    --custom-data "$CLOUD_INIT" \
    --os-disk-size-gb 64 \
    -o none
else
  echo "    $DATA_VM already exists (cloud-init not re-run; rebuild with down.sh + up.sh)"
fi

DATA_VM_PIP=$(az vm show -d -g "$RG" -n "$DATA_VM" --query publicIps -o tsv)
DATA_VM_PRIVATE=$(az vm show -d -g "$RG" -n "$DATA_VM" --query privateIps -o tsv)

echo ">>> [9/9] connection strings into KV"
az keyvault secret set --vault-name "$KV" --name data-vm-private-ip --value "$DATA_VM_PRIVATE" -o none
az keyvault secret set --vault-name "$KV" --name server-database-url \
  --value "postgres://osbciuser:${PG_PASS}@${DATA_VM_PRIVATE}:5432/postgres?sslmode=disable" -o none
az keyvault secret set --vault-name "$KV" --name worker-database-url \
  --value "postgres://osbciuser:${PG_PASS}@${DATA_VM_PRIVATE}:5432/postgres?sslmode=disable" -o none
az keyvault secret set --vault-name "$KV" --name server-redis-url \
  --value "redis://default:${REDIS_PASS}@${DATA_VM_PRIVATE}:6379/0" -o none
az keyvault secret set --vault-name "$KV" --name worker-redis-url \
  --value "redis://default:${REDIS_PASS}@${DATA_VM_PRIVATE}:6379/0" -o none

cat <<DONE

================================================================
DONE. opencomputer-ci persistent layer is up.

  resource group:   $RG
  region:           $LOCATION
  data VM:          $DATA_VM   public=$DATA_VM_PIP   private=$DATA_VM_PRIVATE
  key vault:        $KV
  storage:          $STORAGE
  data identity:    $DATA_IDENTITY_ID
  worker identity:  $WORKER_IDENTITY_ID

  postgres:         postgres://osbciuser:***@${DATA_VM_PRIVATE}:5432/postgres
  redis:            redis://default:***@${DATA_VM_PRIVATE}:6379/0

  smoke test:       ./verify.sh
  ssh:              ssh azureuser@${DATA_VM_PIP}
  cloud-init log:   /var/log/cloud-init-output.log on the VM

  IMPORTANT: cloud-init takes ~3-4 min after VM create to finish installing
  Postgres + Redis. Run ./verify.sh after a few minutes.
================================================================
DONE
