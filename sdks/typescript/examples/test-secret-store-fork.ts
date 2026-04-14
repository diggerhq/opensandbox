/**
 * Test secret store inheritance on fork / createFromCheckpoint.
 *
 * Creates two secret stores, a sandbox with store A, checkpoints it,
 * then forks with store B attached. Verifies:
 *   1. Secret env vars from both stores are present (layered merge)
 *   2. Secrets are sealed in-guest (osb_sealed_* tokens, not plaintext)
 *   3. Actual secret VALUES resolve correctly through the proxy (httpbin echo)
 *   4. Store B's value wins on collision (SHARED_KEY override)
 *   5. Egress allowlists aggregate across layers
 *
 * Usage:
 *   OPENCOMPUTER_API_URL=... OPENCOMPUTER_API_KEY=... npx tsx examples/test-secret-store-fork.ts
 */

import { Sandbox, SecretStore } from "../src/index";
import { Snapshots, Image } from "../src/node";
import { randomBytes } from "node:crypto";

// Random values so we can verify exact substitution via httpbin
const GIT_TOKEN_VAL = `git_${randomBytes(12).toString("hex")}`;
const SHARED_KEY_A_VAL = `shared_a_${randomBytes(12).toString("hex")}`;
const SHARED_KEY_B_VAL = `shared_b_${randomBytes(12).toString("hex")}`;
const API_KEY_VAL = `api_${randomBytes(12).toString("hex")}`;

let storeAId: string | null = null;
let storeBId: string | null = null;
let baseSandbox: Sandbox | null = null;
let forkedSandbox: Sandbox | null = null;
let snapshotSandbox: Sandbox | null = null;
let snapshotName: string | null = null;
let passed = 0;
let failed = 0;

const green = (s: string) => console.log(`\x1b[32m✓ ${s}\x1b[0m`);
const red = (s: string, detail?: string) =>
  console.log(`\x1b[31m✗ ${s}${detail ? ` — ${detail}` : ""}\x1b[0m`);
const bold = (s: string) => console.log(`\x1b[1m${s}\x1b[0m`);
const dim = (s: string) => console.log(`\x1b[2m  ${s}\x1b[0m`);

function check(desc: string, condition: boolean, detail?: string) {
  if (condition) {
    green(desc);
    passed++;
  } else {
    red(desc, detail);
    failed++;
  }
}

async function main() {
  const suffix = Date.now();
  const storeAName = `fork-test-a-${suffix}`;
  const storeBName = `fork-test-b-${suffix}`;

  try {
    // ── Setup: two secret stores ──────────────────────────────────────
    bold("\n=== 1. Setup: create two secret stores ===\n");

    // Store A: egress to httpbin.org (for value verification) + github.com
    const storeA = await SecretStore.create({
      name: storeAName,
      egressAllowlist: ["github.com", "httpbin.org"],
    });
    storeAId = storeA.id;
    console.log(`  Store A: ${storeAId} (${storeAName})`);

    await SecretStore.setSecret(storeAId, "GIT_TOKEN", GIT_TOKEN_VAL, {
      allowedHosts: ["github.com", "httpbin.org"],
    });
    await SecretStore.setSecret(storeAId, "SHARED_KEY", SHARED_KEY_A_VAL, {
      allowedHosts: ["httpbin.org"],
    });
    dim(`GIT_TOKEN = ${GIT_TOKEN_VAL}`);
    dim(`SHARED_KEY(A) = ${SHARED_KEY_A_VAL}`);
    green(
      "Store A created: GIT_TOKEN + SHARED_KEY, egress=[github.com, httpbin.org]",
    );

    // Store B: egress to httpbin.org (for value verification) + api.anthropic.com
    const storeB = await SecretStore.create({
      name: storeBName,
      egressAllowlist: ["api.anthropic.com", "httpbin.org"],
    });
    storeBId = storeB.id;
    console.log(`  Store B: ${storeBId} (${storeBName})`);

    await SecretStore.setSecret(storeBId, "API_KEY", API_KEY_VAL, {
      allowedHosts: ["api.anthropic.com", "httpbin.org"],
    });
    await SecretStore.setSecret(storeBId, "SHARED_KEY", SHARED_KEY_B_VAL, {
      allowedHosts: ["httpbin.org"],
    });
    dim(`API_KEY = ${API_KEY_VAL}`);
    dim(`SHARED_KEY(B) = ${SHARED_KEY_B_VAL} (should override A)`);
    green(
      "Store B created: API_KEY + SHARED_KEY(override), egress=[api.anthropic.com, httpbin.org]",
    );

    // ── Layer 1: sandbox with store A ─────────────────────────────────
    bold("\n=== 2. Create base sandbox with store A ===\n");

    baseSandbox = await Sandbox.create({
      secretStore: storeAName,
      timeout: 120,
    });
    console.log(`  Base sandbox: ${baseSandbox.sandboxId}`);
    await new Promise((r) => setTimeout(r, 5000));

    const envBase = (await baseSandbox.exec.run("env")).stdout;
    check("Base has GIT_TOKEN", envBase.includes("GIT_TOKEN"));
    check("Base has SHARED_KEY", envBase.includes("SHARED_KEY"));
    check("Base does NOT have API_KEY", !envBase.includes("API_KEY"));

    // Verify secrets are sealed (not plaintext) in base
    const baseGitEcho = (
      await baseSandbox.exec.run('printf %s "$GIT_TOKEN"')
    ).stdout.trim();
    check(
      "Base: GIT_TOKEN is sealed (not plaintext)",
      baseGitEcho.startsWith("osb_sealed_") &&
        !baseGitEcho.includes(GIT_TOKEN_VAL),
      `got "${baseGitEcho.slice(0, 40)}..."`,
    );

    // Verify actual value via httpbin
    const baseHttpbin = (
      await baseSandbox.exec.run(
        `curl -sS -m 10 https://httpbin.org/headers -H "X-Git: $GIT_TOKEN" -H "X-Shared: $SHARED_KEY"`,
      )
    ).stdout;
    check(
      "Base: GIT_TOKEN value resolves correctly via httpbin",
      baseHttpbin.includes(GIT_TOKEN_VAL),
      `httpbin did not echo back ${GIT_TOKEN_VAL}`,
    );
    check(
      "Base: SHARED_KEY(A) value resolves correctly via httpbin",
      baseHttpbin.includes(SHARED_KEY_A_VAL),
      `httpbin did not echo back ${SHARED_KEY_A_VAL}`,
    );

    // ── Checkpoint ────────────────────────────────────────────────────
    bold("\n=== 3. Create checkpoint from base ===\n");

    const cp = await baseSandbox.createCheckpoint("fork-test-cp");
    console.log(`  Checkpoint: ${cp.id}`);

    for (let i = 0; i < 30; i++) {
      const cps = await baseSandbox.listCheckpoints();
      const found = cps.find((c: { id: string }) => c.id === cp.id);
      if (found && (found as any).status === "ready") break;
      await new Promise((r) => setTimeout(r, 2000));
    }
    green("Checkpoint ready");

    // ── Layer 2: fork with store B ────────────────────────────────────
    bold(
      "\n=== 4. Fork from checkpoint with store B (secretStore inheritance) ===\n",
    );

    forkedSandbox = await Sandbox.createFromCheckpoint(cp.id, {
      secretStore: storeBName,
      timeout: 120,
    });
    console.log(`  Forked sandbox: ${forkedSandbox.sandboxId}`);
    await new Promise((r) => setTimeout(r, 5000));

    // ── Network debug for checkpoint fork ──────────────────────────────
    bold("\n=== 4-debug. Network state inside checkpoint fork ===\n");

    const netDebug = await forkedSandbox.exec.run(
      'echo "=== IP ===" && ip addr show eth0 2>&1 && ' +
        'echo "=== ROUTE ===" && ip route 2>&1 && ' +
        'echo "=== ARP ===" && ip neigh 2>&1 && ' +
        'echo "=== HTTP_PROXY ===" && echo "$HTTP_PROXY" && ' +
        'echo "=== PROXY REACH ===" && curl -sS --connect-timeout 5 -o /dev/null -w "http_code=%{http_code}" "$HTTP_PROXY" 2>&1 || echo "UNREACHABLE" && ' +
        'echo "=== PING GW ===" && ping -c 1 -W 2 $(ip route | grep default | awk "{print \\$3}") 2>&1 || echo "PING_FAIL"',
    );
    dim("Network debug output:");
    for (const line of netDebug.stdout.split("\n")) {
      dim(line);
    }
    if (netDebug.stderr) {
      dim("stderr: " + netDebug.stderr);
    }

    // ── 4a. Check env vars exist ──────────────────────────────────────
    bold("\n=== 4a. Verify secret env vars present ===\n");

    const envFork = (await forkedSandbox.exec.run("env")).stdout;
    check(
      "Fork has GIT_TOKEN (inherited from store A)",
      envFork.includes("GIT_TOKEN"),
    );
    check("Fork has API_KEY (from store B)", envFork.includes("API_KEY"));
    check("Fork has SHARED_KEY (merged)", envFork.includes("SHARED_KEY"));
    check(
      "Fork has HTTP_PROXY (secrets proxy active)",
      envFork.includes("HTTP_PROXY"),
    );

    const sealedCount = (envFork.match(/osb_sealed_/g) || []).length;
    check(
      `Fork has 3 sealed secrets (got ${sealedCount})`,
      sealedCount === 3,
      "expected GIT_TOKEN(A) + SHARED_KEY(B override) + API_KEY(B)",
    );

    // ── 4b. Verify secrets are sealed (not plaintext) ─────────────────
    bold("\n=== 4b. Verify secrets are sealed in-guest ===\n");

    const forkGitEcho = (
      await forkedSandbox.exec.run('printf %s "$GIT_TOKEN"')
    ).stdout.trim();
    check(
      "Fork: GIT_TOKEN is sealed",
      forkGitEcho.startsWith("osb_sealed_") &&
        !forkGitEcho.includes(GIT_TOKEN_VAL),
      `got "${forkGitEcho.slice(0, 40)}..."`,
    );

    const forkApiEcho = (
      await forkedSandbox.exec.run('printf %s "$API_KEY"')
    ).stdout.trim();
    check(
      "Fork: API_KEY is sealed",
      forkApiEcho.startsWith("osb_sealed_") &&
        !forkApiEcho.includes(API_KEY_VAL),
      `got "${forkApiEcho.slice(0, 40)}..."`,
    );

    const forkSharedEcho = (
      await forkedSandbox.exec.run('printf %s "$SHARED_KEY"')
    ).stdout.trim();
    check(
      "Fork: SHARED_KEY is sealed",
      forkSharedEcho.startsWith("osb_sealed_") &&
        !forkSharedEcho.includes(SHARED_KEY_B_VAL),
      `got "${forkSharedEcho.slice(0, 40)}..."`,
    );

    // ── 4c. Verify actual VALUES via httpbin ──────────────────────────
    bold(
      "\n=== 4c. Verify actual secret values via httpbin (proxy substitution) ===\n",
    );

    const forkHttpbin = (
      await forkedSandbox.exec.run(
        `curl -sS -m 10 https://httpbin.org/headers ` +
          `-H "X-Git: $GIT_TOKEN" ` +
          `-H "X-Api: $API_KEY" ` +
          `-H "X-Shared: $SHARED_KEY"`,
      )
    ).stdout;
    dim(`httpbin response (first 500 chars):`);
    dim(forkHttpbin.replace(/\s+/g, " ").slice(0, 500));

    check(
      "Fork: GIT_TOKEN value correct (inherited from A)",
      forkHttpbin.includes(GIT_TOKEN_VAL),
      `httpbin did not echo back ${GIT_TOKEN_VAL}`,
    );
    check(
      "Fork: API_KEY value correct (from B)",
      forkHttpbin.includes(API_KEY_VAL),
      `httpbin did not echo back ${API_KEY_VAL}`,
    );
    check(
      "Fork: SHARED_KEY has B's value (B override wins on collision)",
      forkHttpbin.includes(SHARED_KEY_B_VAL),
      `httpbin did not echo back B's value ${SHARED_KEY_B_VAL}`,
    );
    check(
      "Fork: SHARED_KEY does NOT have A's value (overridden by B)",
      !forkHttpbin.includes(SHARED_KEY_A_VAL),
      `httpbin echoed A's value ${SHARED_KEY_A_VAL} — override did not work`,
    );

    // ── 4d. Verify egress aggregation ─────────────────────────────────
    bold("\n=== 4d. Verify egress allowlist aggregation ===\n");

    // github.com (from A) and api.anthropic.com (from B) should both be reachable
    const githubResult = await forkedSandbox.exec.run(
      `curl -sS -m 10 -o /dev/null -w '%{http_code}' https://github.com/ 2>&1 || echo "BLOCKED"`,
    );
    check(
      "Fork: github.com reachable (from store A egress)",
      !githubResult.stdout.includes("BLOCKED") &&
        !githubResult.stdout.includes("000"),
      `got "${githubResult.stdout.trim()}"`,
    );

    const anthropicResult = await forkedSandbox.exec.run(
      `curl -sS -m 10 -o /dev/null -w '%{http_code}' https://api.anthropic.com/ 2>&1 || echo "BLOCKED"`,
    );
    check(
      "Fork: api.anthropic.com reachable (from store B egress)",
      !anthropicResult.stdout.includes("BLOCKED") &&
        !anthropicResult.stdout.includes("000"),
      `got "${anthropicResult.stdout.trim()}"`,
    );

    // ── 5. Snapshot/template path: create snapshot, fork with secretStore ──
    bold("\n=== 5. Create snapshot template, fork with secretStore ===\n");

    const snapshots = new Snapshots();
    snapshotName = `fork-secret-test-${suffix}`;
    dim(`Creating snapshot "${snapshotName}" from base image...`);
    await snapshots.create({
      name: snapshotName,
      image: Image.base().runCommands(
        "echo 'template-ready' > /home/sandbox/marker.txt",
      ),
    });

    // Poll until snapshot is ready
    for (let i = 0; i < 30; i++) {
      const info = await snapshots.get(snapshotName);
      if (info.status === "ready") break;
      await new Promise((r) => setTimeout(r, 2000));
    }
    green(`Snapshot "${snapshotName}" ready`);

    // Fork from snapshot with store B attached
    snapshotSandbox = await Sandbox.create({
      snapshot: snapshotName,
      secretStore: storeBName,
      timeout: 120,
    });
    console.log(`  Snapshot-forked sandbox: ${snapshotSandbox.sandboxId}`);
    await new Promise((r) => setTimeout(r, 5000));

    // Verify the template content survived
    const marker = (
      await snapshotSandbox.exec.run("cat /home/sandbox/marker.txt")
    ).stdout.trim();
    check(
      "Snapshot fork: template content present (/home/sandbox/marker.txt)",
      marker === "template-ready",
      `got "${marker}"`,
    );

    // Verify secrets from store B are available
    const envSnap = (await snapshotSandbox.exec.run("env")).stdout;
    check(
      "Snapshot fork: has API_KEY (from store B)",
      envSnap.includes("API_KEY"),
    );
    check(
      "Snapshot fork: has SHARED_KEY (from store B)",
      envSnap.includes("SHARED_KEY"),
    );
    check(
      "Snapshot fork: has HTTP_PROXY (secrets proxy active)",
      envSnap.includes("HTTP_PROXY"),
    );

    // Verify secrets are sealed
    const snapApiEcho = (
      await snapshotSandbox.exec.run('printf %s "$API_KEY"')
    ).stdout.trim();
    check(
      "Snapshot fork: API_KEY is sealed",
      snapApiEcho.startsWith("osb_sealed_") &&
        !snapApiEcho.includes(API_KEY_VAL),
      `got "${snapApiEcho.slice(0, 40)}..."`,
    );

    // Verify actual values via httpbin
    const snapHttpbin = (
      await snapshotSandbox.exec.run(
        `curl -sS -m 10 https://httpbin.org/headers ` +
          `-H "X-Api: $API_KEY" ` +
          `-H "X-Shared: $SHARED_KEY"`,
      )
    ).stdout;
    dim(`httpbin response (first 500 chars):`);
    dim(snapHttpbin.replace(/\s+/g, " ").slice(0, 500));

    check(
      "Snapshot fork: API_KEY value correct via httpbin",
      snapHttpbin.includes(API_KEY_VAL),
      `httpbin did not echo back ${API_KEY_VAL}`,
    );
    check(
      "Snapshot fork: SHARED_KEY has B's value via httpbin",
      snapHttpbin.includes(SHARED_KEY_B_VAL),
      `httpbin did not echo back ${SHARED_KEY_B_VAL}`,
    );

    // ── Summary ───────────────────────────────────────────────────────
    bold(`\n=== Results: ${passed} passed, ${failed} failed ===\n`);
  } finally {
    bold("=== Cleanup ===");
    if (snapshotSandbox) {
      try {
        await snapshotSandbox.shutdown();
      } catch {}
    }
    if (forkedSandbox) {
      try {
        await forkedSandbox.shutdown();
      } catch {}
    }
    if (baseSandbox) {
      try {
        await baseSandbox.shutdown();
      } catch {}
    }
    if (snapshotName) {
      try {
        const snapshots = new Snapshots();
        await snapshots.delete(snapshotName);
      } catch {}
    }
    if (storeBId) {
      try {
        await SecretStore.delete(storeBId);
      } catch {}
    }
    if (storeAId) {
      try {
        await SecretStore.delete(storeAId);
      } catch {}
    }
    green("Cleanup done");
  }

  process.exit(failed > 0 ? 1 : 0);
}

main().catch((err) => {
  console.error("Fatal:", err);
  process.exit(1);
});
