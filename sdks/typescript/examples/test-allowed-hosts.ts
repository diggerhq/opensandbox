/**
 * sandbox.getAllowedHosts() Test
 *
 * Verifies AllowedHostsInfo returned by sandbox.getAllowedHosts():
 *   - sandboxID             string
 *   - secretStore           string | undefined           (undefined when no store)
 *   - egressAllowlist       string[]                     (hosts reachable via secrets proxy)
 *   - perSecretAllowedHosts Record<string, string[]>     (per-secret host restrictions)
 *
 * The server response also includes `baseSecretStore` (parent name for layered
 * forks; omitted when empty). The SDK type doesn't expose it yet, so the test
 * reads it via cast.
 *
 * Cases covered:
 *   1. Sandbox with no secret store        → secretStore undefined, empty allowlist
 *   2. Sandbox with secret store           → store name + aggregated egress allowlist
 *   3. Per-secret allowedHosts             → surfaced under perSecretAllowedHosts
 *   4. Forked store (createFromCheckpoint) → baseSecretStore reflects parent
 *
 * Usage:
 *   OPENCOMPUTER_API_URL=... OPENCOMPUTER_API_KEY=... npx tsx examples/test-allowed-hosts.ts
 */

import { Sandbox, SecretStore } from "../src/index";

function green(msg: string) { console.log(`\x1b[32m✓ ${msg}\x1b[0m`); }
function red(msg: string, detail?: string) {
  console.log(`\x1b[31m✗ ${msg}${detail ? ` — ${detail}` : ""}\x1b[0m`);
}
function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }

let passed = 0;
let failed = 0;

function check(desc: string, condition: boolean, detail?: string) {
  if (condition) { green(desc); passed++; }
  else { red(desc, detail); failed++; }
}

async function main() {
  bold("\n=== sandbox.getAllowedHosts() ===\n");

  const suffix = Date.now();
  const baseStoreName = `allowed-hosts-base-${suffix}`;
  const forkStoreName = `allowed-hosts-fork-${suffix}`;

  let baseStoreId: string | null = null;
  let forkStoreId: string | null = null;
  let plainSandbox: Sandbox | null = null;
  let storeSandbox: Sandbox | null = null;
  let forkedSandbox: Sandbox | null = null;
  let checkpointId: string | null = null;

  try {
    // ── Setup: two secret stores ──────────────────────────────────────
    bold("--- Setup: create base + fork secret stores ---\n");

    const baseStore = await SecretStore.create({
      name: baseStoreName,
      egressAllowlist: ["api.anthropic.com", "github.com"],
    });
    baseStoreId = baseStore.id;
    await SecretStore.setSecret(baseStoreId, "GIT_TOKEN", "git_xyz", {
      allowedHosts: ["github.com"],
    });
    await SecretStore.setSecret(baseStoreId, "ANTHROPIC_KEY", "sk-ant-xyz");
    dim(`Base store: ${baseStoreId} (${baseStoreName})`);

    const forkStore = await SecretStore.create({
      name: forkStoreName,
      egressAllowlist: ["httpbin.org"],
    });
    forkStoreId = forkStore.id;
    await SecretStore.setSecret(forkStoreId, "EXTRA_KEY", "extra_val", {
      allowedHosts: ["httpbin.org"],
    });
    dim(`Fork store: ${forkStoreId} (${forkStoreName})`);
    console.log();

    // ── Case 1: sandbox with no secret store ──────────────────────────
    bold("--- Case 1: sandbox with no secret store ---\n");

    plainSandbox = await Sandbox.create({ timeout: 120 });
    const plainInfo = await plainSandbox.getAllowedHosts();
    const plainBase = (plainInfo as { baseSecretStore?: string }).baseSecretStore;

    check("sandboxID matches", plainInfo.sandboxID === plainSandbox.sandboxId,
      `got "${plainInfo.sandboxID}"`);
    check("secretStore is unset", plainInfo.secretStore === undefined,
      `got ${JSON.stringify(plainInfo.secretStore)}`);
    check("baseSecretStore is unset", plainBase === undefined || plainBase === "",
      `got ${JSON.stringify(plainBase)}`);
    check("egressAllowlist is empty array",
      Array.isArray(plainInfo.egressAllowlist) && plainInfo.egressAllowlist.length === 0,
      `got ${JSON.stringify(plainInfo.egressAllowlist)}`);
    check("perSecretAllowedHosts has no entries",
      Object.keys(plainInfo.perSecretAllowedHosts ?? {}).length === 0,
      `got ${JSON.stringify(plainInfo.perSecretAllowedHosts)}`);
    console.log();

    // ── Case 2 + 3: sandbox with secret store + per-secret hosts ──────
    bold("--- Case 2/3: sandbox with secret store ---\n");

    storeSandbox = await Sandbox.create({
      secretStore: baseStoreName,
      timeout: 120,
    });
    const storeInfo = await storeSandbox.getAllowedHosts();
    const storeBase = (storeInfo as { baseSecretStore?: string }).baseSecretStore;

    check("sandboxID matches", storeInfo.sandboxID === storeSandbox.sandboxId);
    check("secretStore name matches", storeInfo.secretStore === baseStoreName,
      `got "${storeInfo.secretStore}"`);
    check("baseSecretStore is unset (no fork)",
      storeBase === undefined || storeBase === "",
      `got ${JSON.stringify(storeBase)}`);

    const egress = storeInfo.egressAllowlist ?? [];
    check("egressAllowlist contains api.anthropic.com",
      egress.includes("api.anthropic.com"), `got ${JSON.stringify(egress)}`);
    check("egressAllowlist contains github.com",
      egress.includes("github.com"), `got ${JSON.stringify(egress)}`);

    const perSecret = storeInfo.perSecretAllowedHosts ?? {};
    check("perSecretAllowedHosts has GIT_TOKEN", "GIT_TOKEN" in perSecret,
      `keys: ${Object.keys(perSecret).join(", ")}`);
    check("GIT_TOKEN restricted to github.com",
      Array.isArray(perSecret.GIT_TOKEN) &&
        perSecret.GIT_TOKEN.length === 1 &&
        perSecret.GIT_TOKEN[0] === "github.com",
      `got ${JSON.stringify(perSecret.GIT_TOKEN)}`);
    check("ANTHROPIC_KEY has no per-secret restriction",
      !("ANTHROPIC_KEY" in perSecret),
      "secret without allowedHosts should not appear in perSecretAllowedHosts");
    console.log();

    // ── Case 4: forked sandbox via createFromCheckpoint ──────────────
    bold("--- Case 4: forked sandbox (baseSecretStore) ---\n");

    checkpointId = (await storeSandbox.createCheckpoint(`allowed-hosts-cp-${suffix}`)).id;
    dim(`Checkpoint: ${checkpointId}`);

    forkedSandbox = await Sandbox.createFromCheckpoint(checkpointId, {
      secretStore: forkStoreName,
      timeout: 120,
    });
    const forkInfo = await forkedSandbox.getAllowedHosts();
    const forkBase = (forkInfo as { baseSecretStore?: string }).baseSecretStore;

    check("sandboxID matches", forkInfo.sandboxID === forkedSandbox.sandboxId);
    check("secretStore is fork store", forkInfo.secretStore === forkStoreName,
      `got "${forkInfo.secretStore}"`);
    check("baseSecretStore is parent store",
      forkBase === baseStoreName,
      `got ${JSON.stringify(forkBase)}`);

    const forkEgress = forkInfo.egressAllowlist ?? [];
    check("forked egress includes parent host (github.com)",
      forkEgress.includes("github.com"), `got ${JSON.stringify(forkEgress)}`);
    check("forked egress includes child host (httpbin.org)",
      forkEgress.includes("httpbin.org"), `got ${JSON.stringify(forkEgress)}`);

    const forkPerSecret = forkInfo.perSecretAllowedHosts ?? {};
    check("forked perSecret includes parent GIT_TOKEN",
      "GIT_TOKEN" in forkPerSecret);
    check("forked perSecret includes child EXTRA_KEY",
      "EXTRA_KEY" in forkPerSecret);
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    // Cleanup
    for (const sb of [plainSandbox, storeSandbox, forkedSandbox]) {
      if (sb) { try { await sb.kill(); } catch {} }
    }
    if (checkpointId && storeSandbox) {
      try { await storeSandbox.deleteCheckpoint(checkpointId); } catch {}
    }
    if (forkStoreId) { try { await SecretStore.delete(forkStoreId); } catch {} }
    if (baseStoreId) { try { await SecretStore.delete(baseStoreId); } catch {} }
  }

  bold("========================================");
  bold(` Results: ${passed} passed, ${failed} failed`);
  bold("========================================\n");
  if (failed > 0) process.exit(1);
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
