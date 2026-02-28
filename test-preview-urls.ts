/**
 * Test script for preview URLs with hibernate/wake cycle.
 * Tests idempotent creation, custom domain support, and hibernate persistence.
 *
 * Usage:
 *   OPENCOMPUTER_API_KEY=<key> npx tsx test-preview-urls.ts
 *
 * Optional env vars:
 *   CUSTOM_DOMAIN=mycompany.com   — test custom domain preview URLs
 */

import { Sandbox, type PreviewURLResult } from "./sdks/typescript/src/sandbox";
import * as readline from "readline";

function assert(cond: boolean, msg: string) {
  if (!cond) { console.error(`  FAIL: ${msg}`); process.exit(1); }
  console.log(`  PASS: ${msg}`);
}

function waitForEnter(prompt: string): Promise<void> {
  const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
  return new Promise(resolve => {
    rl.question(prompt, () => { rl.close(); resolve(); });
  });
}

const PORTS = [3000, 8080, 5173];
const CUSTOM_DOMAIN = process.env.CUSTOM_DOMAIN || "";

async function startServers(sandbox: Sandbox) {
  for (const port of PORTS) {
    await sandbox.commands.run(
      `sh -c 'mkdir -p /tmp/www-${port} && cat > /tmp/www-${port}/index.html << "HTMLEOF"
<!DOCTYPE html><html><head><title>Port ${port}</title></head>
<body style="font-family:system-ui;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#0f172a;color:#e2e8f0">
<div style="text-align:center">
<h1 style="color:#3b82f6">Port ${port}</h1>
<p style="color:#64748b;font-size:0.875rem">Each port gets its own preview URL</p>
</div></body></html>
HTMLEOF'`,
    );
    await sandbox.commands.run(`busybox httpd -p ${port} -h /tmp/www-${port}`);
    console.log(`  PASS: server started on port ${port}`);
  }
}

async function main() {
  let sandbox: Sandbox | null = null;

  try {
    // ── 1. Create sandbox ──────────────────────────────────────────────
    console.log("\n1. Creating sandbox...");
    sandbox = await Sandbox.create({ template: "base", timeout: 300 });
    console.log(`   sandboxId = ${sandbox.sandboxId}`);
    assert(sandbox.status === "running", `status = ${sandbox.status}`);

    // ── 2. Start HTTP servers ──────────────────────────────────────────
    console.log(`\n2. Starting HTTP servers on ports ${PORTS.join(", ")}...`);
    await startServers(sandbox);

    // ── 3. Create preview URLs ─────────────────────────────────────────
    console.log("\n3. Creating preview URLs...");
    const previews: PreviewURLResult[] = [];
    for (const port of PORTS) {
      const opts: { port: number; domain?: string } = { port };
      if (CUSTOM_DOMAIN) opts.domain = CUSTOM_DOMAIN;
      const pv = await sandbox.createPreviewURL(opts);
      assert(pv.port === port, `port = ${pv.port}`);
      assert(pv.hostname.includes(sandbox.sandboxId), `hostname contains sandbox ID`);
      if (CUSTOM_DOMAIN) {
        assert(pv.hostname.endsWith(CUSTOM_DOMAIN), `hostname ends with custom domain`);
      }
      if (pv.customHostname) {
        console.log(`    customHostname: ${pv.customHostname}`);
      }
      previews.push(pv);
    }

    // ── 4. Idempotent re-creation ──────────────────────────────────────
    console.log("\n4. Re-creating same preview URLs (idempotent)...");
    for (const port of PORTS) {
      const opts: { port: number; domain?: string } = { port };
      if (CUSTOM_DOMAIN) opts.domain = CUSTOM_DOMAIN;
      const pv = await sandbox.createPreviewURL(opts);
      assert(pv.port === port, `idempotent: port ${port} returned`);
      const original = previews.find(p => p.port === port)!;
      assert(pv.id === original.id, `idempotent: same id for port ${port}`);
      assert(pv.hostname === original.hostname, `idempotent: same hostname for port ${port}`);
    }

    console.log(`\n${"=".repeat(60)}`);
    console.log("  Preview URLs:\n");
    for (const pv of previews) {
      const displayHost = pv.customHostname || pv.hostname;
      console.log(`    port ${pv.port} → https://${displayHost}`);
    }
    console.log(`\n  Dashboard: https://app.opencomputer.dev/sessions/${sandbox.sandboxId}`);
    console.log(`${"=".repeat(60)}\n`);

    // ── 5. List preview URLs ───────────────────────────────────────────
    console.log("5. Listing preview URLs...");
    const listed = await sandbox.listPreviewURLs();
    assert(listed.length === PORTS.length, `list returns ${PORTS.length} URLs`);
    for (const u of listed) {
      if (CUSTOM_DOMAIN) {
        assert(!!u.customHostname, `list: customHostname present for port ${u.port}`);
      }
    }

    await waitForEnter("Check dashboard shows preview URLs, then press Enter to hibernate...\n");

    // ── 6. Hibernate ───────────────────────────────────────────────────
    console.log("\n6. Hibernating sandbox...");
    await sandbox.hibernate();
    console.log("  PASS: hibernated");

    // Verify preview URLs still in DB
    const listHibernated = await sandbox.listPreviewURLs();
    assert(listHibernated.length === PORTS.length, `preview URLs after hibernate = ${listHibernated.length}`);

    await waitForEnter("Check dashboard shows preview URLs while hibernated, then press Enter to wake...\n");

    // ── 7. Wake ────────────────────────────────────────────────────────
    console.log("\n7. Waking sandbox...");
    await sandbox.wake({ timeout: 300 });
    console.log("  PASS: woken");

    // Verify preview URLs still persist
    const listWoken = await sandbox.listPreviewURLs();
    assert(listWoken.length === PORTS.length, `preview URLs after wake = ${listWoken.length}`);

    // Restart servers (processes don't survive hibernate on base template)
    console.log("\n8. Restarting HTTP servers...");
    await startServers(sandbox);

    console.log(`\n${"=".repeat(60)}`);
    console.log("  URLs should still work after wake:\n");
    for (const pv of previews) {
      const displayHost = pv.customHostname || pv.hostname;
      console.log(`    port ${pv.port} → https://${displayHost}`);
    }
    console.log(`\n  Dashboard: https://app.opencomputer.dev/sessions/${sandbox.sandboxId}`);
    console.log(`${"=".repeat(60)}\n`);

    await waitForEnter("Check dashboard + URLs after wake, then press Enter to kill...\n");

    // ── 9. Kill ────────────────────────────────────────────────────────
    console.log("\n9. Killing sandbox...");
    await sandbox.kill();
    sandbox = null;
    console.log("  PASS: sandbox killed");

    console.log("\n All tests passed!\n");
  } catch (err) {
    console.error("\nTest error:", err);
    process.exit(1);
  } finally {
    if (sandbox) {
      console.log(`\nCleaning up sandbox ${sandbox.sandboxId}...`);
      await sandbox.kill().catch(() => {});
    }
  }
}

main();
