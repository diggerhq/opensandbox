# Tigris cutover runbook

Zero-downtime migration of all worker object storage (checkpoints,
hibernation archives, templates, goldens) from Azure Blob to Tigris using
`internal/blobstore.FallbackStore` in migration mode as the safety net during
the soak window.

> **Why not Tigris shadow buckets?** Tigris shadow buckets require an
> S3-compatible source endpoint. Azure Blob has no native S3 API, so shadow
> buckets don't apply without a separate S3↔Azure gateway. `FallbackStore`
> accomplishes the same goal (transparent fallback on cold reads) entirely
> in-process — no extra infrastructure.

## What this covers

| Data | Old backend | Cutover mechanism |
|---|---|---|
| Checkpoints / hibernation archives / templates | Azure Blob | rclone bulk copy + FallbackStore in migration mode |
| Goldens — existing (`bases/<old-versions>/default.ext4`) | Azure Blob | rclone bulk copy (they live in the same container as checkpoints) |
| Goldens — new ones produced by future AMI bakes | Azure Blob | **AMI bake dual-writes to both Azure and Tigris** until Phase 5 |
| pg-backups / pg-wal | Azure Blob | Out of scope (managed by separate path) |

All worker-managed paths flow through `internal/blobstore.Store`, so a
single set of env-var changes switches them all.

## How worker.env is sourced

Worker `/etc/opensandbox/worker.env` is **not** baked into the AMI. The
control plane generates it per-VM at spawn time from its own `cfg`
(loaded from `/etc/opensandbox/server.env` on the CP):

```
server.env on CP  →  CP cfg  →  workerEnv template  →  cloud-init userdata  →  worker.env on each VM
```

Practical consequence: **for env-var-only changes, you edit `server.env`
on the CP and cycle workers. No AMI rebake needed.** AMI rebakes are only
required for binary or system-level changes.

> The `workerEnv` template lives in `cmd/server/main.go`. If you add new
> env vars that workers need, the template must be updated to propagate
> them. This PR added `OPENSANDBOX_S3_FALLBACK_*` and
> `OPENSANDBOX_BLOB_MIGRATION_MODE` to the template.

## Pre-flight inventory (prod, opencomputer-prod RG, eastus2)

- Storage account: `occkpt3ccf3c31`
- `checkpoints` container: ~1,854 blobs, ~2.08 TB (includes goldens in
  `bases/<hash>/default.ext4`)
- `pg-backups` / `pg-wal`: managed by postgres infra, not part of this cutover

## Tigris target

- Bucket: `opencomputer-prod` (Region Earth or `iad`, snapshots enabled)
- Layout: identical to Azure — rclone copies keys verbatim; no key
  rewriting required
- Per-backend bucket override is available: primary uses Tigris bucket
  `opencomputer-prod`, fallback uses Azure container `checkpoints`,
  configured via `OPENSANDBOX_S3_FALLBACK_BUCKET`

## Phase 1 — Ship the new binary + start dual-writing goldens

**Goal**: get the new code into the worker pool AND start uploading each
new AMI's golden to Tigris in parallel with Azure. No worker behavior
change yet (`OPENSANDBOX_S3_FALLBACK_*` unset means workers log "no
fallback" and behave identically to before).

1. Before merging: stash Tigris creds in GitHub Actions secrets used by
   `build-worker-ami.yml`:
   - `TIGRIS_ENDPOINT` = `https://t3.storage.dev`
   - `TIGRIS_ACCESS_KEY_ID` = `tid_...`
   - `TIGRIS_SECRET_ACCESS_KEY` = `tsec_...`
   - `TIGRIS_GOLDENS_BUCKET` = `opencomputer-prod`
2. Merge the PR.
3. CI builds a new worker AMI. The bake now performs **two** golden
   uploads:
   - Existing Python step → Azure container `checkpoints` (unchanged)
   - New step → Tigris bucket `opencomputer-prod` via
     `opensandbox-worker golden-upload` (skips silently if Tigris secrets
     aren't set, so this is safe on dev/PR branches that don't have them)
4. CI bumps KV `worker-image-version`. Scaler does rolling-replace.
5. Verify all workers report the new version via `GET /api/workers`.
6. Smoke test: create / hibernate / wake. Behavior identical.
7. Verify the new golden landed in Tigris:
   ```
   tigris ls "opencomputer-prod/bases/$(<the hash from packer output>)/"
   ```
   Should show `default.ext4` at the expected size.

**Rollback**: bump KV `worker-image-version` back. Scaler rolls back. The
Tigris-uploaded golden stays in Tigris as a no-op (no reader yet).

## Phase 2 — Bulk rclone Azure → Tigris

**Goal**: get ~all checkpoint data into Tigris before flipping. Workers
still on Azure — zero production impact.

1. Run `scripts/rclone-azure-to-tigris.sh` from a bastion VM in eastus2.
   - ~2 TB, ~1-3 hours on D4-class network (depends on bastion size)
2. Required rclone flags (already in the script):
   - `--s3-storage-class STANDARD` — Tigris rejects Azure's `Hot`/`Cool`
     tier names; force STANDARD or every PUT fails with 400 InvalidStorageClass
   - `--metadata=false` — skip Azure-specific metadata to avoid edge-case
     translation issues
3. Parity check when done:
   ```
   rclone size az:checkpoints
   rclone size tigris:opencomputer-prod
   ```
   Sizes should match within a small delta (new writes during the run).

**Rollback**: nothing to undo. Abandon Tigris copy; costs nothing.

## Phase 3 — Atomic flip (server.env + CP restart + worker cycle)

**Goal**: workers start writing to Tigris and reading Tigris-then-Azure.

1. **Delta rclone sync** — run again right before the next step. Picks
   up anything written to Azure since Phase 2. Should be fast (minutes).

2. **Edit `/etc/opensandbox/server.env`** on the control plane to add the
   Tigris primary, Azure fallback, and migration-mode flag:
   ```
   # Primary: Tigris
   OPENSANDBOX_S3_ENDPOINT=https://t3.storage.dev
   OPENSANDBOX_S3_BUCKET=opencomputer-prod
   OPENSANDBOX_S3_REGION=auto
   OPENSANDBOX_S3_FORCE_PATH_STYLE=true
   OPENSANDBOX_S3_ACCESS_KEY_ID=<tigris key>
   OPENSANDBOX_S3_SECRET_ACCESS_KEY=<tigris secret>

   # Fallback: Azure
   OPENSANDBOX_S3_FALLBACK_ENDPOINT=https://occkpt3ccf3c31.blob.core.windows.net
   OPENSANDBOX_S3_FALLBACK_REGION=eastus2
   OPENSANDBOX_S3_FALLBACK_ACCESS_KEY_ID=occkpt3ccf3c31
   OPENSANDBOX_S3_FALLBACK_SECRET_ACCESS_KEY=<azure key>
   OPENSANDBOX_S3_FALLBACK_BUCKET=checkpoints

   # Lazy-migration: Tigris-miss falls through to Azure
   OPENSANDBOX_BLOB_MIGRATION_MODE=true
   ```

3. **Restart the control plane**:
   ```
   systemctl restart opensandbox-server
   ```
   Re-reads `server.env`. From now on, every new worker spawned by the
   scaler is templated with the new env via cloud-init.

4. **Cycle workers** — two options:
   - **Soft (gradual)**: do nothing. Existing workers keep their old env
     until the scaler cycles them (could be hours/days). Mixed pool during
     the window is fine — cross-worker ops still work because the
     new-config workers can fallback-read whatever the old-config workers
     wrote. The reverse direction (new writes Tigris, old needs to read)
     is avoided by the scaler's drain semantics — old workers are drained,
     not given new placements.
   - **Aggressive (forced)**: delete workers one at a time via
     `az vm delete -g opencomputer-prod -n osb-worker-<id>`. Scaler
     replaces each with a new VM that gets the new env. Whole pool
     flipped in minutes.

5. **Verify** as each worker cycles, log line:
   ```
   checkpoint store: tigris primary, azure-blob-fallback fallback (migration=true)
   ```

**Rollback**: revert `server.env`, restart CP, cycle workers. Data
already in Tigris stays there (passive replica); Azure remains
authoritative throughout the soak window.

## Phase 4 — Soak (1–2 weeks)

Monitor:
- Worker logs for `blobstore: primary tigris failed (...); trying fallback azure-blob` — expected near-zero and trending down as the warm working set lands in Tigris.
- Tigris dashboard: error rate, latency, ingress volume.
- Synthetic daily test: create / checkpoint / fork on another worker / hibernate / wake.

If fallback hit rate stays elevated, run another delta rclone sync to
warm cold keys into Tigris proactively.

## Phase 5 — Disable fallback + drop Azure golden upload (commit to Tigris)

1. Edit `server.env`: remove the `OPENSANDBOX_S3_FALLBACK_*` block and
   `OPENSANDBOX_BLOB_MIGRATION_MODE`.
2. Restart CP.
3. Cycle workers (same options as Phase 3 step 4).
4. Verify log: `checkpoint store: tigris primary (no fallback)`.
5. **Remove the Python Azure-upload step from `deploy/packer/worker-ami.pkr.hcl`.**
   At this point Tigris is the authoritative store and Azure shouldn't
   receive new writes. The Tigris-upload step stays.
6. Optionally also remove the "download previous golden from Azure" step
   and replace with a Tigris fetch (or accept the runtime fetch cost for
   forks of stale-pinned checkpoints, which should be rare).

**Rollback past this point** is harder. Re-adding the fallback re-enables
the safety net, but any keys written to Tigris during the no-fallback
window don't exist in Azure. If Azure must become authoritative again,
run a reverse rclone (Tigris → Azure). Don't enter this phase until
Phase 4 has been clean for at least a week.

## Phase 6 — Decommission Azure

After ~30 days of zero observed reads on the Azure side (verify via Azure
storage metrics):

1. Snapshot the `checkpoints` container to a different storage account
   as a one-time backup (optional).
2. `az storage container delete --account-name occkpt3ccf3c31 -n checkpoints`
3. Eventually delete the storage account.

## Operational rules

- **Never `systemctl restart opensandbox-worker` by hand in prod.** The
  scaler treats the heartbeat gap as "worker unhealthy" and replaces the
  VM from the current AMI — losing any local state. We hit this during
  dev validation. Always cycle workers by deleting them so the scaler
  replaces them cleanly.

- **Don't hold the leader lock in prod.** Holding
  `controlplane:leader = MAINTENANCE-HOLD` pauses the scaler — including
  the rolling-replace that the cutover relies on. The lock trick is for
  one-off dev hot-swaps only.

- **Live migration during the rolling window works** because the new
  workers are in migration mode. Drain direction is OLD → NEW: old
  worker writes drives to Azure, new worker reads Tigris (miss) →
  fallback to Azure (hit) → migration succeeds. The reverse direction
  (NEW writes, OLD reads) doesn't happen — old workers are being
  drained, not receiving new placements.

## Code surface

- `internal/blobstore/store.go` — `Store` interface (Get / GetRange / Put / Head / Exists / Delete / Name)
- `internal/blobstore/s3.go` — S3-compatible backend (Tigris, R2, AWS S3, MinIO); supports per-backend `Bucket` override
- `internal/blobstore/azure.go` — Azure Blob backend; same `Bucket` override pattern
- `internal/blobstore/fallback.go` — `FallbackStore` with `NewFallback` (HA mode) and `NewMigrationFallback` (lazy-migration mode)
- `internal/storage/s3.go` — `CheckpointStore` higher-level type, built on top of `blobstore.Store`
- `cmd/server/main.go` — `workerEnv` template propagates `OPENSANDBOX_S3_FALLBACK_*` and `OPENSANDBOX_BLOB_MIGRATION_MODE` to each spawned worker
- `cmd/worker/main.go` — `buildCheckpointBackend` helper; combines primary + fallback for the checkpoint path
- `internal/config/config.go` — env-var parsing for the new vars
- `scripts/rclone-azure-to-tigris.sh` — bulk + delta rclone driver (with `--s3-storage-class STANDARD --metadata=false`)
