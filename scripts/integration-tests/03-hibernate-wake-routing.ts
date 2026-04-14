/**
 * Regression test for: fix(api): invalidate proxy route cache on hibernate/wake (f4e3600)
 *
 * Before the fix, SandboxAPIProxy cached sandbox→worker routes. On multi-worker
 * deploys wake may land the sandbox on a different worker than hibernate did,
 * but the stale cache kept forwarding data-plane requests to the ex-hibernate
 * worker. That worker's router still showed StateHibernated, so any data-plane
 * request triggered a local auto-wake that failed with
 *   500 "auto-wake failed for sandbox X: no active hibernation"
 *
 * This test:
 *   1. Creates a sandbox
 *   2. Writes a marker so there's guest state to preserve
 *   3. Hibernates
 *   4. Wakes
 *   5. Calls exec.run (the first data-plane request post-wake)
 *
 * If cache invalidation is in place, step 5 succeeds and returns the marker.
 * Otherwise it returns the "auto-wake failed" 500.
 */

import { Sandbox } from "../../sdks/typescript/src";
import { randomBytes } from "node:crypto";

const ITERATIONS = 3;

async function wakeWithRetry(sb: Sandbox, maxAttempts = 15, delayMs = 2000) {
  let lastErr: any = null;
  for (let i = 0; i < maxAttempts; i++) {
    try {
      await sb.wake({ timeout: 300 });
      return;
    } catch (e: any) {
      const msg = e?.message ?? String(e);
      lastErr = e;
      if (msg.includes("BlobNotFound") || msg.includes("404")) {
        await new Promise(r => setTimeout(r, delayMs));
        continue;
      }
      throw e;
    }
  }
  throw lastErr;
}

async function main() {
  let passed = 0, failed = 0;

  for (let i = 0; i < ITERATIONS; i++) {
    const marker = randomBytes(16).toString("hex");
    const sb = await Sandbox.create({ template: "base", timeout: 600 });
    try {
      await sb.exec.run(
        `echo ${marker} > /home/sandbox/.marker && sync`,
        { timeout: 30 },
      );
      await sb.hibernate();
      await wakeWithRetry(sb);
      // The critical call: first data-plane request after wake. With a stale
      // proxy cache, this returned 500 "auto-wake failed".
      const r = await sb.exec.run("cat /home/sandbox/.marker", { timeout: 10 });
      const got = (r.stdout ?? "").trim();
      if (got === marker) {
        console.log(`[${i + 1}/${ITERATIONS}] ✓ hibernate→wake→exec.run routed correctly`);
        passed++;
      } else {
        console.log(`[${i + 1}/${ITERATIONS}] ✗ exec.run after wake returned wrong marker (got '${got.slice(0, 16)}')`);
        failed++;
      }
    } catch (e: any) {
      const msg = e?.message ?? String(e);
      if (msg.includes("auto-wake failed")) {
        console.log(`[${i + 1}/${ITERATIONS}] ✗ REGRESSION: stale proxy cache — ${msg}`);
      } else {
        console.log(`[${i + 1}/${ITERATIONS}] ✗ unexpected failure: ${msg}`);
      }
      failed++;
    } finally {
      try { await sb.kill(); } catch {}
    }
  }

  console.log(`\n=== ${passed}/${ITERATIONS} routed correctly, ${failed} regressed ===`);
  process.exit(failed === 0 ? 0 : 1);
}

main().catch(e => { console.error(e); process.exit(1); });
