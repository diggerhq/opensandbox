/**
 * Checkpoint Feature Test
 *
 * Tests:
 *   1. Create a named checkpoint of a running sandbox
 *   2. List checkpoints
 *   3. Create a second checkpoint with different state
 *   4. Restore to first checkpoint (in-place revert)
 *   5. Verify state was reverted
 *   6. Fork a new sandbox from a checkpoint
 *   7. Verify forked sandbox has checkpoint state
 *   8. Delete a checkpoint
 *   9. Duplicate name is rejected
 *  10. Max 10 checkpoint limit
 *
 * Usage:
 *   npx tsx examples/test-checkpoints.ts
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
    if (cp && cp.status !== "processing") return false; // failed or other
    await sleep(2000);
  }
  return false;
}

async function main() {
  bold("\n\u2554\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2557");
  bold("\u2551       Checkpoint Feature Test                    \u2551");
  bold("\u255a\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u255d\n");

  let sandbox: Sandbox | null = null;
  let forkedSandbox: Sandbox | null = null;

  try {
    // ── Setup: Create sandbox and write initial state ──────────────
    bold("━━━ Setup: Create sandbox ━━━\n");

    sandbox = await Sandbox.create({ template: "base", timeout: 300 });
    green(`Created sandbox: ${sandbox.sandboxId}`);

    // Write a marker file so we can verify state after restore
    await sandbox.commands.run("echo version-1 > /workspace/marker.txt");
    const v1 = await sandbox.commands.run("cat /workspace/marker.txt");
    check("Initial state written", v1.stdout.trim() === "version-1");
    console.log();

    // ── Test 1: Create first checkpoint ───────────────────────────
    bold("━━━ Test 1: Create checkpoint ━━━\n");

    const cp1 = await sandbox.createCheckpoint("v1-baseline");
    check("Checkpoint created", cp1.id !== undefined && cp1.id.length > 0);
    check("Checkpoint name is correct", cp1.name === "v1-baseline");
    check("Checkpoint status is processing or ready", cp1.status === "processing" || cp1.status === "ready");
    dim(`Checkpoint ID: ${cp1.id}`);
    console.log();

    // ── Test 2: List checkpoints ──────────────────────────────────
    bold("━━━ Test 2: List checkpoints ━━━\n");

    const list1 = await sandbox.listCheckpoints();
    check("List returns 1 checkpoint", list1.length === 1, `got ${list1.length}`);
    check("Listed checkpoint has correct name", list1[0]?.name === "v1-baseline");
    console.log();

    // ── Test 3: Modify state and create second checkpoint ─────────
    bold("━━━ Test 3: Create second checkpoint with different state ━━━\n");

    await sandbox.commands.run("echo version-2 > /workspace/marker.txt");
    await sandbox.commands.run("echo extra-data > /workspace/extra.txt");
    const v2 = await sandbox.commands.run("cat /workspace/marker.txt");
    check("State modified to version-2", v2.stdout.trim() === "version-2");

    const cp2 = await sandbox.createCheckpoint("v2-modified");
    check("Second checkpoint created", cp2.id !== undefined);
    dim(`Checkpoint ID: ${cp2.id}`);

    const list2 = await sandbox.listCheckpoints();
    check("List now returns 2 checkpoints", list2.length === 2, `got ${list2.length}`);
    console.log();

    // ── Test 4: Wait for first checkpoint to be ready ─────────────
    bold("━━━ Test 4: Wait for checkpoint ready ━━━\n");

    dim("Waiting for v1-baseline checkpoint to become ready...");
    const cp1Ready = await waitForCheckpointReady(sandbox, cp1.id);
    check("First checkpoint is ready", cp1Ready);

    if (!cp1Ready) {
      red("Cannot proceed with restore tests - checkpoint not ready");
      throw new Error("Checkpoint not ready");
    }
    console.log();

    // ── Test 5: Restore to first checkpoint ───────────────────────
    bold("━━━ Test 5: Restore to v1-baseline checkpoint ━━━\n");

    dim("Restoring sandbox to v1-baseline...");
    await sandbox.restoreCheckpoint(cp1.id);
    green("Restore completed");

    // Wait a moment for the VM to boot
    await sleep(3000);

    // Verify state was reverted
    const restored = await sandbox.commands.run("cat /workspace/marker.txt");
    check("Marker file reverted to version-1", restored.stdout.trim() === "version-1", `got: ${restored.stdout.trim()}`);

    // The extra.txt should NOT exist since it was created after v1
    const extraCheck = await sandbox.commands.run("test -f /workspace/extra.txt && echo exists || echo missing");
    check("Extra file does not exist after restore", extraCheck.stdout.trim() === "missing", `got: ${extraCheck.stdout.trim()}`);
    console.log();

    // ── Test 6: Wait for second checkpoint and fork ───────────────
    bold("━━━ Test 6: Fork new sandbox from v2-modified checkpoint ━━━\n");

    dim("Waiting for v2-modified checkpoint to become ready...");
    const cp2Ready = await waitForCheckpointReady(sandbox, cp2.id);
    check("Second checkpoint is ready", cp2Ready);

    if (cp2Ready) {
      dim("Creating new sandbox from v2-modified checkpoint...");
      forkedSandbox = await Sandbox.createFromCheckpoint(cp2.id);
      green(`Forked sandbox created: ${forkedSandbox.sandboxId}`);
      check("Forked sandbox has different ID", forkedSandbox.sandboxId !== sandbox.sandboxId);

      // Wait for forked sandbox to be ready
      await sleep(3000);

      // Verify the forked sandbox has v2 state
      const forkedMarker = await forkedSandbox.commands.run("cat /workspace/marker.txt");
      check("Forked sandbox has version-2 state", forkedMarker.stdout.trim() === "version-2", `got: ${forkedMarker.stdout.trim()}`);

      const forkedExtra = await forkedSandbox.commands.run("cat /workspace/extra.txt");
      check("Forked sandbox has extra.txt", forkedExtra.stdout.trim() === "extra-data", `got: ${forkedExtra.stdout.trim()}`);
    } else {
      dim("Skipping fork test - checkpoint not ready");
    }
    console.log();

    // ── Test 7: Delete a checkpoint ───────────────────────────────
    bold("━━━ Test 7: Delete checkpoint ━━━\n");

    await sandbox.deleteCheckpoint(cp2.id);
    green("Deleted v2-modified checkpoint");

    const listAfterDelete = await sandbox.listCheckpoints();
    check("List returns 1 checkpoint after delete", listAfterDelete.length === 1, `got ${listAfterDelete.length}`);
    check("Remaining checkpoint is v1-baseline", listAfterDelete[0]?.name === "v1-baseline");
    console.log();

    // ── Test 8: Duplicate name rejected ───────────────────────────
    bold("━━━ Test 8: Duplicate checkpoint name ━━━\n");

    try {
      await sandbox.createCheckpoint("v1-baseline");
      red("Should have rejected duplicate name");
      failed++;
    } catch (err: any) {
      check("Duplicate name rejected with error", err.message.includes("409") || err.message.includes("already exists"), err.message);
    }
    console.log();

    // ── Test 9: Max 10 checkpoints limit ──────────────────────────
    bold("━━━ Test 9: Max checkpoint limit ━━━\n");

    // We already have 1 checkpoint (v1-baseline), create 9 more to hit the limit
    dim("Creating 9 more checkpoints to reach the limit...");
    for (let i = 2; i <= 10; i++) {
      try {
        await sandbox.createCheckpoint(`limit-test-${i}`);
      } catch (err: any) {
        red(`Failed to create checkpoint limit-test-${i}: ${err.message}`);
        failed++;
      }
    }

    const listFull = await sandbox.listCheckpoints();
    check("10 checkpoints exist", listFull.length === 10, `got ${listFull.length}`);

    // The 11th should fail
    try {
      await sandbox.createCheckpoint("one-too-many");
      red("Should have rejected 11th checkpoint");
      failed++;
    } catch (err: any) {
      check("11th checkpoint rejected (max 10)", err.message.includes("400") || err.message.includes("maximum"), err.message);
    }
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    // Cleanup
    if (forkedSandbox) {
      try { await forkedSandbox.kill(); green("Forked sandbox killed"); } catch { /* best effort */ }
    }
    if (sandbox) {
      try { await sandbox.kill(); green("Original sandbox killed"); } catch { /* best effort */ }
    }
  }

  // --- Summary ---
  bold("========================================");
  bold(` Results: ${passed} passed, ${failed} failed`);
  bold("========================================\n");
  if (failed > 0) process.exit(1);
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
