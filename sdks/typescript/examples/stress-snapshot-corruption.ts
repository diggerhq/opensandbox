/**
 * Snapshot Corruption Stress Test
 *
 * Runs N sandboxes through a corruption-prone lifecycle designed to expose
 * race conditions in hibernate/wake, checkpoint/fork, and destroy paths.
 *
 * Each sandbox: create → write marker → checkpoint → write marker2 →
 *   hibernate → wake (no delay) → verify SHA256 → destroy
 *
 * Optional fork blast: 5 concurrent forks from same checkpoint +
 * checkpoint deletion while forks verify.
 *
 * Usage:
 *   npx tsx examples/stress-snapshot-corruption.ts --count 20 --concurrency 5
 *   npx tsx examples/stress-snapshot-corruption.ts -n 5 -c 2 --fork-blast
 *   npx tsx examples/stress-snapshot-corruption.ts -n 20 -c 5 -o report.json
 */

import { Sandbox } from "../src/index";
import { createHash, randomBytes } from "node:crypto";
import { writeFileSync } from "node:fs";
import { parseArgs } from "node:util";

// ── CLI args ────────────────────────────────────────────────────────────

const { values: args } = parseArgs({
  options: {
    count: { type: "string", short: "n", default: "20" },
    concurrency: { type: "string", short: "c", default: "5" },
    "marker-size": { type: "string", default: "5" },
    snapshot: { type: "string", default: "" },
    "api-key": { type: "string", default: process.env.OC_API_KEY ?? "" },
    "api-url": { type: "string", default: process.env.OC_API_URL ?? "" },
    "fork-blast": { type: "boolean", default: false },
    output: { type: "string", short: "o", default: "" },
    help: { type: "boolean", short: "h", default: false },
  },
  strict: true,
});

if (args.help) {
  console.log(`
Snapshot Corruption Stress Test

Options:
  -n, --count <num>         Number of sandboxes (default: 20)
  -c, --concurrency <num>   Max simultaneous sandboxes (default: 5)
  --marker-size <mb>        Marker file size in MB (default: 5)
  --snapshot <name>         Pre-built snapshot name
  --api-key <key>           API key (default: $OC_API_KEY)
  --api-url <url>           API URL (default: $OC_API_URL)
  --fork-blast              Run fork blast phase after gauntlet
  -o, --output <file>       Write JSON report to file
  -h, --help                Show this help
`);
  process.exit(0);
}

const COUNT = parseInt(args.count!, 10);
const CONCURRENCY = parseInt(args.concurrency!, 10);
const MARKER_SIZE_MB = parseInt(args["marker-size"]!, 10);
const SNAPSHOT = args.snapshot || undefined;
const API_KEY = args["api-key"]!;
const API_URL = args["api-url"]! || undefined;
const FORK_BLAST = args["fork-blast"]!;
const OUTPUT = args.output || undefined;

if (!API_KEY) {
  console.error("Error: --api-key or $OC_API_KEY required");
  process.exit(1);
}

// ── Types ───────────────────────────────────────────────────────────────

interface MarkerResult {
  path: string;
  sha256: string;
  verified: boolean;
  verifiedSha256?: string;
}

interface Lifecycle {
  createMs: number;
  writeMarker1Ms: number;
  checkpointMs: number;
  writeMarker2Ms: number;
  hibernateMs: number;
  wakeMs: number;
  verifyMs: number;
  destroyMs: number;
}

interface SandboxResult {
  index: number;
  sandboxId: string;
  checkpointId: string;
  lifecycle: Partial<Lifecycle>;
  markers: { marker1?: MarkerResult; marker2?: MarkerResult };
  corrupted: boolean;
  error?: string;
  failedAt?: string;
}

interface ForkBlastResult {
  sourceCheckpointId: string;
  forks: number;
  results: {
    sandboxId: string;
    marker1Verified: boolean;
    marker2Absent: boolean;
    error?: string;
  }[];
  corrupted: number;
}

interface TestReport {
  startedAt: string;
  completedAt: string;
  config: {
    count: number;
    concurrency: number;
    markerSizeMB: number;
    snapshot?: string;
  };
  results: SandboxResult[];
  summary: {
    created: number;
    completed: number;
    corrupted: number;
    errored: number;
    totalDurationMs: number;
  };
  forkBlast?: ForkBlastResult;
}

// ── Formatting helpers ──────────────────────────────────────────────────

function green(msg: string) { console.log(`\x1b[32m✓ ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m✗ ${msg}\x1b[0m`); }
function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }
function yellow(msg: string) { console.log(`\x1b[33m⚠ ${msg}\x1b[0m`); }

function formatMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

function sleep(ms: number) {
  return new Promise((r) => globalThis.setTimeout(r, ms));
}

function sha256(data: string | Buffer | Uint8Array): string {
  return createHash("sha256").update(data).digest("hex");
}

// ── Concurrency pool ────────────────────────────────────────────────────

function createPool(limit: number) {
  let active = 0;
  const queue: (() => void)[] = [];

  async function acquire(): Promise<void> {
    if (active < limit) {
      active++;
      return;
    }
    await new Promise<void>((resolve) => queue.push(resolve));
    active++;
  }

  function release(): void {
    active--;
    const next = queue.shift();
    if (next) next();
  }

  return { acquire, release };
}

// ── Core helpers ────────────────────────────────────────────────────────

const sdkOpts = {
  ...(API_KEY ? { apiKey: API_KEY } : {}),
  ...(API_URL ? { apiUrl: API_URL } : {}),
};

async function timed<T>(fn: () => Promise<T>): Promise<{ result: T; ms: number }> {
  const start = Date.now();
  const result = await fn();
  return { result, ms: Date.now() - start };
}

async function writeMarker(
  sb: Sandbox,
  name: string,
): Promise<{ path: string; sha256: string }> {
  const data = randomBytes(MARKER_SIZE_MB * 1024 * 1024).toString("base64");
  const hash = sha256(data);
  const path = `/home/user/${name}`;
  await sb.files.write(path, data);
  // Verify write round-trip immediately
  const readBack = await sb.files.read(path);
  if (sha256(readBack) !== hash) {
    throw new Error(`Write verification failed for ${name} — data corrupted on initial write`);
  }
  return { path, sha256: hash };
}

async function verifyMarker(
  sb: Sandbox,
  path: string,
  expectedSha256: string,
): Promise<{ verified: boolean; actualSha256: string }> {
  const content = await sb.files.read(path);
  const actual = sha256(content);
  return { verified: actual === expectedSha256, actualSha256: actual };
}

async function waitForCheckpointReady(
  sb: Sandbox,
  checkpointId: string,
  timeoutMs = 120_000,
): Promise<boolean> {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const list = await sb.listCheckpoints();
    const cp = list.find((c) => c.id === checkpointId);
    if (cp && cp.status === "ready") return true;
    if (cp && cp.status !== "processing") return false;
    await sleep(2000);
  }
  return false;
}

// ── Single sandbox lifecycle ────────────────────────────────────────────

async function runLifecycle(index: number): Promise<SandboxResult> {
  const result: SandboxResult = {
    index,
    sandboxId: "",
    checkpointId: "",
    lifecycle: {},
    markers: {},
    corrupted: false,
  };

  let sb: Sandbox | undefined;

  try {
    // Step 1: Create
    const createOpts = SNAPSHOT
      ? { snapshot: SNAPSHOT, timeout: 300, ...sdkOpts }
      : { timeout: 300, ...sdkOpts };
    const { result: sandbox, ms: createMs } = await timed(() => Sandbox.create(createOpts));
    sb = sandbox;
    result.sandboxId = sb.sandboxId;
    result.lifecycle.createMs = createMs;
    dim(`[${index}] created ${sb.sandboxId} (${formatMs(createMs)})`);

    // Step 2: Write marker 1
    const { result: m1, ms: writeM1Ms } = await timed(() => writeMarker(sb!, `marker1-${index}.bin`));
    result.lifecycle.writeMarker1Ms = writeM1Ms;
    result.markers.marker1 = { path: m1.path, sha256: m1.sha256, verified: false };
    dim(`[${index}] wrote marker1 (${formatMs(writeM1Ms)})`);

    // Step 3: Checkpoint
    const { result: cpReady, ms: cpMs } = await timed(async () => {
      const cp = await sb!.createCheckpoint(`stress-${index}-${Date.now()}`);
      result.checkpointId = cp.id;
      const ready = await waitForCheckpointReady(sb!, cp.id);
      if (!ready) throw new Error(`Checkpoint ${cp.id} never became ready`);
      return ready;
    });
    result.lifecycle.checkpointMs = cpMs;
    dim(`[${index}] checkpoint ready (${formatMs(cpMs)})`);

    // Step 4: Write marker 2 (after checkpoint — should NOT appear in forks)
    const { result: m2, ms: writeM2Ms } = await timed(() => writeMarker(sb!, `marker2-${index}.bin`));
    result.lifecycle.writeMarker2Ms = writeM2Ms;
    result.markers.marker2 = { path: m2.path, sha256: m2.sha256, verified: false };
    dim(`[${index}] wrote marker2 (${formatMs(writeM2Ms)})`);

    // Step 5: Hibernate
    const { ms: hibMs } = await timed(() => sb!.hibernate());
    result.lifecycle.hibernateMs = hibMs;
    dim(`[${index}] hibernated (${formatMs(hibMs)})`);

    // Step 6: Wake — no delay, immediately after hibernate returns
    const { ms: wakeMs } = await timed(() => sb!.wake({ timeout: 120 }));
    result.lifecycle.wakeMs = wakeMs;
    dim(`[${index}] woke (${formatMs(wakeMs)})`);

    // Step 7: Verify both markers
    const { ms: verifyMs } = await timed(async () => {
      const v1 = await verifyMarker(sb!, m1.path, m1.sha256);
      result.markers.marker1!.verified = v1.verified;
      result.markers.marker1!.verifiedSha256 = v1.actualSha256;

      const v2 = await verifyMarker(sb!, m2.path, m2.sha256);
      result.markers.marker2!.verified = v2.verified;
      result.markers.marker2!.verifiedSha256 = v2.actualSha256;

      if (!v1.verified || !v2.verified) {
        result.corrupted = true;
        red(`[${index}] CORRUPTION DETECTED`);
        if (!v1.verified) red(`  marker1: expected ${m1.sha256}, got ${v1.actualSha256}`);
        if (!v2.verified) red(`  marker2: expected ${m2.sha256}, got ${v2.actualSha256}`);
      }
    });
    result.lifecycle.verifyMs = verifyMs;

    if (!result.corrupted) {
      green(`[${index}] verified OK (${formatMs(verifyMs)})`);
    }

    // Step 8: Destroy
    const { ms: destroyMs } = await timed(() => sb!.kill());
    result.lifecycle.destroyMs = destroyMs;
    sb = undefined; // prevent double-kill in finally
    dim(`[${index}] destroyed (${formatMs(destroyMs)})`);

  } catch (err: any) {
    result.error = err.message ?? String(err);
    result.failedAt = inferStage(result.lifecycle);
    red(`[${index}] FAILED at ${result.failedAt}: ${result.error}`);
  } finally {
    if (sb) {
      try { await sb.kill(); } catch {}
    }
  }

  return result;
}

function inferStage(lifecycle: Partial<Lifecycle>): string {
  if (lifecycle.destroyMs != null) return "destroy";
  if (lifecycle.verifyMs != null) return "verify";
  if (lifecycle.wakeMs != null) return "wake";
  if (lifecycle.hibernateMs != null) return "hibernate";
  if (lifecycle.writeMarker2Ms != null) return "writeMarker2";
  if (lifecycle.checkpointMs != null) return "checkpoint";
  if (lifecycle.writeMarker1Ms != null) return "writeMarker1";
  if (lifecycle.createMs != null) return "create";
  return "create";
}

// ── Fork blast ──────────────────────────────────────────────────────────

async function runForkBlast(
  sourceCheckpointId: string,
  expectedMarker1: { path: string; sha256: string },
  marker2Path: string,
): Promise<ForkBlastResult> {
  const FORK_COUNT = 5;
  bold("\n── Fork Blast ─────────────────────────────────────────");
  dim(`Forking ${FORK_COUNT}x from checkpoint ${sourceCheckpointId}`);

  const forkResults: ForkBlastResult["results"] = [];

  // Launch all forks simultaneously
  const promises = Array.from({ length: FORK_COUNT }, async (_, i) => {
    let fork: Sandbox | undefined;
    try {
      fork = await Sandbox.createFromCheckpoint(sourceCheckpointId, {
        timeout: 120,
        ...sdkOpts,
      });
      dim(`  fork[${i}] created: ${fork.sandboxId}`);

      // Verify marker1 is present and intact
      const v1 = await verifyMarker(fork, expectedMarker1.path, expectedMarker1.sha256);

      // Verify marker2 is NOT present (written after checkpoint)
      let marker2Absent = false;
      try {
        await fork.files.read(marker2Path);
        marker2Absent = false; // file exists — unexpected
      } catch {
        marker2Absent = true; // file not found — correct
      }

      const entry = {
        sandboxId: fork.sandboxId,
        marker1Verified: v1.verified,
        marker2Absent,
        ...((!v1.verified || !marker2Absent) ? { error: `marker1=${v1.verified}, marker2Absent=${marker2Absent}` } : {}),
      };
      forkResults.push(entry);

      if (v1.verified && marker2Absent) {
        green(`  fork[${i}] verified OK`);
      } else {
        red(`  fork[${i}] CORRUPTION: marker1=${v1.verified}, marker2Absent=${marker2Absent}`);
      }
    } catch (err: any) {
      forkResults.push({
        sandboxId: fork?.sandboxId ?? "unknown",
        marker1Verified: false,
        marker2Absent: false,
        error: err.message,
      });
      red(`  fork[${i}] FAILED: ${err.message}`);
    } finally {
      if (fork) {
        try { await fork.kill(); } catch {}
      }
    }
  });

  await Promise.allSettled(promises);

  const corrupted = forkResults.filter(
    (r) => !r.marker1Verified || !r.marker2Absent || r.error,
  ).length;

  return {
    sourceCheckpointId,
    forks: FORK_COUNT,
    results: forkResults,
    corrupted,
  };
}

// ── Main ────────────────────────────────────────────────────────────────

async function main() {
  bold("╔══════════════════════════════════════════════════════════╗");
  bold("║     Snapshot Corruption Stress Test                     ║");
  bold("╚══════════════════════════════════════════════════════════╝");
  console.log();
  dim(`Count: ${COUNT}  Concurrency: ${CONCURRENCY}  Marker: ${MARKER_SIZE_MB}MB`);
  if (SNAPSHOT) dim(`Snapshot: ${SNAPSHOT}`);
  if (FORK_BLAST) dim(`Fork blast: enabled`);
  dim(`API: ${API_URL ?? "(default)"}`);
  console.log();

  const startedAt = new Date().toISOString();
  const startMs = Date.now();

  // Run lifecycle gauntlet with concurrency pool
  const pool = createPool(CONCURRENCY);
  const promises = Array.from({ length: COUNT }, async (_, i) => {
    await pool.acquire();
    try {
      return await runLifecycle(i);
    } finally {
      pool.release();
    }
  });

  const results = await Promise.allSettled(promises);
  const sandboxResults: SandboxResult[] = results.map((r, i) => {
    if (r.status === "fulfilled") return r.value;
    return {
      index: i,
      sandboxId: "",
      checkpointId: "",
      lifecycle: {},
      markers: {},
      corrupted: false,
      error: r.reason?.message ?? String(r.reason),
      failedAt: "unknown",
    };
  });

  // Fork blast (uses first successful checkpoint)
  let forkBlastResult: ForkBlastResult | undefined;
  if (FORK_BLAST) {
    const donor = sandboxResults.find((r) => r.checkpointId && !r.error && r.markers.marker1);
    if (donor) {
      forkBlastResult = await runForkBlast(
        donor.checkpointId,
        { path: donor.markers.marker1!.path, sha256: donor.markers.marker1!.sha256 },
        donor.markers.marker2?.path ?? `/home/user/marker2-${donor.index}.bin`,
      );

      // Clean up the donor checkpoint
      try {
        const donorSb = await Sandbox.connect(donor.sandboxId, sdkOpts);
        await donorSb.deleteCheckpoint(donor.checkpointId);
        dim("Donor checkpoint deleted");
      } catch {
        dim("Donor checkpoint cleanup skipped (sandbox already destroyed)");
      }
    } else {
      yellow("Fork blast skipped — no successful checkpoint available");
    }
  }

  const totalMs = Date.now() - startMs;
  const completedAt = new Date().toISOString();

  // Summary
  const created = sandboxResults.filter((r) => r.sandboxId).length;
  const completed = sandboxResults.filter(
    (r) => r.lifecycle.destroyMs != null || r.lifecycle.verifyMs != null,
  ).length;
  const corrupted = sandboxResults.filter((r) => r.corrupted).length;
  const errored = sandboxResults.filter((r) => r.error).length;

  console.log();
  bold("══════════════════════════════════════════════════════════");
  bold("  RESULTS");
  bold("══════════════════════════════════════════════════════════");
  console.log();

  // Timing table
  const completedResults = sandboxResults.filter((r) => r.lifecycle.createMs != null);
  if (completedResults.length > 0) {
    const stages = [
      "createMs", "writeMarker1Ms", "checkpointMs", "writeMarker2Ms",
      "hibernateMs", "wakeMs", "verifyMs", "destroyMs",
    ] as const;

    for (const stage of stages) {
      const vals = completedResults
        .map((r) => (r.lifecycle as Record<string, number | undefined>)[stage])
        .filter((v): v is number => v != null);
      if (vals.length === 0) continue;
      const avg = vals.reduce((a, b) => a + b, 0) / vals.length;
      const max = Math.max(...vals);
      const min = Math.min(...vals);
      const label = stage.replace("Ms", "").padEnd(14);
      dim(`${label}  avg=${formatMs(avg)}  min=${formatMs(min)}  max=${formatMs(max)}`);
    }
    console.log();
  }

  // Aggregate
  const tag = corrupted > 0 ? "\x1b[31m" : "\x1b[32m";
  console.log(
    `${tag}  ${created} created / ${completed} completed / ${corrupted} corrupted / ${errored} errored\x1b[0m`,
  );
  dim(`Total time: ${formatMs(totalMs)}`);

  if (forkBlastResult) {
    console.log();
    const fTag = forkBlastResult.corrupted > 0 ? "\x1b[31m" : "\x1b[32m";
    console.log(
      `${fTag}  Fork blast: ${forkBlastResult.forks} forks / ${forkBlastResult.corrupted} corrupted\x1b[0m`,
    );
  }

  console.log();

  // List failures
  const failures = sandboxResults.filter((r) => r.corrupted || r.error);
  if (failures.length > 0) {
    bold("  Failures:");
    for (const f of failures) {
      if (f.corrupted) {
        red(`  [${f.index}] ${f.sandboxId} — CORRUPTED at verify`);
        if (f.markers.marker1 && !f.markers.marker1.verified) {
          dim(`    marker1: expected ${f.markers.marker1.sha256}`);
          dim(`         got ${f.markers.marker1.verifiedSha256}`);
        }
        if (f.markers.marker2 && !f.markers.marker2.verified) {
          dim(`    marker2: expected ${f.markers.marker2.sha256}`);
          dim(`         got ${f.markers.marker2.verifiedSha256}`);
        }
      } else if (f.error) {
        yellow(`  [${f.index}] ${f.sandboxId || "no-id"} — ${f.failedAt}: ${f.error}`);
      }
    }
    console.log();
  }

  // Build report
  const report: TestReport = {
    startedAt,
    completedAt,
    config: {
      count: COUNT,
      concurrency: CONCURRENCY,
      markerSizeMB: MARKER_SIZE_MB,
      ...(SNAPSHOT ? { snapshot: SNAPSHOT } : {}),
    },
    results: sandboxResults,
    summary: { created, completed, corrupted, errored, totalDurationMs: totalMs },
    ...(forkBlastResult ? { forkBlast: forkBlastResult } : {}),
  };

  if (OUTPUT) {
    writeFileSync(OUTPUT, JSON.stringify(report, null, 2));
    dim(`Report written to ${OUTPUT}`);
  }

  // Exit code
  const forkCorrupted = forkBlastResult?.corrupted ?? 0;
  if (corrupted > 0 || forkCorrupted > 0) {
    red(`\nFAILED — ${corrupted + forkCorrupted} corruption(s) detected`);
    process.exit(1);
  } else if (errored > 0 && errored > COUNT * 0.1) {
    yellow(`\nWARN — 0 corruptions but ${errored} errors (>${Math.round(COUNT * 0.1)} threshold)`);
    process.exit(2);
  } else {
    green(`\nPASSED — ${completed} sandboxes verified, 0 corruptions`);
    process.exit(0);
  }
}

main().catch((err) => {
  red(`Fatal: ${err.message}`);
  console.error(err);
  process.exit(1);
});
