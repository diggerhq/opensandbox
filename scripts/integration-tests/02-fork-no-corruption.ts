/**
 * Regression test for:
 *   - fix(qemu): savevm/loadvm for checkpoints (1caf148)
 *   - fix(qemu): harden fork against post-loadvm virtio-serial flake (b2aa416)
 *
 * Before the fix, checkpoint creation used QMP migrate + external reflink copy
 * of the qcow2 files. QEMU's virtio-blk layer could have writes in flight at
 * reflink time that weren't yet flushed to the qcow2, so ~30% of forks came
 * up with corrupted workspace: empty marker.txt, git commands segfaulting,
 * ext4 EBADMSG errors. This is the headline bug the PR fixes.
 *
 * The virtio-serial mitigation (1s settle + retry) folds in here because a
 * flaky virtio-serial on fork manifests as "agent not ready after 30s" — we
 * want every fork to successfully reach the marker-verification step.
 *
 * This test creates N distinct checkpoints (each with a different marker hash)
 * and forks each one, verifying:
 *   - /home/sandbox/marker.txt content matches what was written pre-checkpoint
 *   - git fsck on the in-sandbox repo exits 0 (no object corruption)
 *   - git log --oneline count matches expected
 *
 * Corruption rate must be 0% — one failure is a regression.
 */

import { Sandbox } from "../../sdks/typescript/src";
import { randomBytes } from "node:crypto";

const N = 10;

async function waitReady(sb: Sandbox, cpId: string, timeoutMs = 180000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const cps = await sb.listCheckpoints();
    const cp = cps.find((c: any) => c.id === cpId);
    if (cp && (cp as any).status === "ready") return;
    if (cp && (cp as any).status === "failed") throw new Error("checkpoint failed");
    await new Promise(r => setTimeout(r, 1000));
  }
  throw new Error("checkpoint not ready");
}

async function main() {
  let passed = 0, failed = 0;
  const failures: string[] = [];
  const source = await Sandbox.create({ template: "base", timeout: 1800 });
  const toKill: string[] = [source.sandboxId];
  const cpIds: string[] = [];

  try {
    // Seed git repo
    await source.exec.run(
      "mkdir -p /home/sandbox/repo && cd /home/sandbox/repo && " +
      "git init -q && git config user.email t@t && git config user.name t && " +
      "echo initial > README && git add README && git commit -qm initial && sync",
      { timeout: 30 }
    );

    type CP = { id: string; marker: string; commitCount: number; label: string };
    const checkpoints: CP[] = [];

    // Create N checkpoints, each with distinct marker + new git commit
    for (let i = 0; i < N; i++) {
      const marker = randomBytes(32).toString("hex");
      const mutateCmd =
        `echo ${marker} > /home/sandbox/marker.txt && ` +
        `cd /home/sandbox/repo && echo "line-${i}" >> README && ` +
        `git add README && git commit -qm "cp-${i}" && sync`;
      const r = await source.exec.run(mutateCmd, { timeout: 30 });
      if ((r.exitCode ?? -1) !== 0) throw new Error(`mutate ${i}: exit=${r.exitCode}`);
      const cc = await source.exec.run(
        "cd /home/sandbox/repo && git log --oneline | wc -l",
        { timeout: 10 },
      );
      const commitCount = parseInt((cc.stdout ?? "").trim(), 10);
      const label = `no-corrupt-${i}-${Date.now()}`;
      const cp = await source.createCheckpoint(label);
      cpIds.push(cp.id);
      await waitReady(source, cp.id);
      checkpoints.push({ id: cp.id, marker, commitCount, label });
      console.log(`  created ${i + 1}/${N} checkpoint ${label} commits=${commitCount}`);
    }

    // Fork each checkpoint and verify
    for (const cp of checkpoints) {
      let fork: Sandbox | null = null;
      try {
        fork = await Sandbox.createFromCheckpoint(cp.id, { timeout: 300 });
        toKill.push(fork.sandboxId);
        // Check 1: marker matches
        const m = await fork.exec.run("cat /home/sandbox/marker.txt", { timeout: 10 });
        const mv = (m.stdout ?? "").trim();
        if (mv !== cp.marker) {
          failed++;
          failures.push(`${cp.label}: marker mismatch (expected ${cp.marker.slice(0, 12)}… got '${mv.slice(0, 16)}')`);
          continue;
        }
        // Check 2: git fsck clean
        const fsck = await fork.exec.run(
          "cd /home/sandbox/repo && git fsck --full 2>&1",
          { timeout: 30 },
        );
        if ((fsck.exitCode ?? -1) !== 0) {
          failed++;
          failures.push(`${cp.label}: git fsck exit=${fsck.exitCode} out=${((fsck.stdout ?? "") + (fsck.stderr ?? "")).slice(0, 200)}`);
          continue;
        }
        // Check 3: commit count
        const cc = await fork.exec.run(
          "cd /home/sandbox/repo && git log --oneline | wc -l",
          { timeout: 10 },
        );
        const got = parseInt((cc.stdout ?? "").trim(), 10);
        if (got !== cp.commitCount) {
          failed++;
          failures.push(`${cp.label}: commit count expected=${cp.commitCount} got=${got}`);
          continue;
        }
        passed++;
        console.log(`  ✓ fork ${fork.sandboxId} from ${cp.label}: marker+fsck+commits OK`);
      } catch (e: any) {
        failed++;
        failures.push(`${cp.label}: fork/verify threw ${e?.message ?? String(e)}`);
      } finally {
        if (fork) {
          try { await fork.kill(); } catch {}
        }
      }
    }
  } finally {
    for (const id of cpIds) {
      try { await source.deleteCheckpoint(id); } catch {}
    }
    for (const id of toKill) {
      try { const s = await Sandbox.connect(id); await s.kill(); } catch {}
    }
  }

  console.log(`\n=== ${passed}/${N} forks clean, ${failed} corrupt ===`);
  for (const f of failures) console.log("  - " + f);
  process.exit(failed === 0 ? 0 : 1);
}

main().catch(e => { console.error(e); process.exit(1); });
