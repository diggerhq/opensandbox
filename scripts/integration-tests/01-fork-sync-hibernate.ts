/**
 * Regression test for: fix(api): make createFromCheckpoint synchronous (08d5320)
 *
 * Before the fix, createFromCheckpoint wrote the sandbox_sessions row with
 * status=running upfront, then kicked the actual fork into a background
 * goroutine. For 2-5s the worker's m.vms[id] wasn't populated yet, so any
 * control-plane RPC routed to the worker returned
 *   500 "sandbox not found"
 *
 * This test forks from a checkpoint and IMMEDIATELY calls hibernate. If the
 * fix is in place, hibernate works; otherwise we see "sandbox not found".
 *
 * Usage:
 *   OPENCOMPUTER_API_URL=... OPENCOMPUTER_API_KEY=... npx tsx 01-fork-sync-hibernate.ts
 */

import { Sandbox } from "../../sdks/typescript/src";

const ITERATIONS = 3;

async function main() {
  const source = await Sandbox.create({ template: "base", timeout: 600 });
  const toKill: string[] = [source.sandboxId];
  let passed = 0, failed = 0;

  try {
    // Seed minimal state and create a checkpoint on the source
    await source.exec.run("echo seed > /home/sandbox/seed.txt && sync", { timeout: 30 });
    const cp = await source.createCheckpoint("sync-fork-test");
    for (let i = 0; i < 60; i++) {
      const cps = await source.listCheckpoints();
      const found = cps.find((c: any) => c.id === cp.id);
      if (found && (found as any).status === "ready") break;
      await new Promise(r => setTimeout(r, 1000));
    }
    console.log(`checkpoint ${cp.id} ready`);

    // Run ITERATIONS of: fork -> immediate hibernate
    for (let i = 0; i < ITERATIONS; i++) {
      let fork: Sandbox | null = null;
      try {
        fork = await Sandbox.createFromCheckpoint(cp.id, { timeout: 300 });
        toKill.push(fork.sandboxId);
        // Immediate hibernate — previously this would race with the worker's
        // m.vms[id] registration and 500.
        await fork.hibernate();
        console.log(`[${i + 1}/${ITERATIONS}] ✓ fork ${fork.sandboxId} -> hibernate OK`);
        passed++;
      } catch (e: any) {
        const msg = e?.message ?? String(e);
        if (msg.includes("sandbox") && msg.includes("not found")) {
          console.log(`[${i + 1}/${ITERATIONS}] ✗ REGRESSION: sync-fork race returned: ${msg}`);
        } else {
          console.log(`[${i + 1}/${ITERATIONS}] ✗ unexpected failure: ${msg}`);
        }
        failed++;
      }
    }

    // Clean up checkpoint
    try { await source.deleteCheckpoint(cp.id); } catch {}
  } finally {
    for (const id of toKill) {
      try { const s = await Sandbox.connect(id); await s.kill(); } catch {}
    }
  }

  console.log(`\n=== ${passed} passed, ${failed} failed ===`);
  process.exit(failed === 0 ? 0 : 1);
}

main().catch(e => { console.error(e); process.exit(1); });
