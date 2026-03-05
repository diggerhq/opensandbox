/**
 * Checkpoint Patch System Test
 *
 * Tests:
 *   1. Create a sandbox, checkpoint it, fork 2 sandboxes from it
 *   2. Create a "hot" patch on the checkpoint (should exec on running sandboxes)
 *   3. Verify the patch was applied to both running forks
 *   4. Create a third fork — verify existing patches are applied on boot
 *   5. Create an "on_wake" patch
 *   6. Hibernate a fork, wake it, verify on_wake patch was applied
 *   7. List patches and verify ordering
 *   8. Verify patch failure handling (bad script)
 *
 * Usage:
 *   npx tsx examples/test-checkpoint-patches.ts
 *
 * Environment:
 *   OPENCOMPUTER_API_URL  (default: https://app.opencomputer.dev)
 *   OPENCOMPUTER_API_KEY  (required)
 */

import { Sandbox } from "../src/index";

function green(msg: string) { console.log(`\x1b[32m\u2713 ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m\u2717 ${msg}\x1b[0m`); }
function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }

let passed = 0;
let failed = 0;

function check(desc: string, condition: boolean, detail?: string) {
  if (condition) {
    green(desc);
    passed++;
  } else {
    red(`${desc}${detail ? ` (${detail})` : ""}`);
    failed++;
  }
}

async function sleep(ms: number) {
  return new Promise((r) => globalThis.setTimeout(r, ms));
}

async function waitForCheckpointReady(sandbox: Sandbox, checkpointId: string, timeoutMs = 60000): Promise<boolean> {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const checkpoints = await sandbox.listCheckpoints();
    const cp = checkpoints.find((c) => c.id === checkpointId);
    if (cp && cp.status === "ready") return true;
    if (cp && cp.status !== "processing") return false;
    await sleep(2000);
  }
  return false;
}

async function main() {
  bold("\n====================================================");
  bold("       Checkpoint Patch System Test");
  bold("====================================================\n");

  const sandboxes: Sandbox[] = [];

  try {
    // ── Setup: Create source sandbox with initial state ──────────
    bold("--- Setup: Create source sandbox and checkpoint ---\n");

    const source = await Sandbox.create({ template: "base", timeout: 600 });
    sandboxes.push(source);
    green(`Created source sandbox: ${source.sandboxId}`);

    // Install a marker to verify patch effects
    await source.commands.run("echo 'original' > /workspace/patch-marker.txt");
    await source.commands.run("echo 'python3.10' > /workspace/python-version.txt");
    const initial = await source.commands.run("cat /workspace/python-version.txt");
    check("Initial state set", initial.stdout.trim() === "python3.10");

    // Create checkpoint
    const cp = await source.createCheckpoint("patch-base");
    dim(`Checkpoint ID: ${cp.id}`);

    dim("Waiting for checkpoint to be ready...");
    const ready = await waitForCheckpointReady(source, cp.id);
    check("Checkpoint is ready", ready);
    if (!ready) throw new Error("Checkpoint not ready, cannot proceed");
    console.log();

    // ── Test 1: Fork 2 sandboxes from checkpoint ──────────────────
    bold("--- Test 1: Fork sandboxes from checkpoint ---\n");

    const fork1 = await Sandbox.createFromCheckpoint(cp.id, { timeout: 600 });
    sandboxes.push(fork1);
    green(`Fork 1 created: ${fork1.sandboxId}`);

    const fork2 = await Sandbox.createFromCheckpoint(cp.id, { timeout: 600 });
    sandboxes.push(fork2);
    green(`Fork 2 created: ${fork2.sandboxId}`);

    // Wait for forks to boot
    await sleep(5000);

    // Verify forks have the original state
    const f1State = await fork1.commands.run("cat /workspace/python-version.txt");
    check("Fork 1 has original state", f1State.stdout.trim() === "python3.10");
    const f2State = await fork2.commands.run("cat /workspace/python-version.txt");
    check("Fork 2 has original state", f2State.stdout.trim() === "python3.10");
    console.log();

    // ── Test 2: Create a "hot" patch ─────────────────────────────
    bold("--- Test 2: Create hot patch (should apply to running forks) ---\n");

    const patchResult = await Sandbox.createCheckpointPatch(cp.id, {
      script: "echo 'python3.11' > /workspace/python-version.txt && echo 'patched' > /workspace/patch-applied.txt",
      description: "Upgrade Python version marker",
      strategy: "hot",
    });

    check("Patch created", patchResult.patch.id !== undefined);
    check("Patch sequence is 1", patchResult.patch.sequence === 1);
    check("Patch strategy is hot", patchResult.patch.strategy === "hot");
    dim(`Patch ID: ${patchResult.patch.id}`);
    dim(`Results: total=${patchResult.results.total}, patched=${patchResult.results.runningPatched}, failed=${patchResult.results.runningFailed}, queued=${patchResult.results.hibernatedQueued}`);

    // The source sandbox is not forked from the checkpoint, so only fork1 and fork2 should be patched
    check("Running sandboxes were patched", patchResult.results.runningPatched >= 2, `got ${patchResult.results.runningPatched}`);
    check("No failures", patchResult.results.runningFailed === 0, `got ${patchResult.results.runningFailed}`);
    console.log();

    // ── Test 3: Verify patch was applied ─────────────────────────
    bold("--- Test 3: Verify patch applied to running forks ---\n");

    // Small delay for exec to complete
    await sleep(2000);

    const f1Patched = await fork1.commands.run("cat /workspace/python-version.txt");
    check("Fork 1 has patched state", f1Patched.stdout.trim() === "python3.11", `got: ${f1Patched.stdout.trim()}`);

    const f1Marker = await fork1.commands.run("cat /workspace/patch-applied.txt");
    check("Fork 1 has patch marker", f1Marker.stdout.trim() === "patched");

    const f2Patched = await fork2.commands.run("cat /workspace/python-version.txt");
    check("Fork 2 has patched state", f2Patched.stdout.trim() === "python3.11", `got: ${f2Patched.stdout.trim()}`);
    console.log();

    // ── Test 4: New fork gets existing patches applied ───────────
    bold("--- Test 4: New fork gets existing patches on boot ---\n");

    const fork3 = await Sandbox.createFromCheckpoint(cp.id, { timeout: 600 });
    sandboxes.push(fork3);
    green(`Fork 3 created: ${fork3.sandboxId}`);

    // Wait for boot + patch application
    await sleep(8000);

    const f3Patched = await fork3.commands.run("cat /workspace/python-version.txt");
    check("Fork 3 has patched state (applied on boot)", f3Patched.stdout.trim() === "python3.11", `got: ${f3Patched.stdout.trim()}`);

    const f3Marker = await fork3.commands.run("cat /workspace/patch-applied.txt");
    check("Fork 3 has patch marker", f3Marker.stdout.trim() === "patched", `got: ${f3Marker.stdout.trim()}`);
    console.log();

    // ── Test 5: Create "on_wake" patch ───────────────────────────
    bold("--- Test 5: Create on_wake patch ---\n");

    const patch2Result = await Sandbox.createCheckpointPatch(cp.id, {
      script: "echo 'security-fix-applied' > /workspace/security-patch.txt",
      description: "Security hotfix",
      strategy: "on_wake",
    });

    check("on_wake patch created", patch2Result.patch.id !== undefined);
    check("Patch sequence is 2", patch2Result.patch.sequence === 2);
    check("on_wake patch did NOT exec on running sandboxes", patch2Result.results.runningPatched === 0);
    dim(`Hibernated queued: ${patch2Result.results.hibernatedQueued}`);
    console.log();

    // Verify on_wake patch was NOT applied to running fork1
    const f1Security = await fork1.commands.run("test -f /workspace/security-patch.txt && echo exists || echo missing");
    check("on_wake patch not applied to running sandbox", f1Security.stdout.trim() === "missing");
    console.log();

    // ── Test 6: Hibernate + wake → on_wake patch applied ─────────
    bold("--- Test 6: Hibernate and wake fork (patches applied on wake) ---\n");

    dim("Hibernating fork 1...");
    await fork1.hibernate();
    green("Fork 1 hibernated");

    await sleep(3000);

    dim("Waking fork 1...");
    await fork1.wake({ timeout: 600 });
    green("Fork 1 woken");

    // Wait for post-wake patching
    await sleep(5000);

    const f1SecurityAfterWake = await fork1.commands.run("cat /workspace/security-patch.txt");
    check("on_wake patch applied after wake", f1SecurityAfterWake.stdout.trim() === "security-fix-applied", `got: ${f1SecurityAfterWake.stdout.trim()}`);

    // Verify prior patches still present
    const f1PythonAfterWake = await fork1.commands.run("cat /workspace/python-version.txt");
    check("Prior patch state preserved after wake", f1PythonAfterWake.stdout.trim() === "python3.11", `got: ${f1PythonAfterWake.stdout.trim()}`);
    console.log();

    // ── Test 7: List patches ─────────────────────────────────────
    bold("--- Test 7: List patches ---\n");

    const patches = await Sandbox.listCheckpointPatches(cp.id);
    check("2 patches listed", patches.length === 2, `got ${patches.length}`);
    check("First patch has sequence 1", patches[0]?.sequence === 1);
    check("Second patch has sequence 2", patches[1]?.sequence === 2);
    check("First patch is hot", patches[0]?.strategy === "hot");
    check("Second patch is on_wake", patches[1]?.strategy === "on_wake");
    console.log();

    // ── Test 8: Bad patch (should fail gracefully) ───────────────
    bold("--- Test 8: Bad patch script ---\n");

    const badPatch = await Sandbox.createCheckpointPatch(cp.id, {
      script: "exit 1",
      description: "Intentionally failing patch",
      strategy: "hot",
    });

    check("Bad patch created", badPatch.patch.id !== undefined);
    check("Bad patch sequence is 3", badPatch.patch.sequence === 3);
    // Running sandboxes should report failures
    check("Running sandboxes reported failures", badPatch.results.runningFailed > 0, `failed=${badPatch.results.runningFailed}`);
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    // Cleanup
    bold("--- Cleanup ---\n");
    for (const sb of sandboxes) {
      try {
        await sb.kill();
        green(`Killed ${sb.sandboxId}`);
      } catch {
        /* best effort */
      }
    }
  }

  // --- Summary ---
  console.log();
  bold("========================================");
  bold(` Results: ${passed} passed, ${failed} failed`);
  bold("========================================\n");
  if (failed > 0) process.exit(1);
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
