/**
 * Checkpoint Patch System Test
 *
 * Tests:
 *   1. Create a sandbox, checkpoint it, create a patch
 *   2. Fork a sandbox — verify patch is applied on boot
 *   3. Create a second patch, hibernate fork, wake it — verify both patches applied
 *   4. List patches and verify ordering
 *   5. Bad patch blocks the chain
 *   6. Delete the bad patch — recovery on next wake
 *   7. Delete a patch and verify list updates
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

async function waitForSandboxReady(sandbox: Sandbox, timeoutMs = 60000): Promise<boolean> {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      const result = await sandbox.commands.run("echo ready", { timeout: 10 });
      if (result.stdout.trim() === "ready") return true;
    } catch {
      // sandbox not ready yet
    }
    await sleep(3000);
  }
  return false;
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

    await source.commands.run("echo 'python3.10' > /root/python-version.txt");
    const initial = await source.commands.run("cat /root/python-version.txt");
    check("Initial state set", initial.stdout.trim() === "python3.10");

    const cp = await source.createCheckpoint("patch-base");
    dim(`Checkpoint ID: ${cp.id}`);

    dim("Waiting for checkpoint to be ready...");
    const ready = await waitForCheckpointReady(source, cp.id);
    check("Checkpoint is ready", ready);
    if (!ready) throw new Error("Checkpoint not ready, cannot proceed");
    console.log();

    // ── Test 1: Create a patch ──────────────────────────────────
    bold("--- Test 1: Create a patch ---\n");

    const patchResult = await Sandbox.createCheckpointPatch(cp.id, {
      script: "echo 'python3.11' > /root/python-version.txt && echo 'patched' > /root/patch-applied.txt",
      description: "Upgrade Python version marker",
    });

    check("Patch created", patchResult.patch.id !== undefined);
    check("Patch sequence is 1", patchResult.patch.sequence === 1);
    dim(`Patch ID: ${patchResult.patch.id}`);
    console.log();

    // ── Test 2: Fork sandbox — patch applied on boot ────────────
    bold("--- Test 2: Fork sandbox — patch applied on boot ---\n");

    const fork1 = await Sandbox.createFromCheckpoint(cp.id, { timeout: 600 });
    sandboxes.push(fork1);
    green(`Fork 1 created: ${fork1.sandboxId}`);

    dim("Waiting for fork 1 to be ready...");
    const fork1Ready = await waitForSandboxReady(fork1);
    check("Fork 1 is ready", fork1Ready);
    if (!fork1Ready) throw new Error("Fork 1 not ready, cannot proceed");

    // Give patches a moment to complete after sandbox is accepting commands
    await sleep(5000);

    const f1Patched = await fork1.commands.run("cat /root/python-version.txt");
    check("Fork 1 has patched state (applied on boot)", f1Patched.stdout.trim() === "python3.11", `got: ${f1Patched.stdout.trim()}`);

    const f1Marker = await fork1.commands.run("cat /root/patch-applied.txt");
    check("Fork 1 has patch marker", f1Marker.stdout.trim() === "patched");
    console.log();

    // ── Test 3: Second patch + hibernate/wake ───────────────────
    bold("--- Test 3: Second patch applied after hibernate + wake ---\n");

    const patch2Result = await Sandbox.createCheckpointPatch(cp.id, {
      script: "echo 'security-fix-applied' > /root/security-patch.txt",
      description: "Security hotfix",
    });

    check("Second patch created", patch2Result.patch.id !== undefined);
    check("Patch sequence is 2", patch2Result.patch.sequence === 2);

    const f1Security = await fork1.commands.run("test -f /root/security-patch.txt && echo exists || echo missing");
    check("Patch not applied to running sandbox", f1Security.stdout.trim() === "missing");

    dim("Hibernating fork 1...");
    await fork1.hibernate();
    green("Fork 1 hibernated");

    await sleep(3000);

    dim("Waking fork 1...");
    await fork1.wake({ timeout: 600 });
    green("Fork 1 woken");

    dim("Waiting for fork 1 to be ready after wake...");
    const fork1WakeReady = await waitForSandboxReady(fork1);
    check("Fork 1 ready after wake", fork1WakeReady);
    await sleep(5000);

    const f1SecurityAfterWake = await fork1.commands.run("cat /root/security-patch.txt");
    check("Second patch applied after wake", f1SecurityAfterWake.stdout.trim() === "security-fix-applied", `got: ${f1SecurityAfterWake.stdout.trim()}`);

    const f1PythonAfterWake = await fork1.commands.run("cat /root/python-version.txt");
    check("Prior patch state preserved after wake", f1PythonAfterWake.stdout.trim() === "python3.11", `got: ${f1PythonAfterWake.stdout.trim()}`);
    console.log();

    // ── Test 4: List patches ─────────────────────────────────────
    bold("--- Test 4: List patches ---\n");

    const patches = await Sandbox.listCheckpointPatches(cp.id);
    check("2 patches listed", patches.length === 2, `got ${patches.length}`);
    check("First patch has sequence 1", patches[0]?.sequence === 1);
    check("Second patch has sequence 2", patches[1]?.sequence === 2);
    console.log();

    // ── Test 5: Bad patch blocks the chain ───────────────────────
    bold("--- Test 5: Bad patch blocks the chain ---\n");

    const badPatch = await Sandbox.createCheckpointPatch(cp.id, {
      script: "exit 1",
      description: "Intentionally failing patch",
    });

    check("Bad patch created", badPatch.patch.id !== undefined);
    check("Bad patch sequence is 3", badPatch.patch.sequence === 3);
    dim(`Bad patch ID: ${badPatch.patch.id}`);

    // Add a 4th patch after the bad one
    const patch4Result = await Sandbox.createCheckpointPatch(cp.id, {
      script: "echo 'post-bad' > /root/patch4.txt",
      description: "Patch after the bad one",
    });
    check("Patch 4 created (after bad)", patch4Result.patch.sequence === 4);

    // Fork — bad patch should block patch 4 from applying
    const fork2 = await Sandbox.createFromCheckpoint(cp.id, { timeout: 600 });
    sandboxes.push(fork2);
    green(`Fork 2 created: ${fork2.sandboxId}`);

    dim("Waiting for fork 2 to be ready...");
    const fork2Ready = await waitForSandboxReady(fork2);
    check("Fork 2 is ready", fork2Ready);
    if (!fork2Ready) throw new Error("Fork 2 not ready, cannot proceed");
    await sleep(5000);

    // Patches 1+2 should apply, patch 3 fails, patch 4 never runs
    const f2Patched = await fork2.commands.run("cat /root/python-version.txt");
    check("Good patches applied before bad one", f2Patched.stdout.trim() === "python3.11", `got: ${f2Patched.stdout.trim()}`);

    const f2Security = await fork2.commands.run("cat /root/security-patch.txt");
    check("Patch 2 applied", f2Security.stdout.trim() === "security-fix-applied", `got: ${f2Security.stdout.trim()}`);

    const f2Patch4 = await fork2.commands.run("test -f /root/patch4.txt && echo exists || echo missing");
    check("Patch 4 blocked by bad patch 3", f2Patch4.stdout.trim() === "missing");
    console.log();

    // ── Test 6: Delete bad patch — recovery on wake ──────────────
    bold("--- Test 6: Delete bad patch — recovery on wake ---\n");

    dim(`Deleting bad patch ${badPatch.patch.id}...`);
    await Sandbox.deleteCheckpointPatch(cp.id, badPatch.patch.id);
    green("Bad patch deleted");

    // Verify it's gone from the list
    const patchesAfterDelete = await Sandbox.listCheckpointPatches(cp.id);
    check("3 patches remaining after delete", patchesAfterDelete.length === 3, `got ${patchesAfterDelete.length}`);
    const sequences = patchesAfterDelete.map((p) => p.sequence);
    check("Remaining sequences are 1, 2, 4", JSON.stringify(sequences) === "[1,2,4]", `got ${JSON.stringify(sequences)}`);

    // Hibernate fork2 and wake — patch 4 should now apply (bad patch 3 is gone)
    dim("Hibernating fork 2...");
    await fork2.hibernate();
    green("Fork 2 hibernated");

    await sleep(3000);

    dim("Waking fork 2...");
    await fork2.wake({ timeout: 600 });
    green("Fork 2 woken");

    dim("Waiting for fork 2 to be ready after wake...");
    const fork2WakeReady = await waitForSandboxReady(fork2);
    check("Fork 2 ready after wake", fork2WakeReady);
    await sleep(5000);

    const f2Patch4AfterWake = await fork2.commands.run("cat /root/patch4.txt");
    check("Patch 4 applied after deleting bad patch and waking", f2Patch4AfterWake.stdout.trim() === "post-bad", `got: ${f2Patch4AfterWake.stdout.trim()}`);

    // Prior state still intact
    const f2PythonAfterWake = await fork2.commands.run("cat /root/python-version.txt");
    check("All prior patches preserved", f2PythonAfterWake.stdout.trim() === "python3.11", `got: ${f2PythonAfterWake.stdout.trim()}`);
    console.log();

    // ── Test 7: Delete is idempotent ─────────────────────────────
    bold("--- Test 7: Delete is idempotent ---\n");

    // Deleting the same patch again should not throw
    let deleteAgainOk = true;
    try {
      await Sandbox.deleteCheckpointPatch(cp.id, badPatch.patch.id);
    } catch {
      deleteAgainOk = false;
    }
    check("Deleting already-deleted patch does not throw", deleteAgainOk);
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
