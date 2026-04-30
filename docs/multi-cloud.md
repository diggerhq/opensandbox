# Multi-cloud architecture

OpenSandbox is built so a cell can run on any IaaS that gives us VMs +
S3-compatible blob storage. Today we run cells on Azure (production +
dev1 + dev2) and have a complete AWS implementation ready for a third
cell. This doc explains the abstraction boundary so adding a new cloud
is a checklist, not a rewrite.

## What's cloud-specific

Three things, and only three:

1. **`internal/compute/{cloud}.go`** — implements `compute.Pool`. Knows how
   to launch / destroy / drain / list VMs in that cloud. Builds the
   cloud-init / user-data script using `compute.BuildWorkerEnv(spec)` plus
   provider-specific bring-up (NVMe RAID, AMI metadata fetches, etc.).

2. **`deploy/{cloud}/`** — provisioning scripts for VPC/VNet, control plane VM,
   first worker VM, S3-compatible bucket, IAM/managed identity. One
   `create-{cloud}-{cell}.sh` per cell.

3. **`deploy/packer/worker-ami-{cloud}.pkr.hcl`** — Packer template for the
   worker VM image. Same Go binaries, different baking.

Optional but useful per-cloud:

- **`internal/secrets/{backend}.go`** — runtime secrets backend. KeyVault for
  Azure, SecretsManager for AWS. Falls back to `EnvBackend` if unset.

## What's cloud-neutral

Everything else:

- `internal/api/`, `internal/auth/`, `internal/billing/`, `internal/controlplane/`,
  `internal/qemu/`, `internal/sandbox/`, `internal/storage/`, `internal/db/` —
  no cloud SDK imports anywhere.
- `cmd/server/main.go` selects a pool via `cfg.ComputeProvider` and hands it
  a cloud-neutral `compute.WorkerSpec`. The pool fills in cloud-specific bits.
- All Cloudflare Workers, D1 schema, R2 buckets — global, by definition.
- Postgres, Redis — standard protocols, work on any managed offering.
- Storage layer uses S3 SDK with custom endpoints — Azure Blob, AWS S3, GCS,
  R2, MinIO all work without code changes.

## Cell ID format

`{cloud}-{region}-cell-{slot}`, e.g.:
- `azure-eastus2-cell-a` (production)
- `azure-westus2-cell-b` (dev2)
- `aws-us-east-1-cell-a` (dev3, when provisioned)
- `gcp-us-central1-cell-a` (future)

Cell ID is the only routing identity. Workers, events, sandboxes, all
namespaced by `cell_id` everywhere.

## Adding a new cloud (checklist)

To stand up a GCP cell tomorrow:

1. **Implement Pool**: write `internal/compute/gcp.go` mirroring `azure.go` /
   `ec2.go`. About 400 LOC. Required methods are in the `compute.Pool`
   interface in `pool.go`. Implement `compute.WorkerSpecHolder` so the CP
   can inject the cloud-neutral `WorkerSpec`. Return a `compute.Machine`
   with internal/public addresses populated.

2. **Add config**: in `internal/config/config.go`, add a new `GCPxxx`
   field block (subscription/project equivalents) and add `"gcp"` to
   the switch in `cmd/server/main.go`.

3. **Provisioning script**: `deploy/gcp/create-gcp-{cellname}.sh`,
   mirroring `deploy/azure/create-azure-dev2.sh` and
   `deploy/aws/create-aws-dev3.sh`. Same shape: VPC + control plane VM
   + worker VM + GCS bucket + IAM service account.

4. **Packer template**: `deploy/packer/worker-ami-gcp.pkr.hcl`. Same
   provisioning as the AWS template (install QEMU, copy binaries, install
   systemd unit) but builds a GCE image instead of an AMI.

5. **Optional secrets backend**: `internal/secrets/gcpsecretmanager.go`
   if you want to use GCP Secret Manager for the bootstrap config bundle.

That's it. None of the rest of the codebase changes.

## Things that are NOT cloud-specific (despite looking like they could be)

- **Sandbox storage paths** (`/data/firecracker/images`, `/data/sandboxes`):
  established by the AMI / image, identical across clouds. Avoid forking
  these per cloud — the cost is debugging confusion later.

- **Worker binary**: same `linux/amd64` build runs anywhere. We may add
  `linux/arm64` builds for ARM-based instance families (Graviton, Ampere)
  but that's an instruction-set choice, not a cloud one.

- **gRPC / HTTP APIs**: standard protocols. Cloud-specific NSG/SG rules
  open the same ports.

- **Postgres schema, Redis stream layout, S3 keyspace**: identical across
  cells regardless of cloud.

## R2 as canonical resource store

Per-cell S3-compatible buckets (Azure Blob, AWS S3, GCS) are caches.
**Canonical** golden rootfs blobs and template blobs live in Cloudflare R2,
which is the only object store available identically from every cloud.

When a worker needs a golden it doesn't have, it pulls from R2 and caches
locally. This eliminates "the canonical golden lives in westus2 Azure Blob"
as a hidden assumption and makes new cells truly portable.

See the planning doc and the `golden_versions` D1 table for details.
