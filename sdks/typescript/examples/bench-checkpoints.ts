/**
 * Checkpoint Benchmark
 *
 * Measures wall-clock timing of each checkpoint operation:
 *   1. Create checkpoint (pause + drive copy + resume + S3 upload)
 *   2. List checkpoints
 *   3. Restore checkpoint (kill VM + copy drives + cold boot)
 *   4. Fork from checkpoint (new sandbox from checkpoint drives)
 *   5. Delete checkpoint
 *
 * Each operation is run multiple times and min/avg/max are reported.
 *
 * Usage:
 *   npx tsx examples/bench-checkpoints.ts
 */

import { Sandbox } from "../src/index";

function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m${msg}\x1b[0m`); }

async function sleep(ms: number) {
  return new Promise((r) => globalThis.setTimeout(r, ms));
}

interface Timing {
  label: string;
  samples: number[];
}

function stats(t: Timing) {
  const sorted = [...t.samples].sort((a, b) => a - b);
  const min = sorted[0];
  const max = sorted[sorted.length - 1];
  const avg = Math.round(t.samples.reduce((s, v) => s + v, 0) / t.samples.length);
  const median = sorted[Math.floor(sorted.length / 2)];
  return { min, max, avg, median };
}

function formatMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

function printTable(timings: Timing[]) {
  const labelWidth = Math.max(...timings.map((t) => t.label.length), 9);
  const header = `${"Operation".padEnd(labelWidth)}  ${"Runs".padStart(4)}  ${"Min".padStart(8)}  ${"Avg".padStart(8)}  ${"Median".padStart(8)}  ${"Max".padStart(8)}`;
  const sep = "─".repeat(header.length);

  console.log();
  bold(sep);
  bold(header);
  bold(sep);

  for (const t of timings) {
    const s = stats(t);
    const row = `${t.label.padEnd(labelWidth)}  ${String(t.samples.length).padStart(4)}  ${formatMs(s.min).padStart(8)}  ${formatMs(s.avg).padStart(8)}  ${formatMs(s.median).padStart(8)}  ${formatMs(s.max).padStart(8)}`;
    console.log(row);
  }
  bold(sep);
  console.log();
}

async function timed<T>(fn: () => Promise<T>): Promise<{ result: T; ms: number }> {
  const start = Date.now();
  const result = await fn();
  return { result, ms: Date.now() - start };
}

async function waitForReady(sandbox: Sandbox, checkpointId: string, timeoutMs = 120000): Promise<boolean> {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const list = await sandbox.listCheckpoints();
    const cp = list.find((c) => c.id === checkpointId);
    if (cp && cp.status === "ready") return true;
    if (cp && cp.status !== "processing") return false;
    await sleep(1000);
  }
  return false;
}

const ITERATIONS = 3;

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       Checkpoint Benchmark                       ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  dim(`Iterations per operation: ${ITERATIONS}`);
  console.log();

  const timings: Timing[] = [];
  const createSbTiming: Timing = { label: "Create sandbox", samples: [] };
  const createTiming: Timing = { label: "Create checkpoint", samples: [] };
  const listTiming: Timing = { label: "List checkpoints", samples: [] };
  const restoreTiming: Timing = { label: "Restore (in-place)", samples: [] };
  const forkTiming: Timing = { label: "Fork from checkpoint", samples: [] };
  const deleteTiming: Timing = { label: "Delete checkpoint", samples: [] };
  const firstCmdTiming: Timing = { label: "First cmd after restore", samples: [] };

  timings.push(createSbTiming, createTiming, listTiming, restoreTiming, firstCmdTiming, forkTiming, deleteTiming);

  const sandboxes: Sandbox[] = [];

  try {
    // ── Benchmark: Create sandbox ─────────────────────────────────
    bold("━━━ Create sandbox ━━━\n");

    let sandbox!: Sandbox;
    for (let i = 0; i < ITERATIONS; i++) {
      const { result: sb, ms } = await timed(() =>
        Sandbox.create({ template: "base", timeout: 300 }),
      );
      createSbTiming.samples.push(ms);
      sandboxes.push(sb);
      dim(`#${i + 1}: ${formatMs(ms)} (sandbox=${sb.sandboxId})`);
      if (i === 0) sandbox = sb;
      // Kill extra sandboxes — we only need the first for remaining benchmarks
      if (i > 0) sb.kill().catch(() => {});
    }
    console.log();

    // ── Setup ──────────────────────────────────────────────────────
    bold("━━━ Setup ━━━\n");
    dim(`Using sandbox ${sandbox.sandboxId} for checkpoint benchmarks`);

    // Write payload data to workspace
    const payloadMB = parseInt(process.env.BENCH_PAYLOAD_MB || "10240", 10);
    const payloadGB = payloadMB / 1024;
    dim(`Writing ${payloadMB >= 1024 ? payloadGB + " GB" : payloadMB + " MB"} payload...`);
    const { ms: writeMs } = await timed(async () => {
      if (payloadMB >= 1024) {
        // Write in 1GB chunks for large payloads
        const chunks = Math.ceil(payloadMB / 1024);
        for (let i = 0; i < chunks; i++) {
          await sandbox.commands.run(`dd if=/dev/zero of=/workspace/payload-${i}.bin bs=1M count=1024 2>/dev/null`, { timeout: 300 });
          dim(`  chunk ${i + 1}/${chunks} written`);
        }
      } else {
        await sandbox.commands.run(`dd if=/dev/zero of=/workspace/payload.bin bs=1M count=${payloadMB} 2>/dev/null`, { timeout: 300 });
      }
    });
    await sandbox.commands.run("echo bench-state > /workspace/marker.txt");
    dim(`Wrote ${payloadMB >= 1024 ? payloadGB + " GB" : payloadMB + " MB"} payload + marker file (${formatMs(writeMs)})`);
    console.log();

    // ── Benchmark: Create checkpoint ───────────────────────────────
    bold("━━━ Create checkpoint ━━━\n");

    const checkpointIds: string[] = [];
    for (let i = 0; i < ITERATIONS; i++) {
      const name = `bench-cp-${i}`;
      const { result: cp, ms } = await timed(() => sandbox.createCheckpoint(name));
      createTiming.samples.push(ms);
      checkpointIds.push(cp.id);
      dim(`#${i + 1}: ${formatMs(ms)} (id=${cp.id.slice(0, 8)}…, status=${cp.status})`);

      // Wait for ready before next iteration (so S3 upload doesn't compete)
      await waitForReady(sandbox, cp.id);
    }
    console.log();

    // ── Benchmark: List checkpoints ────────────────────────────────
    bold("━━━ List checkpoints ━━━\n");

    for (let i = 0; i < ITERATIONS; i++) {
      const { result: list, ms } = await timed(() => sandbox.listCheckpoints());
      listTiming.samples.push(ms);
      dim(`#${i + 1}: ${formatMs(ms)} (${list.length} checkpoints)`);
    }
    console.log();

    // ── Benchmark: Restore (in-place revert) ───────────────────────
    bold("━━━ Restore (in-place revert) ━━━\n");

    for (let i = 0; i < ITERATIONS; i++) {
      // Modify state before each restore so we can verify it works
      await sandbox.commands.run(`echo pre-restore-${i} > /workspace/marker.txt`);

      const cpId = checkpointIds[0]; // always restore to the first checkpoint
      const { ms: restoreMs } = await timed(() => sandbox.restoreCheckpoint(cpId));
      restoreTiming.samples.push(restoreMs);

      // Measure time to first command after restore
      const { result: cmd, ms: cmdMs } = await timed(() =>
        sandbox.commands.run("cat /workspace/marker.txt"),
      );
      firstCmdTiming.samples.push(cmdMs);

      const marker = cmd.stdout.trim();
      const ok = marker === "bench-state" ? "✓" : `✗ (got: ${marker})`;
      dim(`#${i + 1}: restore=${formatMs(restoreMs)}, first_cmd=${formatMs(cmdMs)} ${ok}`);
    }
    console.log();

    // ── Benchmark: Fork from checkpoint ────────────────────────────
    bold("━━━ Fork from checkpoint ━━━\n");

    for (let i = 0; i < ITERATIONS; i++) {
      const cpId = checkpointIds[0];
      const { result: forked, ms } = await timed(() => Sandbox.createFromCheckpoint(cpId));
      forkTiming.samples.push(ms);
      sandboxes.push(forked);

      // Verify the fork has correct state
      const cmd = await forked.commands.run("cat /workspace/marker.txt");
      const marker = cmd.stdout.trim();
      const ok = marker === "bench-state" ? "✓" : `✗ (got: ${marker})`;
      dim(`#${i + 1}: ${formatMs(ms)} (sandbox=${forked.sandboxId}) ${ok}`);
    }
    console.log();

    // ── Benchmark: Delete checkpoint ───────────────────────────────
    bold("━━━ Delete checkpoint ━━━\n");

    for (let i = 0; i < ITERATIONS; i++) {
      const cpId = checkpointIds[i];
      const { ms } = await timed(() => sandbox.deleteCheckpoint(cpId));
      deleteTiming.samples.push(ms);
      dim(`#${i + 1}: ${formatMs(ms)}`);
    }
    console.log();

    // ── Results ────────────────────────────────────────────────────
    bold("━━━ Results ━━━");
    printTable(timings);

  } catch (err: any) {
    red(`Fatal: ${err.message}`);
    if (err.stack) dim(err.stack);
  } finally {
    bold("━━━ Cleanup ━━━\n");
    for (const sb of sandboxes) {
      try {
        await sb.kill();
        dim(`Killed ${sb.sandboxId}`);
      } catch { /* best effort */ }
    }
  }
}

main().catch((err) => {
  console.error("Fatal:", err);
  process.exit(1);
});
