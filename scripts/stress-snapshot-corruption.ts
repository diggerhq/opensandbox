/**
 * Snapshot Corruption Stress Test (v3)
 *
 * Reproduces the customer's exact failure: restore from snapshot → git segfaults.
 * Runs 5 rounds × 200 restores = 1000 total. Each round creates a fresh
 * checkpoint (testing creation reliability), destroys the source, then
 * restores 200 times verifying git + file integrity.
 *
 * Usage:
 *   npx tsx scripts/stress-snapshot-corruption.ts -n 1000 -r 5 -c 5
 *   npx tsx scripts/stress-snapshot-corruption.ts -n 10 -r 1 -c 2       # quick smoke test
 *   npx tsx scripts/stress-snapshot-corruption.ts -n 1000 -r 5 -o report.json
 */

import { Sandbox } from "../sdks/typescript/src/index";
import { createHash, randomBytes } from "node:crypto";
import { writeFileSync } from "node:fs";
import { parseArgs } from "node:util";

// ── CLI args ────────────────────────────────────────────────────────────

const { values: args } = parseArgs({
  options: {
    restores: { type: "string", short: "n", default: "1000" },
    rounds: { type: "string", short: "r", default: "5" },
    concurrency: { type: "string", short: "c", default: "5" },
    "marker-size": { type: "string", default: "5" },
    "api-key": { type: "string", default: process.env.OPENCOMPUTER_API_KEY ?? "" },
    "api-url": { type: "string", default: process.env.OPENCOMPUTER_API_URL ?? "" },
    output: { type: "string", short: "o", default: "" },
    help: { type: "boolean", short: "h", default: false },
  },
  strict: true,
});

if (args.help) {
  console.log(`
Snapshot Corruption Stress Test (v3)

Creates snapshots and restores from them at scale, verifying git integrity
and file data after each restore. Reproduces the customer's "git segfault
on restore from snapshot" scenario.

Options:
  -n, --restores <num>      Total restores across all rounds (default: 1000)
  -r, --rounds <num>        Independent snapshot rounds (default: 5)
  -c, --concurrency <num>   Max simultaneous restores (default: 5)
  --marker-size <mb>        Marker file size in MB (default: 5)
  --api-key <key>           API key (default: $OPENCOMPUTER_API_KEY)
  --api-url <url>           API URL (default: $OPENCOMPUTER_API_URL)
  -o, --output <file>       Write JSON report to file
  -h, --help                Show this help

Examples:
  # Quick smoke test: 1 round, 10 restores
  npx tsx scripts/stress-snapshot-corruption.ts -n 10 -r 1 -c 2

  # Full run: 5 rounds × 200 restores = 1000 total
  npx tsx scripts/stress-snapshot-corruption.ts -n 1000 -r 5 -c 5 -o report.json
`);
  process.exit(0);
}

const TOTAL_RESTORES = parseInt(args.restores!, 10);
const ROUNDS = parseInt(args.rounds!, 10);
const CONCURRENCY = parseInt(args.concurrency!, 10);
const MARKER_SIZE_MB = parseInt(args["marker-size"]!, 10);
const API_KEY = args["api-key"]!;
const API_URL = args["api-url"]! || undefined;
const OUTPUT = args.output || undefined;
const RESTORES_PER_ROUND = Math.ceil(TOTAL_RESTORES / ROUNDS);

if (!API_KEY) {
  console.error("Error: --api-key or $OPENCOMPUTER_API_KEY required");
  process.exit(1);
}

// ── Types ───────────────────────────────────────────────────────────────

interface RestoreResult {
  index: number;
  sandboxId: string;
  gitStatusOk: boolean;
  gitLogOk: boolean;
  markerVerified: boolean;
  markerActualSha256?: string;
  createMs: number;
  verifyMs: number;
  error?: string;
}

interface RoundResult {
  round: number;
  sourceSandboxId: string;
  checkpointId: string;
  markerSha256: string;
  expectedGitLog: string;
  setupMs: number;
  restores: RestoreResult[];
  totalRestores: number;
  corrupted: number;
  errored: number;
}

interface TestReport {
  startedAt: string;
  completedAt: string;
  config: {
    totalRestores: number;
    rounds: number;
    restoresPerRound: number;
    concurrency: number;
    markerSizeMB: number;
  };
  rounds: RoundResult[];
  summary: {
    totalRestores: number;
    totalCorrupted: number;
    totalErrored: number;
    totalDurationMs: number;
    corruption: boolean;
  };
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

// ── Round setup: create source sandbox + checkpoint ─────────────────────

interface RoundSetup {
  sourceSandbox: Sandbox;
  sourceSandboxId: string;
  checkpointId: string;
  markerPath: string;
  markerSha256: string;
  expectedGitLog: string;
  setupMs: number;
}

async function setupRound(round: number): Promise<RoundSetup> {
  const { result: setup, ms: setupMs } = await timed(async () => {
    // Create sandbox
    const sb = await Sandbox.create({ timeout: 300, ...sdkOpts });
    dim(`  [round ${round}] source sandbox: ${sb.sandboxId}`);

    // Write marker file
    const markerData = randomBytes(MARKER_SIZE_MB * 1024 * 1024).toString("base64");
    const markerHash = sha256(markerData);
    const markerPath = `/workspace/marker-${round}.bin`;
    await sb.files.write(markerPath, markerData);

    // Verify write
    const readBack = await sb.files.read(markerPath);
    if (sha256(readBack) !== markerHash) {
      throw new Error("Marker write verification failed");
    }
    dim(`  [round ${round}] marker written (${MARKER_SIZE_MB}MB)`);

    // Set up git repo — reproduces the customer's scenario
    const gitSetup = await sb.commands.run(
      `cd /workspace && git config --global user.email "test@test.com" && git config --global user.name "test" && git init && git add marker-${round}.bin && git commit -m "round-${round}-snapshot"`,
      { timeout: 30 },
    );
    if (gitSetup.exitCode !== 0) {
      throw new Error(`git setup failed: ${gitSetup.stderr}`);
    }
    const expectedGitLog = `round-${round}-snapshot`;
    dim(`  [round ${round}] git repo initialized`);

    // Create checkpoint
    const cp = await sb.createCheckpoint(`stress-round-${round}-${Date.now()}`);
    const ready = await waitForCheckpointReady(sb, cp.id);
    if (!ready) throw new Error(`Checkpoint ${cp.id} never became ready`);
    dim(`  [round ${round}] checkpoint ready: ${cp.id}`);

    return {
      sourceSandbox: sb,
      sourceSandboxId: sb.sandboxId,
      checkpointId: cp.id,
      markerPath,
      markerSha256: markerHash,
      expectedGitLog,
    };
  });

  return { ...setup, setupMs };
}

// ── Single restore + verify ─────────────────────────────────────────────

async function runRestore(
  round: number,
  index: number,
  checkpointId: string,
  markerPath: string,
  markerSha256: string,
  expectedGitLog: string,
): Promise<RestoreResult> {
  const result: RestoreResult = {
    index,
    sandboxId: "",
    gitStatusOk: false,
    gitLogOk: false,
    markerVerified: false,
    createMs: 0,
    verifyMs: 0,
  };

  let sb: Sandbox | undefined;

  try {
    // Restore from checkpoint
    const { result: sandbox, ms: createMs } = await timed(() =>
      Sandbox.createFromCheckpoint(checkpointId, { timeout: 120, ...sdkOpts })
    );
    sb = sandbox;
    result.sandboxId = sb.sandboxId;
    result.createMs = createMs;

    // Verify
    const { ms: verifyMs } = await timed(async () => {
      // 1. git status — segfault = corruption (customer's exact failure)
      const gitStatus = await sb!.commands.run("cd /workspace && git status", { timeout: 15 });
      result.gitStatusOk = gitStatus.exitCode === 0;
      if (!result.gitStatusOk) {
        const sig = gitStatus.exitCode > 128 ? ` (signal ${gitStatus.exitCode - 128})` : "";
        throw new Error(`git status failed: exit ${gitStatus.exitCode}${sig} stderr=${gitStatus.stderr}`);
      }

      // 2. git log — verify object database intact
      const gitLog = await sb!.commands.run("cd /workspace && git log --oneline -1", { timeout: 15 });
      result.gitLogOk = gitLog.exitCode === 0 && gitLog.stdout.includes(expectedGitLog);
      if (!result.gitLogOk) {
        throw new Error(`git log mismatch: expected "${expectedGitLog}", got "${gitLog.stdout.trim()}"`);
      }

      // 3. Marker file SHA256
      const content = await sb!.files.read(markerPath);
      const actual = sha256(content);
      result.markerVerified = actual === markerSha256;
      result.markerActualSha256 = actual;
      if (!result.markerVerified) {
        throw new Error(`SHA256 mismatch: expected ${markerSha256}, got ${actual}`);
      }
    });
    result.verifyMs = verifyMs;

    // Destroy
    await sb.kill();
    sb = undefined;

  } catch (err: any) {
    result.error = err.message ?? String(err);
  } finally {
    if (sb) {
      try { await sb.kill(); } catch {}
    }
  }

  return result;
}

// ── Run one round ───────────────────────────────────────────────────────

async function runRound(round: number, restoreCount: number): Promise<RoundResult> {
  bold(`\n── Round ${round + 1}/${ROUNDS} (${restoreCount} restores) ──────────────────────────`);

  // Setup
  const setup = await setupRound(round);

  // Run restores with concurrency pool
  const pool = createPool(CONCURRENCY);
  let completed = 0;

  const promises = Array.from({ length: restoreCount }, async (_, i) => {
    await pool.acquire();
    try {
      const r = await runRestore(
        round, i, setup.checkpointId,
        setup.markerPath, setup.markerSha256, setup.expectedGitLog,
      );

      completed++;
      const globalIndex = round * RESTORES_PER_ROUND + i;
      const isCorrupted = !r.gitStatusOk || !r.gitLogOk || !r.markerVerified;

      // Progress: every restore if ≤20, every 10 if ≤200, every 25 otherwise
      const interval = restoreCount <= 20 ? 1 : restoreCount <= 200 ? 10 : 25;

      if (isCorrupted) {
        red(`  [${globalIndex}] CORRUPTED: ${r.error}`);
      } else if (r.error) {
        yellow(`  [${globalIndex}] ERROR: ${r.error}`);
      } else if (completed % interval === 0 || completed === restoreCount) {
        dim(`  [round ${round + 1}] ${completed}/${restoreCount} (${formatMs(r.createMs)} create, ${formatMs(r.verifyMs)} verify)`);
      }

      return r;
    } finally {
      pool.release();
    }
  });

  const settled = await Promise.allSettled(promises);

  // Destroy source sandbox after all restores complete
  try {
    await setup.sourceSandbox.kill();
    dim(`  [round ${round + 1}] source sandbox destroyed`);
  } catch {}

  const restores: RestoreResult[] = settled.map((r, i) => {
    if (r.status === "fulfilled") return r.value;
    return {
      index: i,
      sandboxId: "",
      gitStatusOk: false,
      gitLogOk: false,
      markerVerified: false,
      createMs: 0,
      verifyMs: 0,
      error: r.reason?.message ?? String(r.reason),
    };
  });

  const corrupted = restores.filter((r) => !r.gitStatusOk || !r.gitLogOk || !r.markerVerified).length;
  const errored = restores.filter((r) => r.error && r.gitStatusOk && r.gitLogOk && r.markerVerified).length;
  // Infra errors: error present but all checks didn't get a chance to run
  const infraErrors = restores.filter((r) => r.error && !r.gitStatusOk && !r.gitLogOk && !r.markerVerified && !isCorruptionError(r)).length;

  // Round summary
  const tag = corrupted > 0 ? "\x1b[31m" : "\x1b[32m";
  console.log(`${tag}  Round ${round + 1}: ${restoreCount} restores / ${corrupted} corrupted / ${infraErrors} errors\x1b[0m`);

  // Timing stats
  const createTimes = restores.filter((r) => r.createMs > 0).map((r) => r.createMs);
  const verifyTimes = restores.filter((r) => r.verifyMs > 0).map((r) => r.verifyMs);
  if (createTimes.length > 0) {
    const avg = (a: number[]) => a.reduce((s, v) => s + v, 0) / a.length;
    dim(`  create: avg=${formatMs(avg(createTimes))} min=${formatMs(Math.min(...createTimes))} max=${formatMs(Math.max(...createTimes))}`);
    dim(`  verify: avg=${formatMs(avg(verifyTimes))} min=${formatMs(Math.min(...verifyTimes))} max=${formatMs(Math.max(...verifyTimes))}`);
  }

  return {
    round,
    sourceSandboxId: setup.sourceSandboxId,
    checkpointId: setup.checkpointId,
    markerSha256: setup.markerSha256,
    expectedGitLog: setup.expectedGitLog,
    setupMs: setup.setupMs,
    restores,
    totalRestores: restoreCount,
    corrupted,
    errored: infraErrors,
  };
}

function isCorruptionError(r: RestoreResult): boolean {
  if (!r.error) return false;
  return r.error.includes("segfault") ||
    r.error.includes("signal 11") ||
    r.error.includes("SHA256 mismatch") ||
    r.error.includes("git log mismatch") ||
    r.error.includes("git status failed");
}

// ── Main ────────────────────────────────────────────────────────────────

async function main() {
  bold("╔══════════════════════════════════════════════════════════╗");
  bold("║     Snapshot Corruption Stress Test (v3)                 ║");
  bold("╚══════════════════════════════════════════════════════════╝");
  console.log();
  dim(`${ROUNDS} rounds × ${RESTORES_PER_ROUND} restores = ${TOTAL_RESTORES} total`);
  dim(`Concurrency: ${CONCURRENCY}  Marker: ${MARKER_SIZE_MB}MB`);
  dim(`API: ${API_URL ?? "(default)"}`);
  console.log();

  const startedAt = new Date().toISOString();
  const startMs = Date.now();

  const rounds: RoundResult[] = [];

  for (let r = 0; r < ROUNDS; r++) {
    const restoreCount = r < ROUNDS - 1
      ? RESTORES_PER_ROUND
      : TOTAL_RESTORES - (ROUNDS - 1) * RESTORES_PER_ROUND; // last round gets remainder
    const round = await runRound(r, restoreCount);
    rounds.push(round);
  }

  const totalMs = Date.now() - startMs;
  const completedAt = new Date().toISOString();

  const totalRestores = rounds.reduce((s, r) => s + r.totalRestores, 0);
  const totalCorrupted = rounds.reduce((s, r) => s + r.corrupted, 0);
  const totalErrored = rounds.reduce((s, r) => s + r.errored, 0);
  const anyCorruption = totalCorrupted > 0;

  // Final report
  console.log();
  bold("══════════════════════════════════════════════════════════");
  bold("  FINAL RESULTS");
  bold("══════════════════════════════════════════════════════════");
  console.log();

  for (const r of rounds) {
    const tag = r.corrupted > 0 ? "\x1b[31m" : "\x1b[32m";
    console.log(`${tag}  Round ${r.round + 1}: ${r.totalRestores} restores / ${r.corrupted} corrupted / ${r.errored} errors  (setup: ${formatMs(r.setupMs)})\x1b[0m`);
  }
  console.log();

  const totalTag = anyCorruption ? "\x1b[31m" : "\x1b[32m";
  console.log(`${totalTag}  TOTAL: ${totalRestores} restores / ${totalCorrupted} corrupted / ${totalErrored} errors\x1b[0m`);
  dim(`Total time: ${formatMs(totalMs)}`);
  console.log();

  // List corruptions
  if (totalCorrupted > 0) {
    bold("  Corruptions:");
    for (const r of rounds) {
      for (const restore of r.restores) {
        if (isCorruptionError(restore) || (!restore.gitStatusOk && restore.sandboxId) || (!restore.markerVerified && restore.sandboxId)) {
          red(`    round ${r.round + 1} restore ${restore.index}: ${restore.sandboxId} — ${restore.error}`);
        }
      }
    }
    console.log();
  }

  // List infra errors (first 10)
  if (totalErrored > 0) {
    bold(`  Infra errors (${totalErrored} total, showing first 10):`);
    let shown = 0;
    for (const r of rounds) {
      for (const restore of r.restores) {
        if (restore.error && !isCorruptionError(restore) && shown < 10) {
          yellow(`    round ${r.round + 1} restore ${restore.index}: ${restore.error}`);
          shown++;
        }
      }
    }
    console.log();
  }

  // JSON report
  const report: TestReport = {
    startedAt,
    completedAt,
    config: {
      totalRestores: TOTAL_RESTORES,
      rounds: ROUNDS,
      restoresPerRound: RESTORES_PER_ROUND,
      concurrency: CONCURRENCY,
      markerSizeMB: MARKER_SIZE_MB,
    },
    rounds,
    summary: {
      totalRestores,
      totalCorrupted,
      totalErrored,
      totalDurationMs: totalMs,
      corruption: anyCorruption,
    },
  };

  if (OUTPUT) {
    writeFileSync(OUTPUT, JSON.stringify(report, null, 2));
    dim(`Report written to ${OUTPUT}`);
  }

  // Exit code
  if (anyCorruption) {
    red(`\nFAILED — ${totalCorrupted} corruption(s) detected`);
    process.exit(1);
  } else if (totalErrored > totalRestores * 0.1) {
    yellow(`\nWARN — 0 corruptions but ${totalErrored} errors (>${Math.round(totalRestores * 0.1)} threshold)`);
    process.exit(2);
  } else {
    green(`\nPASSED — ${totalRestores} restores, 0 corruptions`);
    process.exit(0);
  }
}

main().catch((err) => {
  red(`Fatal: ${err.message}`);
  console.error(err);
  process.exit(1);
});
