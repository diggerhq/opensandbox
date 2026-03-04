/**
 * Stress Tests for Checkpoint System (50GB Dev Environment)
 *
 * 10 tests designed to shake out resource leaks, race conditions, and edge
 * cases in the checkpoint/fork/restore/golden-snapshot pipeline. Sized for
 * the dev environment with ~50GB NVMe at /data.
 *
 * Each test is self-contained: creates its own sandboxes, cleans up after
 * itself, and reports pass/fail + timing.
 *
 * Env vars:
 *   STRESS_ONLY=3        Run only test 3
 *   STRESS_SKIP=6,8      Skip tests 6 and 8
 *
 * Usage:
 *   npx tsx examples/stress-tests.ts
 */

import { Sandbox } from "../src/index";

// ── Formatting helpers ───────────────────────────────────────────────────

function green(msg: string) { console.log(`\x1b[32m✓ ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m✗ ${msg}\x1b[0m`); }
function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }
function cyan(msg: string) { console.log(`\x1b[36m→ ${msg}\x1b[0m`); }
function yellow(msg: string) { console.log(`\x1b[33m⚠ ${msg}\x1b[0m`); }

function sleep(ms: number) {
  return new Promise((r) => globalThis.setTimeout(r, ms));
}

function formatMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

// ── Timing + assertion infrastructure ────────────────────────────────────

let totalPassed = 0;
let totalFailed = 0;

function check(desc: string, condition: boolean, detail?: string): boolean {
  if (condition) {
    green(desc);
    totalPassed++;
  } else {
    red(`${desc}${detail ? ` (${detail})` : ""}`);
    totalFailed++;
  }
  return condition;
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
    await sleep(2000);
  }
  return false;
}

async function killAll(sandboxes: Sandbox[]) {
  await Promise.allSettled(sandboxes.map((sb) => sb.kill().catch(() => {})));
}

// ── Test 1: Max Concurrent Creates ───────────────────────────────────────

async function test1_maxConcurrentCreates() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 1: Max Concurrent Creates (20 sandboxes)  ║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: golden snapshot under load, TAP allocation, parallel boot");
  dim("Disk: ~3GB (reflinked)\n");

  const COUNT = 20;
  const sandboxes: Sandbox[] = [];

  try {
    // Create 20 sandboxes in parallel
    cyan(`Creating ${COUNT} sandboxes simultaneously...`);
    const createStart = Date.now();
    const createPromises = Array.from({ length: COUNT }, (_, i) =>
      timed(() => Sandbox.create({ template: "base", timeout: 120 }))
        .then((r) => ({ index: i, sandbox: r.result, ms: r.ms }))
    );

    const results = await Promise.allSettled(createPromises);
    const createWallClock = Date.now() - createStart;

    let successCount = 0;
    const createTimes: number[] = [];
    for (const r of results) {
      if (r.status === "fulfilled") {
        sandboxes.push(r.value.sandbox);
        createTimes.push(r.value.ms);
        successCount++;
      } else {
        dim(`Create failed: ${r.reason.message}`);
      }
    }

    check(`All ${COUNT} sandboxes created`, successCount === COUNT, `${successCount}/${COUNT}`);
    if (createTimes.length > 0) {
      const sorted = [...createTimes].sort((a, b) => a - b);
      dim(`Create times: min=${formatMs(sorted[0])}, avg=${formatMs(Math.round(createTimes.reduce((s, v) => s + v, 0) / createTimes.length))}, max=${formatMs(sorted[sorted.length - 1])}`);
      dim(`Wall clock: ${formatMs(createWallClock)}`);
    }

    // Run commands on all in parallel
    cyan(`Running echo on all ${sandboxes.length} sandboxes...`);
    const { ms: cmdMs } = await timed(async () => {
      const cmdPromises = sandboxes.map((sb, i) =>
        sb.commands.run(`echo sandbox-${i}`).then((r) => ({ index: i, stdout: r.stdout.trim() }))
      );
      const cmdResults = await Promise.allSettled(cmdPromises);
      let cmdOk = 0;
      for (const r of cmdResults) {
        if (r.status === "fulfilled" && r.value.stdout === `sandbox-${r.value.index}`) cmdOk++;
      }
      check(`All ${sandboxes.length} commands returned correct output`, cmdOk === sandboxes.length, `${cmdOk}/${sandboxes.length}`);
    });
    dim(`All commands completed in ${formatMs(cmdMs)}`);

    // Kill all in parallel
    cyan("Killing all sandboxes...");
    const { ms: killMs } = await timed(() => killAll(sandboxes));
    dim(`All killed in ${formatMs(killMs)}`);

  } finally {
    await killAll(sandboxes);
  }
}

// ── Test 2: Checkpoint Storm ─────────────────────────────────────────────

async function test2_checkpointStorm() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 2: Checkpoint Storm (10 across 2 sandboxes)║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: concurrent checkpoint creation, async pipeline, max limit");
  dim("Disk: ~12GB peak\n");

  const sandboxes: Sandbox[] = [];

  try {
    // Create 2 sandboxes
    cyan("Creating 2 sandboxes...");
    const sb1 = await Sandbox.create({ template: "base", timeout: 300 });
    const sb2 = await Sandbox.create({ template: "base", timeout: 300 });
    sandboxes.push(sb1, sb2);
    dim(`Sandbox A: ${sb1.sandboxId}`);
    dim(`Sandbox B: ${sb2.sandboxId}`);

    // Write 500MB payload to each
    cyan("Writing 500MB payload to each sandbox...");
    await Promise.all([
      sb1.commands.run("dd if=/dev/urandom of=/workspace/payload.bin bs=1M count=500 2>/dev/null", { timeout: 120 }),
      sb2.commands.run("dd if=/dev/urandom of=/workspace/payload.bin bs=1M count=500 2>/dev/null", { timeout: 120 }),
    ]);
    green("Payloads written");

    // Create 5 checkpoints on each — wait for ready between iterations
    // to avoid queueing (each snapshot pauses the VM serially)
    cyan("Creating 5 checkpoints on each sandbox (10 total)...");
    const checkpoints: { sandbox: Sandbox; id: string; name: string; ms: number }[] = [];

    for (let i = 0; i < 5; i++) {
      // Fire both sandboxes in parallel for each iteration
      const [r1, r2] = await Promise.all([
        timed(() => sb1.createCheckpoint(`storm-a-${i}`)),
        timed(() => sb2.createCheckpoint(`storm-b-${i}`)),
      ]);
      checkpoints.push(
        { sandbox: sb1, id: r1.result.id, name: `storm-a-${i}`, ms: r1.ms },
        { sandbox: sb2, id: r2.result.id, name: `storm-b-${i}`, ms: r2.ms },
      );
      dim(`  #${i}: A=${formatMs(r1.ms)}, B=${formatMs(r2.ms)}`);

      // Wait for this pair to be ready before creating the next pair
      // (memory snapshot is serialized per VM — queueing causes timeouts)
      await Promise.all([
        waitForReady(sb1, r1.result.id, 180000),
        waitForReady(sb2, r2.result.id, 180000),
      ]);
    }

    check("All 10 checkpoints created", checkpoints.length === 10);

    // Verify all reached "ready"
    cyan("Verifying all 10 checkpoints are ready...");
    const readyStart = Date.now();
    let readyCount = 0;
    for (const cp of checkpoints) {
      const list = await cp.sandbox.listCheckpoints();
      const found = list.find((c) => c.id === cp.id);
      if (found && found.status === "ready") readyCount++;
      else dim(`  ${cp.name} status=${found?.status ?? "not found"}`);
    }
    const readyMs = Date.now() - readyStart;

    check(`All 10 checkpoints ready`, readyCount === 10, `${readyCount}/10`);
    dim(`Verification took: ${formatMs(readyMs)}`);

    // Delete all checkpoints
    cyan("Deleting all checkpoints...");
    for (const cp of checkpoints) {
      await cp.sandbox.deleteCheckpoint(cp.id).catch(() => {});
    }
    green("All checkpoints deleted");

  } finally {
    await killAll(sandboxes);
  }
}

// ── Test 3: Fork Fan-Out ─────────────────────────────────────────────────

async function test3_forkFanOut() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 3: Fork Fan-Out (8 forks from 1 checkpoint)║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: reflink fan-out, concurrent fork boot, state isolation");
  dim("Disk: ~3GB\n");

  const sandboxes: Sandbox[] = [];
  let checkpointId = "";

  try {
    // Create source sandbox with state
    cyan("Creating source sandbox...");
    const source = await Sandbox.create({ template: "base", timeout: 300 });
    sandboxes.push(source);
    dim(`Source: ${source.sandboxId}`);

    await source.commands.run("echo fork-source > /workspace/marker.txt");
    await source.commands.run("echo 'unique-data-12345' > /workspace/data.txt");
    green("Source state written");

    // Checkpoint
    cyan("Creating checkpoint...");
    const { result: cp } = await timed(() => source.createCheckpoint("fan-out"));
    checkpointId = cp.id;
    const ready = await waitForReady(source, cp.id);
    check("Checkpoint ready", ready);

    if (!ready) throw new Error("Checkpoint not ready, cannot proceed");

    // Fork 8 in parallel
    const FORK_COUNT = 8;
    cyan(`Forking ${FORK_COUNT} sandboxes in parallel...`);
    const forkStart = Date.now();
    const forkPromises = Array.from({ length: FORK_COUNT }, (_, i) =>
      timed(() => Sandbox.createFromCheckpoint(cp.id))
        .then((r) => ({ index: i, sandbox: r.result, ms: r.ms }))
    );

    const forkResults = await Promise.allSettled(forkPromises);
    const forkWallClock = Date.now() - forkStart;

    const forkTimes: number[] = [];
    let forkSuccess = 0;
    for (const r of forkResults) {
      if (r.status === "fulfilled") {
        sandboxes.push(r.value.sandbox);
        forkTimes.push(r.value.ms);
        forkSuccess++;
      } else {
        dim(`Fork failed: ${r.reason.message}`);
      }
    }

    check(`All ${FORK_COUNT} forks created`, forkSuccess === FORK_COUNT, `${forkSuccess}/${FORK_COUNT}`);
    if (forkTimes.length > 0) {
      const sorted = [...forkTimes].sort((a, b) => a - b);
      dim(`Fork times: min=${formatMs(sorted[0])}, avg=${formatMs(Math.round(forkTimes.reduce((s, v) => s + v, 0) / forkTimes.length))}, max=${formatMs(sorted[sorted.length - 1])}`);
      dim(`Wall clock: ${formatMs(forkWallClock)}`);
    }

    // Verify all forks have correct state
    cyan("Verifying fork state...");
    const forks = sandboxes.slice(1); // skip source
    const verifyPromises = forks.map((sb, i) =>
      sb.commands.run("cat /workspace/marker.txt").then((r) => ({ index: i, marker: r.stdout.trim() }))
    );

    const verifyResults = await Promise.allSettled(verifyPromises);
    let verifyOk = 0;
    for (const r of verifyResults) {
      if (r.status === "fulfilled" && r.value.marker === "fork-source") verifyOk++;
    }
    check(`All forks have correct marker`, verifyOk === forks.length, `${verifyOk}/${forks.length}`);

    // Verify isolation: write unique file to each fork, read back
    cyan("Verifying isolation across forks...");
    await Promise.all(forks.map((sb, i) =>
      sb.commands.run(`echo fork-${i} > /workspace/identity.txt`)
    ));

    const isolationPromises = forks.map((sb, i) =>
      sb.commands.run("cat /workspace/identity.txt").then((r) => ({ index: i, content: r.stdout.trim() }))
    );
    const isolationResults = await Promise.all(isolationPromises);
    let isolationOk = 0;
    for (const { index, content } of isolationResults) {
      if (content === `fork-${index}`) isolationOk++;
    }
    check(`All forks are isolated`, isolationOk === forks.length, `${isolationOk}/${forks.length}`);

  } finally {
    if (checkpointId && sandboxes.length > 0) {
      await sandboxes[0].deleteCheckpoint(checkpointId).catch(() => {});
    }
    await killAll(sandboxes);
  }
}

// ── Test 4: Restore Thrash ───────────────────────────────────────────────

async function test4_restoreThrash() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 4: Restore Thrash (20 rapid restores)     ║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: warm restore stability, rapid kill/boot cycles, state correctness");
  dim("Disk: ~3GB\n");

  const sandboxes: Sandbox[] = [];
  const checkpointIds: string[] = [];

  try {
    // Create sandbox with payload
    cyan("Creating sandbox with 500MB payload...");
    const sandbox = await Sandbox.create({ template: "base", timeout: 300 });
    sandboxes.push(sandbox);
    await sandbox.commands.run("dd if=/dev/urandom of=/workspace/payload.bin bs=1M count=500 2>/dev/null", { timeout: 120 });

    // Create checkpoint A with marker=A
    await sandbox.commands.run("echo state-A > /workspace/marker.txt");
    const cpA = await sandbox.createCheckpoint("thrash-A");
    checkpointIds.push(cpA.id);
    const readyA = await waitForReady(sandbox, cpA.id);
    check("Checkpoint A ready", readyA);

    // Create checkpoint B with marker=B
    await sandbox.commands.run("echo state-B > /workspace/marker.txt");
    const cpB = await sandbox.createCheckpoint("thrash-B");
    checkpointIds.push(cpB.id);
    const readyB = await waitForReady(sandbox, cpB.id);
    check("Checkpoint B ready", readyB);

    if (!readyA || !readyB) throw new Error("Checkpoints not ready");

    // Alternate restoring 20 times
    const ITERATIONS = 20;
    cyan(`Running ${ITERATIONS} rapid restores (alternating A/B)...`);
    const restoreTimes: number[] = [];
    const cmdTimes: number[] = [];
    let errors = 0;

    for (let i = 0; i < ITERATIONS; i++) {
      const targetCp = i % 2 === 0 ? cpA.id : cpB.id;
      const expectedMarker = i % 2 === 0 ? "state-A" : "state-B";

      try {
        const { ms: restoreMs } = await timed(() => sandbox.restoreCheckpoint(targetCp));
        restoreTimes.push(restoreMs);

        const { result: cmd, ms: cmdMs } = await timed(() =>
          sandbox.commands.run("cat /workspace/marker.txt")
        );
        cmdTimes.push(cmdMs);

        const marker = cmd.stdout.trim();
        if (marker !== expectedMarker) {
          red(`  #${i + 1}: expected ${expectedMarker}, got ${marker}`);
          errors++;
        } else if (i % 5 === 0) {
          dim(`  #${i + 1}: restore=${formatMs(restoreMs)}, cmd=${formatMs(cmdMs)} ✓`);
        }
      } catch (err: any) {
        red(`  #${i + 1}: ${err.message}`);
        errors++;
      }
    }

    check(`All ${ITERATIONS} restores completed correctly`, errors === 0, `${errors} errors`);

    if (restoreTimes.length > 0) {
      const sorted = [...restoreTimes].sort((a, b) => a - b);
      dim(`Restore: min=${formatMs(sorted[0])}, avg=${formatMs(Math.round(restoreTimes.reduce((s, v) => s + v, 0) / restoreTimes.length))}, max=${formatMs(sorted[sorted.length - 1])}`);
    }
    if (cmdTimes.length > 0) {
      const sorted = [...cmdTimes].sort((a, b) => a - b);
      dim(`First cmd: min=${formatMs(sorted[0])}, avg=${formatMs(Math.round(cmdTimes.reduce((s, v) => s + v, 0) / cmdTimes.length))}, max=${formatMs(sorted[sorted.length - 1])}`);
    }

  } finally {
    for (const cpId of checkpointIds) {
      await sandboxes[0]?.deleteCheckpoint(cpId).catch(() => {});
    }
    await killAll(sandboxes);
  }
}

// ── Test 5: Disk Fill & CoW Divergence ───────────────────────────────────

async function test5_cowDivergence() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 5: Disk Fill & CoW Divergence             ║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: reflink CoW under divergent writes, disk accounting, restore correctness");
  dim("Disk: ~10GB peak\n");

  const sandboxes: Sandbox[] = [];
  const checkpointIds: string[] = [];

  try {
    cyan("Creating sandbox...");
    const sandbox = await Sandbox.create({ template: "base", timeout: 300 });
    sandboxes.push(sandbox);

    // Write 3GB payload
    cyan("Writing 3GB payload...");
    await sandbox.commands.run("dd if=/dev/urandom of=/workspace/payload.bin bs=1M count=3072 2>/dev/null", { timeout: 300 });
    const hash1Cmd = await sandbox.commands.run("md5sum /workspace/payload.bin", { timeout: 300 });
    const hashBefore = hash1Cmd.stdout.trim().split(/\s+/)[0];
    dim(`Original hash: ${hashBefore}`);

    // Checkpoint before diverge
    cyan("Creating checkpoint before-diverge...");
    const cpBefore = await sandbox.createCheckpoint("before-diverge");
    checkpointIds.push(cpBefore.id);
    check("Checkpoint before-diverge created", await waitForReady(sandbox, cpBefore.id));

    // Overwrite first 2GB (triggers CoW on original blocks)
    cyan("Overwriting first 2GB (triggers CoW)...");
    await sandbox.commands.run("dd if=/dev/urandom of=/workspace/payload.bin bs=1M count=2048 conv=notrunc 2>/dev/null", { timeout: 300 });
    const hash2Cmd = await sandbox.commands.run("md5sum /workspace/payload.bin", { timeout: 300 });
    const hashAfter = hash2Cmd.stdout.trim().split(/\s+/)[0];
    dim(`Modified hash: ${hashAfter}`);

    check("Hashes differ after overwrite", hashBefore !== hashAfter);

    // Check disk usage inside VM
    const dfCmd = await sandbox.commands.run("df -h /workspace");
    dim(`Disk usage after CoW divergence:\n    ${dfCmd.stdout.trim().split("\n").join("\n    ")}`);

    // Checkpoint after diverge
    cyan("Creating checkpoint after-diverge...");
    const cpAfter = await sandbox.createCheckpoint("after-diverge");
    checkpointIds.push(cpAfter.id);
    check("Checkpoint after-diverge created", await waitForReady(sandbox, cpAfter.id));

    // Restore to before-diverge, verify original hash
    cyan("Restoring to before-diverge...");
    await sandbox.restoreCheckpoint(cpBefore.id);
    const restoreHash1Cmd = await sandbox.commands.run("md5sum /workspace/payload.bin", { timeout: 300 });
    const restoredHash1 = restoreHash1Cmd.stdout.trim().split(/\s+/)[0];
    check("Restored hash matches original", restoredHash1 === hashBefore, `got ${restoredHash1}`);

    // Restore to after-diverge, verify modified hash
    cyan("Restoring to after-diverge...");
    await sandbox.restoreCheckpoint(cpAfter.id);
    const restoreHash2Cmd = await sandbox.commands.run("md5sum /workspace/payload.bin", { timeout: 300 });
    const restoredHash2 = restoreHash2Cmd.stdout.trim().split(/\s+/)[0];
    check("Restored hash matches modified", restoredHash2 === hashAfter, `got ${restoredHash2}`);

  } finally {
    for (const cpId of checkpointIds) {
      await sandboxes[0]?.deleteCheckpoint(cpId).catch(() => {});
    }
    await killAll(sandboxes);
  }
}

// ── Test 6: Large Workspace Checkpoint (5GB) ─────────────────────────────

async function test6_largeCheckpoint() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 6: Large Workspace Checkpoint (5GB)       ║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: large checkpoint create/fork, S3 upload, data integrity");
  dim("Disk: ~8GB peak\n");

  const sandboxes: Sandbox[] = [];
  let checkpointId = "";

  try {
    cyan("Creating sandbox...");
    const sandbox = await Sandbox.create({ template: "base", timeout: 600 });
    sandboxes.push(sandbox);

    // Write 5GB in 1GB chunks
    cyan("Writing 5GB payload in 1GB chunks...");
    const hashes: string[] = [];
    for (let i = 0; i < 5; i++) {
      const { ms } = await timed(async () => {
        await sandbox.commands.run(`dd if=/dev/urandom of=/workspace/chunk-${i}.bin bs=1M count=1024 2>/dev/null`, { timeout: 300 });
      });
      const hashCmd = await sandbox.commands.run(`md5sum /workspace/chunk-${i}.bin`, { timeout: 60 });
      hashes.push(hashCmd.stdout.trim().split(/\s+/)[0]);
      dim(`  Chunk ${i}: ${formatMs(ms)}, hash=${hashes[i].slice(0, 12)}...`);
    }

    // Create checkpoint
    cyan("Creating checkpoint of 5GB workspace...");
    const { result: cp, ms: createMs } = await timed(() => sandbox.createCheckpoint("large-5gb"));
    checkpointId = cp.id;
    dim(`Checkpoint create API: ${formatMs(createMs)}`);

    // Wait for ready (may take a while for S3 upload)
    cyan("Waiting for checkpoint ready (S3 upload)...");
    const { ms: readyMs } = await timed(async () => {
      const ready = await waitForReady(sandbox, cp.id, 600000); // 10 min timeout
      check("Checkpoint ready", ready);
    });
    dim(`Time to ready: ${formatMs(readyMs)}`);

    // Fork from checkpoint
    cyan("Forking from checkpoint...");
    const { result: fork, ms: forkMs } = await timed(() => Sandbox.createFromCheckpoint(cp.id));
    sandboxes.push(fork);
    dim(`Fork created in ${formatMs(forkMs)}: ${fork.sandboxId}`);

    // Verify all chunk hashes in fork
    cyan("Verifying 5GB data integrity in fork...");
    let hashOk = 0;
    for (let i = 0; i < 5; i++) {
      const hashCmd = await fork.commands.run(`md5sum /workspace/chunk-${i}.bin`, { timeout: 60 });
      const forkHash = hashCmd.stdout.trim().split(/\s+/)[0];
      if (forkHash === hashes[i]) {
        hashOk++;
      } else {
        dim(`  Chunk ${i}: MISMATCH (expected ${hashes[i].slice(0, 12)}, got ${forkHash.slice(0, 12)})`);
      }
    }
    check(`All 5 chunk hashes match in fork`, hashOk === 5, `${hashOk}/5`);

  } finally {
    if (checkpointId && sandboxes.length > 0) {
      await sandboxes[0].deleteCheckpoint(checkpointId).catch(() => {});
    }
    await killAll(sandboxes);
  }
}

// ── Test 7: Rapid Create-Kill Cycle ──────────────────────────────────────

async function test7_rapidCreateKill() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 7: Rapid Create-Kill Cycle (50 iterations)║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: resource leak detection (TAP, disk, FDs)");
  dim("Disk: ~0 (cleaned each cycle)\n");

  const ITERATIONS = 50;
  const createTimes: number[] = [];
  const killTimes: number[] = [];
  let errors = 0;

  // We can't check host disk from the SDK, so we track sandbox-side metrics
  cyan(`Running ${ITERATIONS} create-kill cycles...`);

  for (let i = 0; i < ITERATIONS; i++) {
    try {
      const { result: sb, ms: createMs } = await timed(() =>
        Sandbox.create({ template: "base", timeout: 60 })
      );
      createTimes.push(createMs);

      // Verify it boots
      const cmd = await sb.commands.run("echo alive");
      if (cmd.stdout.trim() !== "alive") {
        errors++;
        dim(`  #${i + 1}: wrong output: ${cmd.stdout.trim()}`);
      }

      const { ms: killMs } = await timed(() => sb.kill());
      killTimes.push(killMs);

      // Progress report every 10 iterations
      if ((i + 1) % 10 === 0) {
        dim(`  ${i + 1}/${ITERATIONS}: create=${formatMs(createMs)}, kill=${formatMs(killMs)}`);
      }
    } catch (err: any) {
      errors++;
      red(`  #${i + 1}: ${err.message}`);
    }
  }

  check(`All ${ITERATIONS} cycles completed without errors`, errors === 0, `${errors} errors`);

  if (createTimes.length > 0) {
    const sorted = [...createTimes].sort((a, b) => a - b);
    dim(`Create: min=${formatMs(sorted[0])}, avg=${formatMs(Math.round(createTimes.reduce((s, v) => s + v, 0) / createTimes.length))}, max=${formatMs(sorted[sorted.length - 1])}`);
  }
  if (killTimes.length > 0) {
    const sorted = [...killTimes].sort((a, b) => a - b);
    dim(`Kill: min=${formatMs(sorted[0])}, avg=${formatMs(Math.round(killTimes.reduce((s, v) => s + v, 0) / killTimes.length))}, max=${formatMs(sorted[sorted.length - 1])}`);
  }

  // Check for timing degradation (last 10 vs first 10)
  if (createTimes.length >= 20) {
    const first10Avg = createTimes.slice(0, 10).reduce((s, v) => s + v, 0) / 10;
    const last10Avg = createTimes.slice(-10).reduce((s, v) => s + v, 0) / 10;
    const ratio = last10Avg / first10Avg;
    check(
      `No timing degradation (last 10 / first 10 ratio)`,
      ratio < 2.0,
      `ratio=${ratio.toFixed(2)} (first10=${formatMs(Math.round(first10Avg))}, last10=${formatMs(Math.round(last10Avg))})`
    );
  }
}

// ── Test 8: Long-Running Stability ───────────────────────────────────────

async function test8_longRunning() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 8: Long-Running Stability (10 VMs, 5 min) ║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: golden VMs stay responsive, no timeout/crash over time");
  dim("Disk: ~2GB\n");

  const VM_COUNT = 10;
  const DURATION_MS = 5 * 60 * 1000; // 5 minutes
  const INTERVAL_MS = 30 * 1000; // 30 seconds
  const sandboxes: Sandbox[] = [];

  try {
    // Create 10 sandboxes
    cyan(`Creating ${VM_COUNT} sandboxes...`);
    const createStart = Date.now();
    const createPromises = Array.from({ length: VM_COUNT }, () =>
      Sandbox.create({ template: "base", timeout: 600 })
    );
    const createResults = await Promise.allSettled(createPromises);
    for (const r of createResults) {
      if (r.status === "fulfilled") sandboxes.push(r.value);
    }
    const createMs = Date.now() - createStart;
    check(`All ${VM_COUNT} sandboxes created`, sandboxes.length === VM_COUNT, `${sandboxes.length}/${VM_COUNT}`);
    dim(`Created in ${formatMs(createMs)}`);

    // Poll every 30 seconds for 5 minutes
    const rounds = Math.ceil(DURATION_MS / INTERVAL_MS);
    let totalFailures = 0;

    for (let round = 1; round <= rounds; round++) {
      if (round > 1) await sleep(INTERVAL_MS);

      cyan(`Round ${round}/${rounds}...`);
      const roundStart = Date.now();
      const cmdPromises = sandboxes.map((sb, i) =>
        sb.commands.run("uptime", { timeout: 10 })
          .then((r) => ({ index: i, ok: true, ms: Date.now() - roundStart }))
          .catch((err) => ({ index: i, ok: false, ms: Date.now() - roundStart, error: err.message }))
      );

      const results = await Promise.allSettled(cmdPromises);
      let roundOk = 0;
      const roundTimes: number[] = [];
      for (const r of results) {
        if (r.status === "fulfilled") {
          if (r.value.ok) {
            roundOk++;
            roundTimes.push(r.value.ms);
          } else {
            dim(`    VM ${(r.value as any).index}: failed - ${(r.value as any).error}`);
          }
        }
      }

      if (roundOk < sandboxes.length) totalFailures += (sandboxes.length - roundOk);
      const avgMs = roundTimes.length > 0 ? Math.round(roundTimes.reduce((s, v) => s + v, 0) / roundTimes.length) : 0;
      dim(`  ${roundOk}/${sandboxes.length} responded, avg=${formatMs(avgMs)}`);
    }

    check(`All VMs stayed responsive (${rounds} rounds)`, totalFailures === 0, `${totalFailures} failures`);

    // Final verification: write + read
    cyan("Final write/read verification...");
    const writePromises = sandboxes.map((sb, i) =>
      sb.files.write("/tmp/stability.txt", `vm-${i}-stable`)
    );
    await Promise.all(writePromises);

    const readPromises = sandboxes.map((sb, i) =>
      sb.files.read("/tmp/stability.txt").then((c) => ({ index: i, ok: c === `vm-${i}-stable` }))
    );
    const readResults = await Promise.all(readPromises);
    const readOk = readResults.filter((r) => r.ok).length;
    check(`All VMs write/read correctly`, readOk === sandboxes.length, `${readOk}/${sandboxes.length}`);

  } finally {
    await killAll(sandboxes);
  }
}

// ── Test 9: Concurrent Checkpoint + Exec ─────────────────────────────────

async function test9_concurrentCheckpointExec() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 9: Concurrent Checkpoint + Exec           ║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: sandbox stays responsive during checkpoint (reflink-only, ~2ms pause)");
  dim("Disk: ~4GB\n");

  const sandboxes: Sandbox[] = [];
  let checkpointId = "";

  try {
    cyan("Creating sandbox with 1GB payload...");
    const sandbox = await Sandbox.create({ template: "base", timeout: 300 });
    sandboxes.push(sandbox);
    await sandbox.commands.run("dd if=/dev/urandom of=/workspace/payload.bin bs=1M count=1024 2>/dev/null", { timeout: 120 });
    await sandbox.commands.run("echo concurrent-test > /workspace/marker.txt");

    // Create checkpoint — the VM pause is ~2ms (reflink only, no memory snapshot).
    // The API returns immediately with status=processing. S3 upload happens async.
    cyan("Starting checkpoint creation...");
    const cp = await sandbox.createCheckpoint("concurrent-test");
    checkpointId = cp.id;
    dim(`Checkpoint initiated: ${cp.id.slice(0, 8)}...`);

    // Run 10 commands immediately — sandbox should stay responsive since
    // checkpoint pause is only ~2ms for reflink copies.
    cyan("Running 10 commands immediately after checkpoint...");
    const cmdResults: { index: number; ok: boolean; ms: number }[] = [];

    for (let i = 0; i < 10; i++) {
      try {
        const { result, ms } = await timed(() =>
          sandbox.commands.run(`echo cmd-${i}`)
        );
        cmdResults.push({ index: i, ok: result.stdout.trim() === `cmd-${i}`, ms });
      } catch (err: any) {
        cmdResults.push({ index: i, ok: false, ms: 0 });
        dim(`  cmd-${i}: failed - ${err.message}`);
      }
    }

    const cmdOk = cmdResults.filter((r) => r.ok).length;
    check(`All 10 commands succeeded during checkpoint`, cmdOk === 10, `${cmdOk}/10`);

    const cmdTimesMs = cmdResults.filter((r) => r.ok).map((r) => r.ms);
    if (cmdTimesMs.length > 0) {
      const avg = Math.round(cmdTimesMs.reduce((s, v) => s + v, 0) / cmdTimesMs.length);
      dim(`Command latency during checkpoint: avg=${formatMs(avg)}`);
    }

    // Wait for checkpoint to finish
    cyan("Waiting for checkpoint to become ready...");
    const { ms: readyMs } = await timed(async () => {
      const ready = await waitForReady(sandbox, cp.id);
      check("Checkpoint completed successfully", ready);
    });
    dim(`Time to ready: ${formatMs(readyMs)}`);

    // Validate checkpoint by forking (cold boot — no memory snapshot)
    cyan("Validating checkpoint by forking...");
    const fork = await Sandbox.createFromCheckpoint(cp.id);
    sandboxes.push(fork);
    const forkMarker = await fork.commands.run("cat /workspace/marker.txt");
    check("Fork has correct state", forkMarker.stdout.trim() === "concurrent-test", forkMarker.stdout.trim());

  } finally {
    if (checkpointId && sandboxes.length > 0) {
      await sandboxes[0].deleteCheckpoint(checkpointId).catch(() => {});
    }
    await killAll(sandboxes);
  }
}

// ── Test 10: Full Lifecycle Chaos ────────────────────────────────────────

async function test10_lifecycleChaos() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Test 10: Full Lifecycle Chaos                  ║");
  bold("╚══════════════════════════════════════════════════╝\n");
  dim("Validates: mixed concurrent operations, realistic load patterns");
  dim("Disk: ~10GB peak\n");

  const SB_COUNT = 3; // sized for dev worker (max_capacity=5, need room for forks)
  const allSandboxes: Sandbox[] = [];
  const checkpointIds: { sandboxIdx: number; id: string }[] = [];

  try {
    // Phase 1: Create sandboxes in parallel
    cyan(`Phase 1: Creating ${SB_COUNT} sandboxes...`);
    const { ms: createMs } = await timed(async () => {
      const results = await Promise.allSettled(
        Array.from({ length: SB_COUNT }, () => Sandbox.create({ template: "base", timeout: 300 }))
      );
      for (const r of results) {
        if (r.status === "fulfilled") allSandboxes.push(r.value);
      }
    });
    check(`${SB_COUNT} sandboxes created`, allSandboxes.length === SB_COUNT, `${allSandboxes.length}/${SB_COUNT}`);
    if (allSandboxes.length < SB_COUNT) throw new Error(`Only created ${allSandboxes.length}/${SB_COUNT} sandboxes`);
    dim(`Created in ${formatMs(createMs)}`);

    // Phase 2: Write 500MB to each
    cyan("Phase 2: Writing 500MB payload to each...");
    const { ms: writeMs } = await timed(async () => {
      await Promise.all(allSandboxes.map((sb, i) =>
        sb.commands.run(`dd if=/dev/urandom of=/workspace/payload.bin bs=1M count=500 2>/dev/null && echo chaos-${i} > /workspace/marker.txt`, { timeout: 120 })
      ));
    });
    dim(`Payloads written in ${formatMs(writeMs)}`);

    // Phase 3: Checkpoint all
    cyan("Phase 3: Creating 1 checkpoint on each...");
    for (let i = 0; i < allSandboxes.length; i++) {
      const cp = await allSandboxes[i].createCheckpoint(`chaos-${i}`);
      checkpointIds.push({ sandboxIdx: i, id: cp.id });
    }

    // Wait for all ready
    cyan("Waiting for all checkpoints ready...");
    let readyCount = 0;
    for (const cp of checkpointIds) {
      if (await waitForReady(allSandboxes[cp.sandboxIdx], cp.id)) readyCount++;
    }
    check(`All ${SB_COUNT} checkpoints ready`, readyCount === SB_COUNT, `${readyCount}/${SB_COUNT}`);

    // Phase 4: Kill 1 sandbox to make room, then fork 2 from checkpoints
    cyan("Phase 4: Killing sandbox 0 to make room for forks...");
    await allSandboxes[0].kill();

    cyan("Phase 4: Forking 2 sandboxes from checkpoints...");
    const fork1 = await Sandbox.createFromCheckpoint(checkpointIds[0].id);
    allSandboxes.push(fork1);
    const fork2 = await Sandbox.createFromCheckpoint(checkpointIds[2].id);
    allSandboxes.push(fork2);

    // Verify fork state
    const fork1Marker = await fork1.commands.run("cat /workspace/marker.txt");
    check("Fork 1 has correct state", fork1Marker.stdout.trim() === "chaos-0", fork1Marker.stdout.trim());
    const fork2Marker = await fork2.commands.run("cat /workspace/marker.txt");
    check("Fork 2 has correct state", fork2Marker.stdout.trim() === "chaos-2", fork2Marker.stdout.trim());

    // Phase 5: Restore 1 sandbox to its checkpoint
    cyan("Phase 5: Restoring sandbox 1...");
    await allSandboxes[1].commands.run("echo modified > /workspace/marker.txt");
    await allSandboxes[1].restoreCheckpoint(checkpointIds[1].id);
    const restored1 = await allSandboxes[1].commands.run("cat /workspace/marker.txt");
    check("Sandbox 1 restored correctly", restored1.stdout.trim() === "chaos-1", restored1.stdout.trim());

    // Phase 6: Verify remaining sandboxes respond (0 killed, 1,2 alive, 3=fork1, 4=fork2)
    cyan("Phase 6: Verifying remaining sandboxes...");
    const remainingIndices = [1, 2, 3, 4]; // indices into allSandboxes (0 killed)
    let respondOk = 0;
    for (const idx of remainingIndices) {
      try {
        const cmd = await allSandboxes[idx].commands.run("echo alive");
        if (cmd.stdout.trim() === "alive") respondOk++;
      } catch {
        dim(`  Sandbox at index ${idx} not responding`);
      }
    }
    check(`All ${remainingIndices.length} remaining sandboxes respond`, respondOk === remainingIndices.length, `${respondOk}/${remainingIndices.length}`);

    // Phase 8: Delete all checkpoints
    cyan("Phase 8: Deleting all checkpoints...");
    for (const cp of checkpointIds) {
      // Use any living sandbox that owns the checkpoint
      const sb = allSandboxes[cp.sandboxIdx];
      await sb.deleteCheckpoint(cp.id).catch(() => {});
    }
    green("All checkpoints deleted");

  } finally {
    await killAll(allSandboxes);
  }
}

// ── Main runner ──────────────────────────────────────────────────────────

interface TestEntry {
  num: number;
  name: string;
  fn: () => Promise<void>;
}

const ALL_TESTS: TestEntry[] = [
  { num: 1, name: "Max Concurrent Creates", fn: test1_maxConcurrentCreates },
  { num: 2, name: "Checkpoint Storm", fn: test2_checkpointStorm },
  { num: 3, name: "Fork Fan-Out", fn: test3_forkFanOut },
  { num: 4, name: "Restore Thrash", fn: test4_restoreThrash },
  { num: 5, name: "Disk Fill & CoW Divergence", fn: test5_cowDivergence },
  { num: 6, name: "Large Workspace Checkpoint (5GB)", fn: test6_largeCheckpoint },
  { num: 7, name: "Rapid Create-Kill Cycle", fn: test7_rapidCreateKill },
  { num: 8, name: "Long-Running Stability", fn: test8_longRunning },
  { num: 9, name: "Concurrent Checkpoint + Exec", fn: test9_concurrentCheckpointExec },
  { num: 10, name: "Full Lifecycle Chaos", fn: test10_lifecycleChaos },
];

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       Stress Test Suite (50GB Dev)               ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  const only = process.env.STRESS_ONLY ? parseInt(process.env.STRESS_ONLY, 10) : 0;
  const skip = new Set(
    (process.env.STRESS_SKIP || "").split(",").filter(Boolean).map((s) => parseInt(s, 10))
  );

  const testsToRun = ALL_TESTS.filter((t) => {
    if (only > 0) return t.num === only;
    return !skip.has(t.num);
  });

  dim(`Running ${testsToRun.length} of ${ALL_TESTS.length} tests`);
  if (only > 0) dim(`STRESS_ONLY=${only}`);
  if (skip.size > 0) dim(`STRESS_SKIP=${Array.from(skip).join(",")}`);

  const testResults: { name: string; passed: number; failed: number; ms: number; error?: string }[] = [];

  for (let ti = 0; ti < testsToRun.length; ti++) {
    const test = testsToRun[ti];

    // Let worker finish cleanup from previous test
    if (ti > 0) await sleep(3000);

    const beforePassed = totalPassed;
    const beforeFailed = totalFailed;
    const start = Date.now();

    try {
      await test.fn();
      testResults.push({
        name: `${test.num}. ${test.name}`,
        passed: totalPassed - beforePassed,
        failed: totalFailed - beforeFailed,
        ms: Date.now() - start,
      });
    } catch (err: any) {
      red(`Test ${test.num} crashed: ${err.message}`);
      if (err.stack) dim(err.stack);
      totalFailed++;
      testResults.push({
        name: `${test.num}. ${test.name}`,
        passed: totalPassed - beforePassed,
        failed: totalFailed - beforeFailed,
        ms: Date.now() - start,
        error: err.message,
      });
    }
  }

  // ── Summary ────────────────────────────────────────────────────────────
  console.log();
  bold("╔══════════════════════════════════════════════════╗");
  bold("║       Summary                                    ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  const nameWidth = Math.max(...testResults.map((r) => r.name.length), 4);
  for (const r of testResults) {
    const status = r.failed === 0 ? "\x1b[32mPASS\x1b[0m" : "\x1b[31mFAIL\x1b[0m";
    const line = `  ${status}  ${r.name.padEnd(nameWidth)}  ${formatMs(r.ms).padStart(8)}  (${r.passed}✓ ${r.failed}✗)`;
    console.log(line);
    if (r.error) dim(`        Error: ${r.error}`);
  }

  console.log();
  bold(`Total: ${totalPassed} passed, ${totalFailed} failed`);
  const totalMs = testResults.reduce((s, r) => s + r.ms, 0);
  dim(`Total time: ${formatMs(totalMs)}`);
  console.log();

  if (totalFailed > 0) process.exit(1);
}

main().catch((err) => {
  console.error("Fatal:", err);
  process.exit(1);
});
