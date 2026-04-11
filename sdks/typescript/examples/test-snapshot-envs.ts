/**
 * Snapshot Envs & Secret-Sealing Test
 *
 * Regression coverage for two bugs that previously interacted around
 * Sandbox.create({ envs }):
 *
 *   1. The snapshot/fork path silently dropped user-supplied envs because
 *      createFromCheckpointCore re-bound only `Timeout` from the request body
 *      and forwarded `originalCfg.Envs` from the checkpoint instead.
 *
 *   2. Every env var was tokenized into an `osb_sealed_…` value by the
 *      secrets proxy regardless of source, so `echo $TEST_VAR` inside the
 *      guest returned the sealed token rather than the plaintext the user
 *      passed. Sealing should only apply to envs that originated from a
 *      SecretStore — those are the ones the MITM proxy needs to swap on
 *      outbound HTTPS.
 *
 * What this test asserts:
 *   - Plain `Sandbox.create({ envs })` → guest sees plaintext.
 *   - `Sandbox.create({ snapshot, envs })` → user envs survive the fork AND
 *     reach the guest as plaintext.
 *   - `Sandbox.create({ secretStore, envs })` → user-supplied envs are
 *     plaintext, store-derived envs are sealed (`osb_sealed_…`).
 *
 * Usage:
 *   npx tsx examples/test-snapshot-envs.ts
 */

import { Sandbox, SecretStore } from "../src/index";
import { Snapshots, Image } from "../src/node";

function green(msg: string) { console.log(`\x1b[32m✓ ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m✗ ${msg}\x1b[0m`); }
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

// Unique names so concurrent runs / reruns don't collide.
const RUN_ID = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
const SNAPSHOT_NAME = `test-snapshot-envs-${RUN_ID}`;
const STORE_NAME = `test-snapshot-envs-store-${RUN_ID}`;
const USER_VALUE = "plaintext-from-user";
const SECRET_VALUE = "plaintext-from-store";

async function readVar(sb: Sandbox, name: string): Promise<string> {
  // -n to avoid trailing newline; printenv would also work but echo matches
  // the original repro and is universally available in the base image.
  const out = await sb.commands.run(`echo -n "$${name}"`);
  return out.stdout;
}

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       Snapshot Envs & Secret Sealing Test        ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  const snapshots = new Snapshots();
  let storeId: string | null = null;
  let createdSnapshot = false;
  const sandboxes: Sandbox[] = [];

  try {
    // ── Test 1: Plain create — user envs reach guest as plaintext ──
    bold("━━━ Test 1: envs without snapshot or secret store ━━━\n");

    const a = await Sandbox.create({ timeout: 60, envs: { TEST_VAR: USER_VALUE } });
    sandboxes.push(a);
    const aVal = await readVar(a, "TEST_VAR");
    dim(`TEST_VAR=${JSON.stringify(aVal)}`);
    check("plain create: user env is plaintext", aVal === USER_VALUE,
      `expected ${JSON.stringify(USER_VALUE)}, got ${JSON.stringify(aVal)}`);
    check("plain create: user env is NOT sealed", !aVal.startsWith("osb_sealed_"));
    console.log();

    // ── Test 2: Snapshot fork preserves user envs as plaintext ──
    // Bug #1 regression: this returned "" before the fix because the fork
    // path dropped envs from the request body.
    // Bug #2 regression: even after #1 was fixed it returned an osb_sealed_
    // token because every env was tokenized unconditionally.
    bold("━━━ Test 2: envs survive snapshot fork as plaintext ━━━\n");

    dim(`creating snapshot ${SNAPSHOT_NAME}...`);
    await snapshots.create({ name: SNAPSHOT_NAME, image: Image.base() });
    createdSnapshot = true;

    const b = await Sandbox.create({
      snapshot: SNAPSHOT_NAME,
      timeout: 60,
      envs: { TEST_VAR: USER_VALUE },
    });
    sandboxes.push(b);
    const bVal = await readVar(b, "TEST_VAR");
    dim(`TEST_VAR=${JSON.stringify(bVal)}`);
    check("snapshot fork: user env survives the fork", bVal.length > 0,
      "got empty string — bug #1 (createFromCheckpointCore drops req body)");
    check("snapshot fork: user env is NOT sealed", !bVal.startsWith("osb_sealed_"),
      "bug #2 (CreateSealedEnvs tokenizes every env)");
    check("snapshot fork: user env equals plaintext", bVal === USER_VALUE,
      `expected ${JSON.stringify(USER_VALUE)}, got ${JSON.stringify(bVal)}`);
    console.log();

    // ── Test 3: Secret store envs are still sealed; user envs are not ──
    // Selective sealing: passing both a SecretStore *and* explicit envs in
    // the same create should produce mixed plaintext/sealed values.
    bold("━━━ Test 3: secret store envs sealed, user envs plaintext ━━━\n");

    const store = await SecretStore.create({ name: STORE_NAME });
    storeId = store.id;
    await SecretStore.setSecret(storeId, "STORE_VAR", SECRET_VALUE);

    const c = await Sandbox.create({
      timeout: 60,
      secretStore: STORE_NAME,
      envs: { USER_VAR: USER_VALUE },
    });
    sandboxes.push(c);
    const userVal = await readVar(c, "USER_VAR");
    const storeVal = await readVar(c, "STORE_VAR");
    dim(`USER_VAR=${JSON.stringify(userVal)}`);
    dim(`STORE_VAR=${JSON.stringify(storeVal)}`);
    check("mixed: user env is plaintext", userVal === USER_VALUE,
      `expected ${JSON.stringify(USER_VALUE)}, got ${JSON.stringify(userVal)}`);
    check("mixed: user env is NOT sealed", !userVal.startsWith("osb_sealed_"));
    check("mixed: store env IS sealed", storeVal.startsWith("osb_sealed_"),
      `store-derived env should be tokenized by secrets proxy, got ${JSON.stringify(storeVal)}`);
    console.log();

    // ── Test 4: secretStore + snapshot must NOT silently leak secrets ──
    // Regression guard. The combination used to silently drop the user's
    // secret store on fork; the bug-#1 fix turned that silent drop into a
    // silent plaintext leak (the parent handler eagerly resolved the store
    // into cfg.Envs, which the fix then merged into the fork — but the
    // seal-set is computed from the snapshot's *original* store, so the
    // smuggled values reached the guest unsealed).
    //
    // The supported semantics are: a fork inherits the snapshot's secret
    // store and cannot override it. The API must reject the combination
    // explicitly so users can't trip the leak by accident.
    bold("━━━ Test 4: secretStore + snapshot is rejected (no leak) ━━━\n");

    let rejected = false;
    let leaked = false;
    let leakedValue = "";
    let d: Sandbox | null = null;
    try {
      d = await Sandbox.create({
        snapshot: SNAPSHOT_NAME,
        timeout: 60,
        secretStore: STORE_NAME,
      });
      // Reached this point: API accepted the combo. Verify it didn't leak.
      sandboxes.push(d);
      leakedValue = await readVar(d, "STORE_VAR");
      leaked = leakedValue === SECRET_VALUE;
    } catch (err: any) {
      rejected = true;
      dim(`API rejected combo: ${err.message}`);
    }
    check(
      "secretStore + snapshot is rejected at the API edge",
      rejected,
      leaked
        ? `LEAK: STORE_VAR reached the guest as plaintext (${JSON.stringify(leakedValue)})`
        : "API accepted the combo without leaking, but the supported contract is rejection",
    );
    check(
      "secretStore + snapshot does not leak plaintext into the guest",
      !leaked,
      `STORE_VAR=${JSON.stringify(leakedValue)} — secret-store value reached the guest unsealed`,
    );
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    for (const sb of sandboxes) {
      await sb.kill().catch(() => {});
    }
    if (createdSnapshot) {
      await snapshots.delete(SNAPSHOT_NAME).catch(() => {});
    }
    if (storeId) {
      await SecretStore.delete(storeId).catch(() => {});
    }
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
