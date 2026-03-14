/**
 * Secret Stores & Secrets Test
 *
 * Tests:
 *   1. Create a secret store
 *   2. List secret stores
 *   3. Get secret store by ID
 *   4. Update secret store
 *   5. Set secrets on a store
 *   6. List secrets (names only, values never returned)
 *   7. Create sandbox with secret store (inherits secrets)
 *   8. Verify secrets are injected as sealed env vars
 *   9. Delete secret
 *  10. Delete secret store
 *
 * Usage:
 *   npx tsx examples/test-projects.ts
 */

import { Sandbox, SecretStore } from "../src/index";

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

async function main() {
  bold("\n\u2554\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2557");
  bold("\u2551       Secret Stores & Secrets Test               \u2551");
  bold("\u255a\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u255d\n");

  let storeId: string | null = null;
  let sandbox: Sandbox | null = null;
  const storeName = `test-store-${Date.now()}`;

  try {
    // -- Test 1: Create secret store --
    bold("--- Test 1: Create secret store ---\n");

    const store = await SecretStore.create({
      name: storeName,
      egressAllowlist: ["api.anthropic.com"],
    });

    storeId = store.id;
    check("Store created", !!store.id);
    check("Name matches", store.name === storeName);
    check("Egress allowlist set", store.egressAllowlist?.length === 1);
    dim(`Store ID: ${store.id}`);
    console.log();

    // -- Test 2: List secret stores --
    bold("--- Test 2: List secret stores ---\n");

    const stores = await SecretStore.list();
    check("List returns array", Array.isArray(stores));
    const found = stores.find((s) => s.id === storeId);
    check("Created store in list", !!found);
    dim(`Total stores: ${stores.length}`);
    console.log();

    // -- Test 3: Get secret store --
    bold("--- Test 3: Get secret store by ID ---\n");

    const fetched = await SecretStore.get(storeId!);
    check("Get returns correct store", fetched.id === storeId);
    check("Get has correct name", fetched.name === storeName);
    console.log();

    // -- Test 4: Update secret store --
    bold("--- Test 4: Update secret store ---\n");

    const updatedName = `${storeName}-updated`;
    const updated = await SecretStore.update(storeId!, {
      name: updatedName,
      egressAllowlist: ["api.anthropic.com", "*.openai.com"],
    });
    check("Name updated", updated.name === updatedName);
    check("Egress allowlist updated", updated.egressAllowlist?.length === 2);
    console.log();

    // -- Test 5: Set secrets --
    bold("--- Test 5: Set secrets ---\n");

    await SecretStore.setSecret(storeId!, "TEST_API_KEY", "sk-test-12345");
    green("Set TEST_API_KEY");

    await SecretStore.setSecret(storeId!, "DATABASE_URL", "postgres://localhost/test");
    green("Set DATABASE_URL");

    await SecretStore.setSecret(storeId!, "TEMP_SECRET", "will-be-deleted");
    green("Set TEMP_SECRET");
    console.log();

    // -- Test 6: List secrets --
    bold("--- Test 6: List secret entries ---\n");

    const entries = await SecretStore.listSecrets(storeId!);
    check("Returns array", Array.isArray(entries));
    const names = entries.map((e) => e.name);
    check("Has TEST_API_KEY", names.includes("TEST_API_KEY"));
    check("Has DATABASE_URL", names.includes("DATABASE_URL"));
    check("Has TEMP_SECRET", names.includes("TEMP_SECRET"));
    check("3 secrets total", entries.length === 3, `got ${entries.length}`);
    dim(`Secret names: ${names.join(", ")}`);
    console.log();

    // -- Test 7: Create sandbox with secret store --
    bold("--- Test 7: Create sandbox with secret store ---\n");

    sandbox = await Sandbox.create({
      secretStore: updatedName,
      timeout: 120,
    });
    check("Sandbox created", !!sandbox.sandboxId);
    dim(`Sandbox ID: ${sandbox.sandboxId}`);
    console.log();

    // -- Test 8: Verify secrets are sealed in sandbox --
    bold("--- Test 8: Verify secrets sealed in sandbox ---\n");

    // Secrets should be sealed tokens (osb_sealed_*) inside the VM.
    // The MITM proxy replaces sealed tokens with real values on outbound HTTPS requests,
    // so the real secret never exists in VM memory.
    const apiKeyResult = await sandbox.commands.run("echo $TEST_API_KEY");
    const apiKeyVal = apiKeyResult.stdout.trim();
    check("TEST_API_KEY is sealed", apiKeyVal.startsWith("osb_sealed_"),
      `got "${apiKeyVal}"`);

    const dbUrlResult = await sandbox.commands.run("echo $DATABASE_URL");
    const dbUrlVal = dbUrlResult.stdout.trim();
    check("DATABASE_URL is sealed", dbUrlVal.startsWith("osb_sealed_"),
      `got "${dbUrlVal}"`);

    const tempResult = await sandbox.commands.run("echo $TEMP_SECRET");
    const tempVal = tempResult.stdout.trim();
    check("TEMP_SECRET is sealed", tempVal.startsWith("osb_sealed_"),
      `got "${tempVal}"`);
    console.log();

    // -- Test 9: Delete secret --
    bold("--- Test 9: Delete secret ---\n");

    await SecretStore.deleteSecret(storeId!, "TEMP_SECRET");
    green("Deleted TEMP_SECRET");

    const afterDelete = await SecretStore.listSecrets(storeId!);
    const afterNames = afterDelete.map((e) => e.name);
    check("TEMP_SECRET removed", !afterNames.includes("TEMP_SECRET"));
    check("2 secrets remaining", afterDelete.length === 2, `got ${afterDelete.length}`);
    console.log();

    // -- Test 10: Delete secret store --
    bold("--- Test 10: Delete secret store ---\n");

    // Kill sandbox first
    await sandbox.kill();
    green("Sandbox killed");
    sandbox = null;

    await SecretStore.delete(storeId!);
    green("Secret store deleted");

    // Verify it's gone
    try {
      await SecretStore.get(storeId!);
      red("Store should not exist after delete");
      failed++;
    } catch {
      green("Store not found after delete (expected)");
      passed++;
    }
    storeId = null;
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    // Cleanup
    if (sandbox) {
      try { await sandbox.kill(); } catch {}
    }
    if (storeId) {
      try { await SecretStore.delete(storeId); } catch {}
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
