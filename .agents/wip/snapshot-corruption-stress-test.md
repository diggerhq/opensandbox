# Snapshot Corruption Stress Test — Design

**Goal:** Prove (or disprove) that snapshot corruption is fixed by running 20 sandboxes
through the most corruption-prone lifecycle possible, against production.
Scale to 1000 once 20 passes clean.

**Target:** Production cluster (Azure East US 2)
**Client:** TypeScript SDK
**Concurrency limit:** 5 simultaneous sandboxes (API limit)

---

## Context

Four corruption vectors were identified and fixed on March 26, 2026 (see incident report).
All fixes were validated with sequential stress tests (48 iterations, N=1).

This test operates at N=20 concurrent with the explicit goal of:
1. Exercising all 4 known vectors under real concurrency pressure
2. Exposing potential new vectors that only manifest under load
3. Producing a customer-facing artifact: "20/20 sandboxes, 0 corruption"

## Test Design

### The Lifecycle Gauntlet

Every sandbox runs an identical lifecycle. The lifecycle is designed so that the most
corruption-prone operations (hibernate, wake, checkpoint, fork, destroy) overlap
across sandboxes sharing the same workers.

```
Per-sandbox lifecycle (8 steps):
  1. Create from shared snapshot       ← concurrent fork from same checkpoint cache
  2. Write unique 5MB marker + SHA256  ← establishes ground truth
  3. Checkpoint                        ← savevm while other sandboxes do I/O
  4. Write second marker + SHA256      ← post-checkpoint state
  5. Hibernate                         ← triggers async archive + S3 upload
  6. Wake (no delay)                   ← races with archive (Vector 1)
  7. Verify both markers (SHA256)      ← corruption detection point
  8. Destroy                           ← races with any in-flight archive (Vector 2)
```

### Wave Structure

20 sandboxes, 5 concurrent (API limit), 4 waves:

```
Wave 1: sandbox 01-05   [create ──── checkpoint ──── hibernate ──── wake ── verify ── destroy]
Wave 2: sandbox 06-10         [create ──── checkpoint ──── hibernate ──── wake ── verify ── destroy]
Wave 3: sandbox 11-15               [create ──── checkpoint ──── hibernate ──── wake ── verify ── destroy]
Wave 4: sandbox 16-20                     [create ──── checkpoint ──── hibernate ──── wake ── verify ── destroy]
```

Waves are NOT synchronized — each sandbox proceeds at its own pace within the
concurrency pool. This means:
- Wave 1 archives upload while Wave 2 creates/forks
- Wave 2 hibernates while Wave 1 destroys + Wave 3 creates
- Worker disk has archive-staging dirs from multiple sandboxes simultaneously

### Concurrency Pool

Use a semaphore (e.g. p-limit) to enforce max 5 in-flight sandboxes.
All 20 lifecycle promises are launched at once; the pool serializes creation
but allows all other lifecycle steps to overlap freely.

### Variant: Fork Blast

After the main gauntlet, a second phase stress-tests the fork path specifically:

```
Fork blast (uses checkpoint from a surviving sandbox):
  1. Pick one sandbox's checkpoint (don't destroy it yet)
  2. Fork 5 sandboxes from it simultaneously
  3. Verify each fork has correct markers
  4. Delete the checkpoint while forks are still verifying (Vector 4)
  5. Confirm forks are unaffected
```

## What This Stresses

| Overlap pattern | Known vector | New vector? |
|---|---|---|
| Archive upload from wave N + create/fork from wave N+1 | #1 (hibernate archive race) | S3 bandwidth contention |
| Destroy from wave N + archive still running from wave N | #2 (destroy during archive) | — |
| 5 concurrent forks from same snapshot | #4 (checkpoint cache) | Cache lock contention under load |
| 5 concurrent checkpoints hitting same worker | #1 indirectly | `opMu` contention, `savevm` queueing |
| Rapid hibernate→wake with no delay, 5 at a time | #1 | Wake polls archive-staging 30s — enough? |
| 20 sandboxes' archive-staging dirs on same worker | — | **Disk exhaustion during reflink divergence** |
| 20 concurrent S3 uploads | — | **Upload timeout cascade → archiveDone delay → Vector 2 revival** |
| Checkpoint "ready" polling under load | — | **Polling timeout → fork from not-ready checkpoint** |

## Potential New Vectors (Hypotheses)

### H1: Disk pressure from concurrent reflink staging
Each sandbox has ~2 qcow2 files. Reflink copies share blocks until divergence.
When wake modifies originals (loadvm), blocks diverge and consume real disk.
5 concurrent archive-staging dirs all diverging = sudden disk spike.
If worker hits ENOSPC mid-tar, archive fails silently or produces truncated output.

**Detection:** Monitor worker disk usage during test. Check for tar errors in worker logs.

### H2: S3 upload timeout cascade
Archive upload has 5-min timeout. Under load, S3 latency rises.
If timeout fires, `archiveDone` channel closes (via defer), but the incomplete
upload is left in S3. A cross-worker wake could download this partial archive.

**Detection:** After wake, verify file content — a partial archive would produce
missing/truncated files even if the sandbox boots.

### H3: Checkpoint ready polling exhaustion
API polls 30s for checkpoint status "ready". Under 5 concurrent checkpoints,
`savevm` operations queue behind per-VM `opMu`. Total queue time could exceed 30s.
API returns checkpoint in "processing" state. SDK may try to fork from it.

**Detection:** Log checkpoint status at fork time. Count "processing" vs "ready".

### H4: Wake proceeds despite archive-staging present
Wake polls for archive-staging/ removal for 30s max, then proceeds anyway.
Under load, archives could take >30s (S3 slow, tar slow on large files).
Wake starts QEMU while staging dir still exists — no corruption (archive reads
from staging), but disk waste accumulates. With enough sandboxes, this could
cascade into H1.

**Detection:** Check if any wake proceeded with archive-staging present (worker logs).

## Data Model

```typescript
interface SandboxResult {
  sandboxId: string;
  wave: number;
  lifecycle: {
    createMs: number;
    writeMs: number;
    checkpointMs: number;
    hibernateMs: number;
    wakeMs: number;
    verifyMs: number;
    destroyMs: number;
  };
  markers: {
    marker1: { path: string; sha256: string; verified: boolean };
    marker2: { path: string; sha256: string; verified: boolean };
  };
  checkpointId: string;
  error?: string;           // first error encountered
  failedAt?: string;        // lifecycle stage where failure occurred
  corrupted: boolean;       // markers didn't match
}

interface TestReport {
  startedAt: string;
  completedAt: string;
  totalSandboxes: number;
  concurrencyLimit: number;
  results: SandboxResult[];
  summary: {
    created: number;
    completed: number;
    corrupted: number;
    errored: number;         // infra errors (create timeout, API 500, etc.)
    totalDurationMs: number;
  };
  forkBlast?: {
    sourceCheckpointId: string;
    forks: number;
    verified: number;
    corrupted: number;
  };
}
```

## Verification

**Per-sandbox verification (step 7):**
1. Read marker1, compute SHA256, compare to recorded hash
2. Read marker2, compute SHA256, compare to recorded hash
3. Run `ls -la /workspace/` to confirm no unexpected files/state

**Fork blast verification:**
1. Each fork reads marker1 and marker2 from the source checkpoint
2. SHA256 comparison — must match the values recorded at checkpoint time
3. marker2 should NOT be present (it was written after checkpoint)

**Aggregate pass criteria:**
- `corrupted == 0` across all 20 sandboxes
- `corrupted == 0` across all fork blast sandboxes
- `errored` count acceptable if <10% and errors are transient (timeouts, not data loss)

## Implementation Plan

**Language:** TypeScript (matches existing SDK examples and test patterns)
**Location:** `sdks/typescript/examples/stress-snapshot-corruption.ts`
**Dependencies:** opencomputer SDK, crypto (SHA256), p-limit (concurrency)

```
File structure:
  stress-snapshot-corruption.ts
    ├── main()                    — orchestrator, prints report
    ├── runLifecycle(id, sem)     — single sandbox lifecycle, returns SandboxResult
    ├── runForkBlast(cpId, sem)   — fork blast phase
    ├── writeMarker(sb, name)     — write 5MB random data, return sha256
    ├── verifyMarker(sb, path, expectedSha256) — read + compare
    ├── waitCheckpointReady(sb, cpId) — poll with backoff
    └── report(results)           — print summary table + JSON
```

**Config via CLI args (using commander or yargs):**
```
npx tsx stress-snapshot-corruption.ts \
  --count 20 \
  --concurrency 5 \
  --marker-size 5 \
  --snapshot base \
  --api-key sk-... \
  --api-url https://api.opencomputer.com \
  --fork-blast \
  --output report.json
```

| Flag | Default | Description |
|---|---|---|
| `--count, -n` | 20 | Number of sandboxes to run |
| `--concurrency, -c` | 5 | Max simultaneous sandboxes |
| `--marker-size` | 5 | Marker file size in MB |
| `--snapshot` | — | Pre-built snapshot name (skip image build) |
| `--api-key` | `$OC_API_KEY` | API key (falls back to env var) |
| `--api-url` | `$OC_API_URL` | API URL (falls back to env var) |
| `--fork-blast` | false | Run fork blast phase after gauntlet |
| `--output, -o` | — | Write JSON report to file |

## Scaling to 1000

Once 20 passes clean:
1. Request concurrency limit increase (or run from multiple API keys)
2. Set `SANDBOX_COUNT=1000`, `CONCURRENCY=50`
3. Add worker-level monitoring (disk, S3 upload queue, goroutine count)
4. Run for ~1 hour, collect full report
5. Customer deliverable: "1000 sandboxes, 0 corruption, full report attached"

## Open Questions

1. **Pre-built snapshot?** Creating 20 sandboxes from scratch is slow. A shared snapshot
   (pre-built with the base image) would speed up creation and also stress the fork path harder.
   → Recommendation: yes, create a snapshot first, then fork all 20 from it.

2. **Cross-worker wake?** The test naturally produces same-worker hibernate/wake (fastest path).
   To test the S3 download path, we'd need to force cross-worker migration. This requires
   either worker affinity controls or just running enough sandboxes to span multiple workers.
   → At N=20, likely spans 2-3 workers naturally. At N=1000, guaranteed.

3. **Worker log access?** Some hypotheses (H1, H4) require worker-side observability.
   Can we tail worker logs during the test?
   → Nice-to-have. The SHA256 verification catches corruption regardless of cause.
