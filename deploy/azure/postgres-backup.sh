#!/usr/bin/env bash
# postgres-backup.sh — Daily PostgreSQL backup with Azure Blob upload
#
# Uses the VM's managed identity for Azure auth (no keys to manage).
# Set BACKUP_STORAGE_ACCOUNT in /etc/opensandbox/postgres-backup.env
# or pass as environment variable.
#
# Install:
#   1. Assign managed identity to Postgres VM
#   2. Grant "Storage Blob Data Contributor" on the storage account
#   3. Install az CLI: curl -sL https://aka.ms/InstallAzureCLIDeb | sudo bash
#   4. echo "BACKUP_STORAGE_ACCOUNT=osbdevchkpts" | sudo tee /etc/opensandbox/postgres-backup.env
#   5. Add cron: 0 3 * * * /opt/opensandbox/postgres-backup.sh >> /var/log/pg-backup.log 2>&1
set -euo pipefail

[ -f /etc/opensandbox/postgres-backup.env ] && source /etc/opensandbox/postgres-backup.env

BACKUP_DIR="/data/backups"
DB_NAME="opensandbox"
KEEP_DAYS=7
STORAGE_ACCOUNT="${BACKUP_STORAGE_ACCOUNT:-}"
CONTAINER="pg-backups"

mkdir -p "$BACKUP_DIR"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
BACKUP_FILE="$BACKUP_DIR/${DB_NAME}_${TIMESTAMP}.sql.gz"

echo "$(date): Starting backup"
sudo -u postgres pg_dump "$DB_NAME" | gzip > "$BACKUP_FILE"
echo "$(date): Local backup done ($(du -h "$BACKUP_FILE" | cut -f1))"

if [ -n "$STORAGE_ACCOUNT" ] && command -v az &>/dev/null; then
    az storage blob upload \
        --account-name "$STORAGE_ACCOUNT" \
        --container-name "$CONTAINER" \
        --file "$BACKUP_FILE" \
        --name "$(basename "$BACKUP_FILE")" \
        --auth-mode login \
        --overwrite 2>/dev/null \
        && echo "$(date): Uploaded to $STORAGE_ACCOUNT/$CONTAINER" \
        || echo "$(date): WARNING: Blob upload failed"
fi

find "$BACKUP_DIR" -name "${DB_NAME}_*.sql.gz" -mtime +$KEEP_DAYS -delete
echo "$(date): Complete"
