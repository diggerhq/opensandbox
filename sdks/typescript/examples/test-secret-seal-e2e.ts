/**
 * End-to-end secret-seal verification.
 *
 * Single script covering all the things that have silently broken before:
 *   1. Secrets land in the guest as `osb_sealed_*` tokens, never plaintext
 *      (echo, file write+read, hex roundtrip).
 *   2. Per-secret `allowedHosts` is honored on outbound: a secret marked
 *      allowed for httpbin.org IS substituted when curling httpbin.org;
 *      a secret NOT allowed for httpbin.org is left as the literal token.
 *   3. Store-level `egressAllowlist` blocks connections to disallowed hosts
 *      entirely (regardless of any secret).
 *
 * Usage:
 *   OPENCOMPUTER_API_URL=... OPENCOMPUTER_API_KEY=... npx tsx examples/test-secret-seal-e2e.ts
 */

import { Sandbox, SecretStore } from "../src/index";
import { randomBytes } from "node:crypto";

const ALLOWED_VAL = `allowed_${randomBytes(12).toString("hex")}`;
const DENIED_VAL = `denied_${randomBytes(12).toString("hex")}`;

let storeId: string | null = null;
let sandbox: Sandbox | null = null;
let forkedSandbox: Sandbox | null = null;
let passed = 0;
let failed = 0;

const green = (s: string) => console.log(`\x1b[32m\u2713 ${s}\x1b[0m`);
const red = (s: string, detail?: string) =>
  console.log(`\x1b[31m\u2717 ${s}${detail ? ` — ${detail}` : ""}\x1b[0m`);
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

async function execStdout(cmd: string): Promise<string> {
  const r = await sandbox!.exec.run(cmd);
  return r.stdout;
}

async function main() {
  const storeName = `seal-e2e-${Date.now()}`;
  try {
    bold("\n=== 1. Setup: store + secrets ===\n");

    // Store-level egressAllowlist: only httpbin.org reachable.
    // example.com is intentionally OUT of the allowlist to test blocking.
    const store = await SecretStore.create({
      name: storeName,
      egressAllowlist: ["httpbin.org"],
    });
    storeId = store.id;
    dim(`Store: ${storeName} (id=${store.id})`);

    // Two secrets with different per-secret allowedHosts:
    //   ALLOWED — should be substituted ONLY on httpbin.org
    //   DENIED  — allowed on a host the sandbox can't reach, so should
    //             never be substituted on httpbin.org
    await SecretStore.setSecret(storeId, "SECRET_ALLOWED", ALLOWED_VAL, {
      allowedHosts: ["httpbin.org"],
    });
    await SecretStore.setSecret(storeId, "SECRET_DENIED", DENIED_VAL, {
      allowedHosts: ["api.example.com"],
    });
    green("Created 2 secrets with distinct allowedHosts");

    bold("\n=== 2. Create sandbox bound to store ===\n");
    sandbox = await Sandbox.create({ secretStore: storeName, timeout: 180 });
    dim(`Sandbox: ${sandbox.sandboxId}`);
    green("Sandbox created");

    bold("\n=== 3. In-guest: secrets must be sealed tokens ===\n");

    const allowedEcho = (await execStdout("printf %s \"$SECRET_ALLOWED\"")).trim();
    const deniedEcho = (await execStdout("printf %s \"$SECRET_DENIED\"")).trim();

    check(
      "SECRET_ALLOWED is sealed in env",
      allowedEcho.startsWith("osb_sealed_") && !allowedEcho.includes(ALLOWED_VAL),
      `got "${allowedEcho}"`,
    );
    check(
      "SECRET_DENIED is sealed in env",
      deniedEcho.startsWith("osb_sealed_") && !deniedEcho.includes(DENIED_VAL),
      `got "${deniedEcho}"`,
    );

    // file roundtrip — different agent code path than env echo
    await sandbox.exec.run('printf %s "$SECRET_ALLOWED" > /tmp/s.txt');
    const fileBuf = await sandbox.files.read("/tmp/s.txt");
    const fileVal = (typeof fileBuf === "string" ? fileBuf : Buffer.from(fileBuf).toString("utf-8")).trim();
    check(
      "file write+read of secret stays sealed",
      fileVal.startsWith("osb_sealed_") && !fileVal.includes(ALLOWED_VAL),
      `got "${fileVal}"`,
    );

    bold("\n=== 4. Egress: per-secret allowedHosts on httpbin.org ===\n");

    // Single httpbin call carries BOTH headers. httpbin echoes them back in
    // its JSON response. Expected behavior:
    //   X-Allowed → real value (proxy substituted, host matches allowedHosts)
    //   X-Denied  → still the sealed token (proxy did NOT substitute)
    const httpbinOut = await execStdout(
      `curl -sS -m 10 https://httpbin.org/headers -H "X-Allowed: $SECRET_ALLOWED" -H "X-Denied: $SECRET_DENIED"`,
    );
    dim(`httpbin response (first 400 chars):`);
    dim(httpbinOut.replace(/\s+/g, " ").slice(0, 400));

    check(
      "httpbin echoed REAL value for SECRET_ALLOWED (substituted on allowed host)",
      httpbinOut.includes(ALLOWED_VAL),
      "expected substitution on httpbin.org since it's in SECRET_ALLOWED.allowedHosts",
    );
    check(
      "httpbin did NOT echo real value for SECRET_DENIED",
      !httpbinOut.includes(DENIED_VAL),
      "real DENIED value leaked to httpbin even though it's not in SECRET_DENIED.allowedHosts",
    );
    check(
      "httpbin echoed sealed token literal for SECRET_DENIED",
      httpbinOut.includes("osb_sealed_") && /X-Denied[^\n]*osb_sealed_/i.test(httpbinOut),
      "expected literal osb_sealed_* token in X-Denied header",
    );

    bold("\n=== 5. Egress: store-level egressAllowlist blocks unrelated hosts ===\n");

    // example.com is NOT in egressAllowlist=["httpbin.org"], so the proxy
    // should refuse the CONNECT entirely. Curl exits non-zero / no body.
    const blockedOut = await sandbox.exec.run(
      `curl -sS -m 10 -o /tmp/blocked.body -w '%{http_code}\\n' https://example.com/ ; echo "exit=$?"`,
    );
    const blockedBody = await sandbox.exec.run("cat /tmp/blocked.body 2>/dev/null || true");
    dim(`curl status line: ${blockedOut.stdout.trim().replace(/\n/g, " | ")}`);
    dim(`body bytes: ${blockedBody.stdout.length}`);

    const reachedExample = /<title>Example Domain<\/title>/i.test(blockedBody.stdout);
    check(
      "example.com (not in egressAllowlist) is BLOCKED",
      !reachedExample,
      "fetched example.com despite it not being in the store's egressAllowlist",
    );

    // Sanity: an allowed host still works (proves we didn't just kill all egress)
    const sanity = await execStdout(`curl -sS -m 10 -o /dev/null -w '%{http_code}' https://httpbin.org/get`);
    check("sanity: httpbin.org (in egressAllowlist) is reachable", sanity.trim() === "200", `got "${sanity.trim()}"`);

    bold("\n=== 6. Snapshot/fork inheritance: secret store binds at first creation ===\n");

    // Take a checkpoint of the running sandbox, wait for it to be ready,
    // then fork. The fork must inherit the secret store binding without the
    // user passing secretStore again — and the seal/proxy semantics must
    // continue to hold on the fork.
    dim("Creating checkpoint of running sandbox...");
    const cp = await sandbox.createCheckpoint(`seal-e2e-cp-${Date.now()}`);
    dim(`Checkpoint id: ${cp.id}`);

    // Poll until ready — generous timeout because S3 upload can be slow on
    // dev boxes. Print status periodically so a hang is visible.
    let cpReady = false;
    let lastStatus: string | undefined;
    for (let i = 0; i < 180; i++) {
      const list = await sandbox.listCheckpoints();
      const found = list.find((c) => c.id === cp.id);
      if (found?.status !== lastStatus) {
        dim(`  [t=${i}s] checkpoint status: ${found?.status ?? "(missing)"}`);
        lastStatus = found?.status;
      }
      if (found?.status === "ready") { cpReady = true; break; }
      if (found?.status === "failed") break;
      await new Promise((r) => setTimeout(r, 1000));
    }
    check("checkpoint reached ready state", cpReady, `last status: ${lastStatus}`);
    if (!cpReady) throw new Error(`checkpoint never became ready (last status: ${lastStatus})`);

    // Mutate the secret store BETWEEN snapshot and fork. The fork should see
    // the new value because we re-resolve the store name fresh on fork load.
    const ROTATED_VAL = `rotated_${randomBytes(12).toString("hex")}`;
    await SecretStore.setSecret(storeId!, "SECRET_ALLOWED", ROTATED_VAL, {
      allowedHosts: ["httpbin.org"],
    });
    dim(`Rotated SECRET_ALLOWED to ${ROTATED_VAL}`);

    // Fork. The SDK's createFromCheckpoint takes no secretStore — inheritance
    // is the only contract.
    forkedSandbox = await Sandbox.createFromCheckpoint(cp.id, { timeout: 180 });
    dim(`Forked sandbox: ${forkedSandbox.sandboxId}`);
    check("forked sandbox created without re-passing secretStore", !!forkedSandbox.sandboxId);

    // In-guest: the inherited secret must still be sealed (token, not value)
    const forkEcho = (await forkedSandbox.exec.run('printf %s "$SECRET_ALLOWED"')).stdout.trim();
    check(
      "forked sandbox: SECRET_ALLOWED is sealed (inherited)",
      forkEcho.startsWith("osb_sealed_") && !forkEcho.includes(ROTATED_VAL) && !forkEcho.includes(ALLOWED_VAL),
      `got "${forkEcho}"`,
    );

    // The DENIED secret must also have inherited
    const forkDeniedEcho = (await forkedSandbox.exec.run('printf %s "$SECRET_DENIED"')).stdout.trim();
    check(
      "forked sandbox: SECRET_DENIED is sealed (inherited)",
      forkDeniedEcho.startsWith("osb_sealed_") && !forkDeniedEcho.includes(DENIED_VAL),
      `got "${forkDeniedEcho}"`,
    );

    // Egress on the fork: hit httpbin and assert
    //   - SECRET_ALLOWED is substituted with the *ROTATED* value (proves we
    //     re-resolved the store fresh, instead of replaying baked-in values)
    //   - SECRET_DENIED is left as the literal sealed token (per-host filter still active)
    const forkHttpbinOut = (await forkedSandbox.exec.run(
      `curl -sS -m 10 https://httpbin.org/headers -H "X-Allowed: $SECRET_ALLOWED" -H "X-Denied: $SECRET_DENIED"`,
    )).stdout;
    dim(`forked httpbin response (first 400 chars):`);
    dim(forkHttpbinOut.replace(/\s+/g, " ").slice(0, 400));

    check(
      "forked sandbox: httpbin sees ROTATED value for SECRET_ALLOWED",
      forkHttpbinOut.includes(ROTATED_VAL),
      "fork should re-resolve the store fresh, picking up the rotated secret value",
    );
    check(
      "forked sandbox: httpbin does NOT see old (pre-rotation) value",
      !forkHttpbinOut.includes(ALLOWED_VAL),
      "fork served stale baked-in value instead of re-resolving",
    );
    check(
      "forked sandbox: SECRET_DENIED still not substituted on httpbin",
      !forkHttpbinOut.includes(DENIED_VAL),
      "per-secret allowedHosts filter regressed on the fork path",
    );

    // Egress allowlist still enforced on the fork
    const forkBlocked = await forkedSandbox.exec.run(
      `curl -sS -m 10 -o /tmp/blocked.body -w '%{http_code}' https://example.com/ ; cat /tmp/blocked.body 2>/dev/null || true`,
    );
    check(
      "forked sandbox: example.com still blocked by inherited egressAllowlist",
      !/<title>Example Domain<\/title>/i.test(forkBlocked.stdout),
      "egress allowlist not inherited on fork",
    );
  } catch (err: any) {
    red(`Fatal: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    bold("\n=== Cleanup ===\n");
    if (forkedSandbox) {
      try { await forkedSandbox.kill(); green("Forked sandbox killed"); } catch (e: any) { red(`fork kill failed: ${e.message}`); }
    }
    if (sandbox) {
      try {
        await sandbox.kill();
        green("Sandbox killed");
      } catch (e: any) {
        red(`kill failed: ${e.message}`);
      }
    }
    if (storeId) {
      try {
        await SecretStore.delete(storeId);
        green("Store deleted");
      } catch (e: any) {
        red(`store delete failed: ${e.message}`);
      }
    }
  }

  bold("\n========================================");
  bold(` Results: ${passed} passed, ${failed} failed`);
  bold("========================================\n");
  if (failed > 0) process.exit(1);
}

main().catch((err) => {
  console.error("Unhandled:", err);
  process.exit(1);
});
