/**
 * Regression test for: fix(qemu): remove superfluous post-loadvm mount on wake (9f550e2)
 *
 * Before the fix, doWake force-mounted /dev/vdb on /home/sandbox after loadvm.
 * On workers with the stale rootfs build (where the guest's init mounts vdb
 * at /workspace, not /home/sandbox), this force-mount shadowed the rootfs's
 * /home/sandbox directory with the empty workspace filesystem — silently
 * HIDING any files the user had written (those writes landed on rootfs.qcow2
 * because /home/sandbox was a regular directory at the time of the write).
 *
 * The user-visible symptom: hibernate → wake, then `ls /home/sandbox` shows
 * only "lost+found" and their data is apparently gone. (It's still on disk
 * inside rootfs.qcow2 — just hidden under the new mount.)
 *
 * This test:
 *   1. Writes multiple files + a git repo to /home/sandbox
 *   2. Hibernates
 *   3. Wakes
 *   4. Verifies every file is still present and readable
 *
 * With the fix in place, loadvm restores the correct guest mount state and
 * all files are preserved. Without it (on stale-rootfs workers), files vanish.
 */

import { Sandbox } from "../../sdks/typescript/src";
import { randomBytes } from "node:crypto";

async function wakeWithRetry(sb: Sandbox, maxAttempts = 15, delayMs = 2000) {
  let lastErr: any = null;
  for (let i = 0; i < maxAttempts; i++) {
    try { await sb.wake({ timeout: 300 }); return; } catch (e: any) {
      const msg = e?.message ?? String(e);
      lastErr = e;
      if (msg.includes("BlobNotFound") || msg.includes("404")) {
        await new Promise(r => setTimeout(r, delayMs)); continue;
      }
      throw e;
    }
  }
  throw lastErr;
}

async function main() {
  const markerA = randomBytes(16).toString("hex");
  const markerB = randomBytes(16).toString("hex");
  const sb = await Sandbox.create({ template: "base", timeout: 600 });
  let passed = 0, failed = 0;

  try {
    // Write a mix: plain file, nested dir, git repo
    await sb.exec.run(
      [
        `echo ${markerA} > /home/sandbox/top.txt`,
        `mkdir -p /home/sandbox/nested/deep`,
        `echo ${markerB} > /home/sandbox/nested/deep/inner.txt`,
        `cd /home/sandbox && git init -q repo && cd repo && git config user.email t@t && git config user.name t`,
        `cd /home/sandbox/repo && echo initial > README && git add README && git commit -qm initial`,
        `sync`,
      ].join(" && "),
      { timeout: 30 },
    );

    await sb.hibernate();
    await wakeWithRetry(sb);

    // Verify everything we wrote is still there
    const checks: { name: string; cmd: string; expect: string }[] = [
      { name: "top-level file",        cmd: "cat /home/sandbox/top.txt",             expect: markerA },
      { name: "nested file",           cmd: "cat /home/sandbox/nested/deep/inner.txt", expect: markerB },
      { name: "git repo README",       cmd: "cat /home/sandbox/repo/README",         expect: "initial" },
      { name: "git repo commit count", cmd: "cd /home/sandbox/repo && git log --oneline | wc -l", expect: "1" },
      { name: "git fsck clean",        cmd: "cd /home/sandbox/repo && git fsck --full 2>&1 && echo FSCK_OK", expect: "FSCK_OK" },
    ];

    for (const c of checks) {
      const r = await sb.exec.run(c.cmd, { timeout: 15 });
      const got = (r.stdout ?? "").trim();
      if (got === c.expect || got.endsWith(c.expect)) {
        console.log(`  ✓ ${c.name}`);
        passed++;
      } else {
        const stderr = (r.stderr ?? "").slice(0, 120);
        console.log(`  ✗ ${c.name}: expected '${c.expect.slice(0, 16)}' got '${got.slice(0, 40)}' stderr='${stderr}'`);
        failed++;
      }
    }

    // Also verify the workspace disk really is mounted at /home/sandbox
    // (would also have been broken by the wake mount shadow).
    const df = await sb.exec.run("df /home/sandbox | tail -1 | awk '{print $2}'", { timeout: 10 });
    const blocks = parseInt((df.stdout ?? "").trim(), 10);
    if (blocks > 1_000_000) {
      console.log(`  ✓ /home/sandbox has substantial space (${blocks} 1K-blocks)`);
      passed++;
    } else {
      console.log(`  ✗ /home/sandbox is unexpectedly small (${blocks} 1K-blocks) — may be on tiny rootfs`);
      failed++;
    }
  } finally {
    try { await sb.kill(); } catch {}
  }

  console.log(`\n=== ${passed} passed, ${failed} failed ===`);
  process.exit(failed === 0 ? 0 : 1);
}

main().catch(e => { console.error(e); process.exit(1); });
