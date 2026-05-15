#!/usr/bin/env bash
# rclone-azure-to-tigris.sh
#
# Copies worker-managed object storage from Azure Blob to Tigris.
# Idempotent; safe to re-run for delta syncs.
#
# Required env vars:
#   AZURE_STORAGE_ACCOUNT       e.g. occkpt3ccf3c31
#   AZURE_STORAGE_KEY           az storage account keys list -g <rg> -n <account> --query "[0].value" -o tsv
#   TIGRIS_ACCESS_KEY_ID
#   TIGRIS_SECRET_ACCESS_KEY
#
# Optional:
#   SRC_CONTAINER               default: checkpoints
#   TIGRIS_BUCKET               default: opencomputer-prod
#   TIGRIS_ENDPOINT             default: https://t3.storage.dev
#
# ---------------------------------------------------------------------------
# Why two passes (don't simplify this without understanding):
#
#   The legacy Azure backend stripped a leading "<container>/" from keys on
#   write (internal/storage/blob_azure.go's normalizeKey). So a key like
#   "checkpoints/sb-xxx/123.tar.zst" passed by the worker was stored at Azure
#   blob name "sb-xxx/123.tar.zst" (no checkpoints/ prefix).
#
#   The new S3-compat backend (internal/blobstore/s3.go) does NOT strip.
#   Workers calling Get(bucket="opencomputer-prod", key="checkpoints/sb-xxx/...")
#   expect Tigris to have it at exactly that key.
#
#   So sandbox archives in Azure ("sb-XXXX/...") must be re-prefixed with
#   "checkpoints/" when copying to Tigris. Other prefixes (bases/, migrations/,
#   templates/) were never stripped — their keys never started with
#   "checkpoints/" — so they stay at their current paths.
#
#   Skipped on purpose: deploy/, db-backups/, rootfs-cache/ — no worker
#   code reads these (build artifacts / out-of-band backups).
# ---------------------------------------------------------------------------
#
# Required rclone flags:
#   --s3-storage-class STANDARD   Tigris rejects Azure's "Hot"/"Cool" tier
#                                 names — without this every PUT fails with
#                                 400 InvalidStorageClass.
#   --metadata=false              Skip Azure-specific metadata that doesn't
#                                 translate and isn't needed by our code.

set -euo pipefail

: "${AZURE_STORAGE_ACCOUNT:?required}"
: "${AZURE_STORAGE_KEY:?required}"
: "${TIGRIS_ACCESS_KEY_ID:?required}"
: "${TIGRIS_SECRET_ACCESS_KEY:?required}"

SRC_CONTAINER="${SRC_CONTAINER:-checkpoints}"
TIGRIS_BUCKET="${TIGRIS_BUCKET:-opencomputer-prod}"
TIGRIS_ENDPOINT="${TIGRIS_ENDPOINT:-https://t3.storage.dev}"

export RCLONE_CONFIG_AZ_TYPE=azureblob
export RCLONE_CONFIG_AZ_ACCOUNT="$AZURE_STORAGE_ACCOUNT"
export RCLONE_CONFIG_AZ_KEY="$AZURE_STORAGE_KEY"

export RCLONE_CONFIG_TIGRIS_TYPE=s3
export RCLONE_CONFIG_TIGRIS_PROVIDER=Other
export RCLONE_CONFIG_TIGRIS_ENDPOINT="$TIGRIS_ENDPOINT"
export RCLONE_CONFIG_TIGRIS_REGION=auto
export RCLONE_CONFIG_TIGRIS_ACCESS_KEY_ID="$TIGRIS_ACCESS_KEY_ID"
export RCLONE_CONFIG_TIGRIS_SECRET_ACCESS_KEY="$TIGRIS_SECRET_ACCESS_KEY"
export RCLONE_CONFIG_TIGRIS_FORCE_PATH_STYLE=true

TIMESTAMP="$(date +%Y%m%d-%H%M%S)"
LOG_SB="${LOG_SB:-rclone-az-to-tigris-sb-${TIMESTAMP}.log}"
LOG_REST="${LOG_REST:-rclone-az-to-tigris-rest-${TIMESTAMP}.log}"

# Shared flags. --s3-storage-class STANDARD + --metadata=false are required
# for Tigris compatibility; the rest is throughput tuning.
#
# Memory sizing: rclone holds (transfers × multi-thread-streams ×
# upload-concurrency × chunk-size) bytes in flight as a worst case. The
# settings below cap at ~4 GB resident on the bastion, fitting D4s_v3
# (16 GB) comfortably. An earlier tuning with transfers=16, streams=8,
# concurrency=4, chunk=64M OOM-killed rclone on Pass 2's 4 GB golden files.
# If you upsize the bastion to D8s_v3 (32 GB) or larger you can crank these
# back up.
COMMON_FLAGS=(
  --s3-storage-class STANDARD
  --metadata=false
  --transfers 8
  --checkers 32
  --multi-thread-streams 4
  --multi-thread-cutoff 512M
  --s3-chunk-size 32M
  --s3-upload-concurrency 2
  --progress
  --stats 30s
  --stats-one-line
  --log-level INFO
)

echo "==> $(date)  Pass 1: sb-* sandbox archives → tigris:${TIGRIS_BUCKET}/checkpoints/"
echo "    log: $LOG_SB"
echo "    extra args: $*"

# Pass 1: relocate sb-* archives under the "checkpoints/" prefix so the
# new worker code's HibernationKey() ("checkpoints/sb-xxx/...") finds them.
rclone copy \
  "az:$SRC_CONTAINER" \
  "tigris:$TIGRIS_BUCKET/checkpoints" \
  --include "sb-*/**" \
  --log-file "$LOG_SB" \
  "${COMMON_FLAGS[@]}" \
  "$@"

echo "==> $(date)  Pass 2: bases/, migrations/, templates/ → tigris:${TIGRIS_BUCKET}/ (unchanged paths)"
echo "    log: $LOG_REST"

# Pass 2: copy other worker-managed prefixes verbatim. These already match
# what the new code expects (no prefix transform needed).
rclone copy \
  "az:$SRC_CONTAINER" \
  "tigris:$TIGRIS_BUCKET" \
  --include "bases/**" \
  --include "migrations/**" \
  --include "templates/**" \
  --log-file "$LOG_REST" \
  "${COMMON_FLAGS[@]}" \
  "$@"

echo "==> $(date)  done. logs: $LOG_SB, $LOG_REST"
echo ""
echo "Parity check:"
echo "  rclone size az:$SRC_CONTAINER --include 'sb-*/**'"
echo "  rclone size tigris:$TIGRIS_BUCKET/checkpoints"
echo ""
echo "  rclone size az:$SRC_CONTAINER --include 'bases/**' --include 'migrations/**' --include 'templates/**'"
echo "  rclone size tigris:$TIGRIS_BUCKET --include 'bases/**' --include 'migrations/**' --include 'templates/**'"
