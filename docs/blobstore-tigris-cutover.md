# Tigris cutover runbook

Zero-downtime migration of all object storage (checkpoints, hibernation archives,
templates, goldens) from Azure Blob to Tigris using Tigris **shadow buckets** for
read-through fallback. No bulk pre-copy required; lazy migration handles cold
data over time. No code-level fallback wrapper required; Tigris does the
fallback server-side.

## What this covers

| Data | Path | Current backend | Migration strategy |
|---|---|---|---|
| Checkpoints | `checkpoints/<sandbox>/<cp>/...` | Azure Blob | Endpoint flip; shadow reads |
| Hibernation archives | Same path tree | Azure Blob | Endpoint flip; shadow reads |
| Templates | `templates/<id>/...` | Azure Blob | Endpoint flip; shadow reads |
| Goldens | `bases/<version>/default.ext4` | Azure Blob (ad-hoc) | New `internal/blobstore` abstraction + endpoint flip |
| pg-backups | `pg-backups/...` | Azure Blob | Separate decision (out of scope) |

Existing `internal/storage/` already auto-detects Azure vs S3 from the endpoint
(see `internal/storage/blob.go`); pointing at Tigris is just an env rotation.

New `internal/blobstore/` is a small S3-compatible abstraction added in this PR
specifically for the global goldens path, which wasn't previously abstracted.

## Pre-flight inventory

At time of writing:
- `checkpoints` container: ~1,834 blobs, ~2 TB
- `pg-backups` container: 6 blobs, 0.25 GB
- Estimated one-time Azure egress cost during drain: ~$180

Lazy migration via shadow bucket avoids paying egress on cold objects that are
never accessed.

## Phase 0 — Pre-flight (zero prod impact)

1. **Provision Tigris buckets** matching the Azure layout:
   ```
   tigris buckets create opencomputer-checkpoints
   tigris buckets create opencomputer-goldens
   tigris buckets create opencomputer-templates    # if separate from checkpoints
   ```
2. **Configure shadow source** on each Tigris bucket, pointing at the
   corresponding Azure container. Use `--write-through` so any writes to
   Tigris during cutover also land in Azure (safety net):
   ```
   tigris buckets set-migration opencomputer-checkpoints \
     --bucket checkpoints \
     --endpoint https://occkpt3ccf3c31.blob.core.windows.net \
     --region eastus2 \
     --access-key "$AZURE_STORAGE_ACCOUNT_NAME" \
     --secret-key "$AZURE_STORAGE_KEY" \
     --write-through
   ```
   Repeat for `opencomputer-goldens`, etc.
3. **Stash Tigris credentials in prod KV** under the existing and new env-var
   names (workers don't read them yet):
   - Existing path (checkpoints/hibernation/templates):
     - `OPENSANDBOX_S3_ENDPOINT` → `https://t3.storage.dev`
     - `OPENSANDBOX_S3_ACCESS_KEY_ID`, `OPENSANDBOX_S3_SECRET_ACCESS_KEY` → Tigris values
     - `OPENSANDBOX_S3_REGION` → `auto`
     - `OPENSANDBOX_S3_FORCE_PATH_STYLE` → `true`
     - `OPENSANDBOX_S3_BUCKET` → `opencomputer-checkpoints`
   - New path (goldens, this PR):
     - `OPENSANDBOX_GLOBAL_BLOB_NAME` → `tigris`
     - `OPENSANDBOX_GLOBAL_BLOB_ENDPOINT` → `https://t3.storage.dev`
     - `OPENSANDBOX_GLOBAL_BLOB_REGION` → `auto`
     - `OPENSANDBOX_GLOBAL_BLOB_ACCESS_KEY_ID`, `OPENSANDBOX_GLOBAL_BLOB_SECRET_ACCESS_KEY` → Tigris values
     - `OPENSANDBOX_GLOBAL_BLOB_USE_PATH_STYLE` → `true`
     - `OPENSANDBOX_GLOBAL_BLOB_GOLDENS_BUCKET` → `opencomputer-goldens`

## Phase 1 — Merge + deploy (additive, no behavior change)

1. Merge this PR.
2. Bake new worker AMI containing the `internal/blobstore` code.
3. Roll out via existing rolling-replace process.

After this phase, workers contain the blobstore code but still read/write Azure
because the new env vars and `OPENSANDBOX_S3_*` haven't been rotated yet. Zero
behavior change.

## Phase 2 — Seed goldens (optional, makes Phase 3 instant for new cells)

Use the new `golden-upload` subcommand to push the canonical `default.ext4`
into the goldens bucket (via the Tigris shadow which will also cache it):

```
# On any worker with the env vars set:
opensandbox-worker golden-upload /data/firecracker/images/default.ext4
```

This puts the bytes at `opencomputer-goldens/default.ext4` AND
`opencomputer-goldens/bases/<hash>/default.ext4`. Skip if you're fine with
the first new cell to come up paying a one-time fetch latency.

## Phase 3 — Atomic flip (no downtime)

1. Rotate prod KV: secrets we stashed in Phase 0 become active.
2. Trigger rolling-replace of workers (existing process). Each worker that
   cycles starts using Tigris for new writes; reads of cold keys
   transparently fetch from Azure via Tigris shadow (and warm Tigris in the
   process).
3. Monitor worker logs for the line
   `storage: using S3-compat backend (endpoint=https://t3.storage.dev, bucket=opencomputer-checkpoints)`
   to confirm the new endpoint is in effect.
4. Smoke-check: create a sandbox, take a checkpoint, fork it on a different
   worker. Both writes (checkpoint upload) and reads (fork download) exercise
   the Tigris path.

No downtime because:
- Existing checkpoints are still readable via Tigris's shadow fallback to Azure
- New checkpoints land in Tigris directly
- Goldens fetch from Tigris (and shadow-fall-back if cold)

## Phase 4 — Drain (background, optional)

Optionally pull all remaining Azure data into Tigris so the shadow can be
disabled cleanly:

```
tigris buckets drain opencomputer-checkpoints
tigris buckets drain opencomputer-goldens
```

This runs in the Tigris control plane; no impact on serving traffic. Can be
left running for days. Skip this if you're fine with lazy migration (only
accessed keys ever move to Tigris).

## Phase 5 — Soak

Run with Tigris primary + Azure shadow for **1-2 weeks**. Monitor:
- Tigris dashboard: cold-fetch rate (shadow reads). Should trend down over time
  as the working set warms.
- Worker logs: no `blob: object not found` errors from the checkpoint store.
- Latency: cold-fetch first-byte > Tigris-native, but should remain within
  acceptable bounds.

## Phase 6 — Cut shadow

Once drained (or after sufficient soak with lazy migration):

```
tigris buckets set-migration opencomputer-checkpoints --disable
# repeat for other buckets
```

Workers now read/write Tigris-only.

## Phase 7 — Decom Azure

After the retention window (suggest 30 days) and zero observed reads from
Azure side:

```
az storage container delete --account-name occkpt3ccf3c31 -n checkpoints
az storage account delete -g opencomputer-prod -n occkpt3ccf3c31
```

## Rollback

At any point through Phase 5, rollback is one env rotation back to Azure
endpoints + roll workers. Data is dual-written (write-through) so neither
side falls behind. Past Phase 6 (shadow disabled), rolling back means
either:
- Re-enabling shadow (Tigris keeps the writes; Azure is stale; would need a
  reverse copy to make Azure authoritative again)
- Living with Tigris

## Open question

`pg-backups` (250 MB, 6 blobs) is outside the checkpoint store code path.
Whether to migrate it is a separate operational decision — it doesn't block
the rest of the cutover.
