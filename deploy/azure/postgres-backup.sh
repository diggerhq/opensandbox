#!/usr/bin/env bash
# postgres-backup.sh — Daily PostgreSQL backup with rotation
# Install as cron: 0 3 * * * /opt/opensandbox/postgres-backup.sh
set -euo pipefail

BACKUP_DIR="/data/backups"
DB_NAME="opensandbox"
DB_USER="opensandbox"
KEEP_DAYS=7

mkdir -p "$BACKUP_DIR"

TIMESTAMP=$(date +%Y%m%d_%H%M%S)
BACKUP_FILE="$BACKUP_DIR/${DB_NAME}_${TIMESTAMP}.sql.gz"

echo "$(date): Starting backup to $BACKUP_FILE"

# Dump and compress
sudo -u postgres pg_dump "$DB_NAME" | gzip > "$BACKUP_FILE"

SIZE=$(ls -lh "$BACKUP_FILE" | awk '{print $5}')
echo "$(date): Backup complete ($SIZE)"

# Rotate old backups
find "$BACKUP_DIR" -name "${DB_NAME}_*.sql.gz" -mtime +$KEEP_DAYS -delete
REMAINING=$(ls "$BACKUP_DIR"/${DB_NAME}_*.sql.gz 2>/dev/null | wc -l)
echo "$(date): Rotated old backups, $REMAINING remaining"

# Optional: upload to Azure Blob Storage (uncomment when configured)
# STORAGE_ACCOUNT="opensandboxbackups"
# CONTAINER="pg-backups"
# az storage blob upload \
#     --account-name "$STORAGE_ACCOUNT" \
#     --container-name "$CONTAINER" \
#     --file "$BACKUP_FILE" \
#     --name "$(basename $BACKUP_FILE)" \
#     --auth-mode login 2>/dev/null && echo "Uploaded to Azure Blob" || echo "Blob upload skipped"
