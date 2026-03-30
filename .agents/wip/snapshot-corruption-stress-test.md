# Snapshot Corruption Stress Test — Design (v2)

**Goal:** Prove (or disprove) that snapshot corruption is fixed by running sandboxes
through corruption-prone lifecycles against production.

**Target:** Production cluster (Azure East US 2)
**Client:** TypeScript SDK
**Concurrency limit:** 5 simultaneous sandboxes (API limit)

---

## What Can We Actually Test From the SDK?

The incident report identified 4 corruption vectors. Not all are testable from
the SDK because corruption often lives in S3 artifacts, not local files.

| Vector | What gets corrupted | SDK-testable? | Why / why not |
|---|---|---|---|
| #1 Hibernate archive race | S3 archive | **Statistically at scale** | Same-worker wake uses local files (always fine). Cross-worker wake downloads from S3 (where corruption lives). We can't force cross-worker wake, but at scale the scheduler naturally distributes — some wakes WILL download from S3. |
| #2 Destroy during archive | S3 archive | **No** | Once destroyed, nobody downloads the corrupt archive. Internal consistency issue only. |
| #3 Migration without lock | S3 upload | **No** | Migration is an internal operation the SDK can't trigger. |
| #4 Checkpoint cache lifetime | Local cache dir | **Yes, deterministically** | We can race fork-from-checkpoint against checkpoint deletion. If cache is deleted mid-fork, the fork gets corrupted/partial files. |

### How corruption actually works (Vector 1 example)

```
1. Hibernate → QEMU saves state into qcow2 files, exits
2. Archive goroutine starts tar-ing the qcow2 files (async, big files = slow)
3. Hibernate API returns to caller (archive still running)
4. Wake arrives immediately → new QEMU opens SAME qcow2 files, runs loadvm
5. loadvm MODIFIES qcow2 while tar is mid-read
6. tar produces corrupt archive (mix of pre/post-loadvm blocks)
7. Corrupt archive uploaded to S3
8. LOCAL files are fine — user's current session works perfectly
9. LATER: sandbox migrates to another worker → downloads corrupt archive → broken
```

The key: corruption doesn't manifest at the moment of the race. It manifests
**later**, when another worker trusts the bad S3 archive.

To deterministically test Vector 1, we'd need worker-level access to delete
local qcow2 files between hibernate and wake, forcing the S3 download path.
That's outside SDK scope.

---

## Test Design (v2)

Two focused phases instead of one lifecycle gauntlet.

### Phase 1: Fork Blast — Deterministic (Vector 4)

The strongest signal we can get from the SDK. Tests checkpoint cache integrity
under concurrent fork + delete.

```
Setup:
  1. Create sandbox
  2. Write 5MB marker file, record SHA256
  3. Checkpoint, wait for ready

Fork blast:
  4. Launch 5 forks from checkpoint simultaneously
  5. While forks are still creating/booting, delete the checkpoint
  6. Each fork: verify marker SHA256
  7. Report: any mismatch = corruption

Cleanup:
  8. Kill all forks + original sandbox
```

**Why this catches Vector 4:** `ForkFromCheckpoint` reads qcow2 files from the
local cache under a read lock. `CleanCheckpointCache` (triggered by delete)
needs the write lock. If the fix works, delete blocks until all forks finish
copying. If it doesn't, the cache dir gets removed mid-copy → corrupted fork.

Run this M times (default 5) with different sandboxes to increase coverage.

### Phase 2: Hibernate/Wake Soak — Statistical (Vector 1)

High-cycle hibernate/wake across many sandboxes. Relies on natural cross-worker
distribution to exercise the S3 archive path.

```
Per sandbox (N sandboxes, C concurrent):
  1. Create sandbox
  2. Write 5MB marker, record SHA256
  3. Loop K times:
     a. Hibernate (archive starts uploading async)
     b. Wake immediately (no delay)
     c. Verify marker SHA256
  4. Destroy
```

**Why this has a chance of catching Vector 1:** With N=20 sandboxes doing K=5
hibernate/wake cycles each = 100 total cycles. The production cluster has
multiple workers. Under load, the scheduler may reassign some sandboxes to
different workers on wake, forcing S3 archive download. Each such cross-worker
wake is a chance to detect a corrupt archive.

At N=20: probabilistic, some cross-worker wakes likely.
At N=1000: near-certain, hundreds of cross-worker wakes.

**What a "pass" means:** Zero SHA256 mismatches across all cycles. This doesn't
guarantee Vector 1 is fixed (we might have gotten lucky with same-worker wakes),
but a failure IS definitive proof of corruption.

**What a "fail" means:** At least one SHA256 mismatch after wake. This is
unambiguous corruption — the file content changed through a hibernate/wake cycle.

---

## CLI Interface

```
npx tsx scripts/stress-snapshot-corruption.ts \
  --count 20 \
  --concurrency 5 \
  --cycles 5 \
  --marker-size 5 \
  --fork-rounds 5 \
  --api-key sk-... \
  --api-url https://app.opencomputer.dev \
  --output report.json
```

| Flag | Default | Description |
|---|---|---|
| `-n, --count` | 20 | Number of sandboxes for hibernate/wake soak |
| `-c, --concurrency` | 5 | Max simultaneous sandboxes |
| `--cycles` | 5 | Hibernate/wake cycles per sandbox |
| `--marker-size` | 5 | Marker file size in MB |
| `--fork-rounds` | 5 | Number of fork blast rounds (0 to skip) |
| `--forks-per-round` | 5 | Concurrent forks per round |
| `--api-key` | `$OPENCOMPUTER_API_KEY` | API key |
| `--api-url` | `$OPENCOMPUTER_API_URL` | API URL |
| `-o, --output` | — | Write JSON report to file |

---

## Data Model

```typescript
interface ForkBlastRound {
  round: number;
  sourceSandboxId: string;
  checkpointId: string;
  markerSha256: string;
  forks: {
    sandboxId: string;
    markerVerified: boolean;
    actualSha256?: string;
    error?: string;
  }[];
  checkpointDeletedDuringForks: boolean;
  corrupted: number;
}

interface HibernateWakeResult {
  index: number;
  sandboxId: string;
  markerSha256: string;
  cycles: {
    cycle: number;
    hibernateMs: number;
    wakeMs: number;
    verified: boolean;
    actualSha256?: string;
  }[];
  corrupted: boolean;       // any cycle failed verification
  corruptedAtCycle?: number; // first cycle where corruption detected
  error?: string;
  failedAt?: string;
}

interface TestReport {
  startedAt: string;
  completedAt: string;
  config: { count: number; concurrency: number; cycles: number; forkRounds: number; markerSizeMB: number };
  phase1_forkBlast: {
    rounds: ForkBlastRound[];
    totalForks: number;
    totalCorrupted: number;
  };
  phase2_hibernateWake: {
    results: HibernateWakeResult[];
    totalCycles: number;
    totalCorrupted: number;
    totalErrored: number;
  };
  summary: {
    totalDurationMs: number;
    corruption: boolean;  // ANY corruption across both phases
  };
}
```

---

## Verification

**Phase 1 (fork blast):**
- Each fork reads marker file, computes SHA256, compares to original
- Any mismatch = definitive corruption (Vector 4)

**Phase 2 (hibernate/wake soak):**
- After each wake, read marker, compute SHA256, compare to original
- Any mismatch = definitive corruption (likely Vector 1 via cross-worker wake)

**Pass criteria:**
- `phase1.totalCorrupted == 0` — checkpoint cache integrity holds under concurrent fork+delete
- `phase2.totalCorrupted == 0` — no data loss across hibernate/wake cycles
- Errors (timeouts, API failures) acceptable if <10% and not data-loss related

---

## Scaling to 1000

Once 20 passes clean:
1. Increase concurrency limit (or use multiple API keys)
2. `--count 1000 --concurrency 50 --cycles 10`
3. Total cycles = 10,000 hibernate/wake operations
4. At this scale, hundreds of cross-worker wakes are near-certain
5. Customer deliverable: "1000 sandboxes × 10 cycles = 10,000 hibernate/wake ops, 0 corruption"

---

## Limitations (be honest with customer)

- **Vector 1 coverage is statistical, not deterministic.** We can't force the
  S3 download path from the SDK. At N=1000 we're highly confident, at N=20
  we have partial coverage. Zero corruption at N=20 is encouraging but not proof.
- **Vectors 2 and 3 are not covered.** They require internal/worker-level access.
  They were validated by the internal stress tests (48 iterations, see incident report).
- **Vector 4 coverage IS deterministic.** The fork blast directly races the
  exact operations that would expose cache lifetime bugs.
