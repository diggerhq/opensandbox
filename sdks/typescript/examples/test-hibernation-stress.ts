/**
 * Hibernation Stress Test
 *
 * Tests:
 *   1. Multiple hibernate/wake cycles on same sandbox
 *   2. Large state persistence across hibernation
 *   3. Rapid auto-wake via HTTP after hibernation
 *   4. Process state survives across cycles
 *
 * Usage:
 *   npx tsx examples/test-hibernation-stress.ts
 */

import { Sandbox } from "../src/index";

function green(msg: string) { console.log(`\x1b[32m✓ ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m✗ ${msg}\x1b[0m`); }
function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }
function cyan(msg: string) { console.log(`\x1b[36m→ ${msg}\x1b[0m`); }

function sleep(ms: number) {
  return new Promise((r) => globalThis.setTimeout(r, ms));
}

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
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       Hibernation Stress Test                    ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let sandbox: Sandbox | null = null;

  try {
    // ── Setup: Create sandbox with a persistent server ──────────────
    bold("━━━ Setup: Create sandbox with counter server ━━━\n");

    sandbox = await Sandbox.create({ timeout: 300 });
    green(`Created: ${sandbox.sandboxId}`);
    dim(`Domain: ${sandbox.domain}`);

    // Write a server that maintains an in-memory counter
    const SERVER_CODE = `
const http = require('http');
const fs = require('fs');
let counter = 0;
try { counter = parseInt(fs.readFileSync('/tmp/counter.txt', 'utf8')) || 0; } catch {}
const server = http.createServer((req, res) => {
  if (req.url === '/increment') {
    counter++;
    fs.writeFileSync('/tmp/counter.txt', String(counter));
    res.writeHead(200, {'Content-Type':'application/json'});
    res.end(JSON.stringify({counter}));
  } else if (req.url === '/health') {
    res.writeHead(200, {'Content-Type':'application/json'});
    res.end(JSON.stringify({counter, uptime: process.uptime().toFixed(1)+'s'}));
  } else {
    res.writeHead(404); res.end('not found');
  }
});
server.listen(80, '0.0.0.0', () => console.log('Counter server on port 80'));
`;

    await sandbox.files.write("/tmp/counter-server.js", SERVER_CODE);
    await sandbox.commands.run("nohup node /tmp/counter-server.js > /tmp/server.log 2>&1 &");
    await sleep(1500);

    // Verify server is up
    const healthResp = await fetch(`https://${sandbox.domain}/health`);
    check("Counter server started", healthResp.ok);
    const health = await healthResp.json();
    dim(`Initial state: counter=${health.counter}, uptime=${health.uptime}`);
    console.log();

    // ── Test 1: Multiple hibernate/wake cycles ──────────────────────
    bold("━━━ Test 1: Multiple hibernate/wake cycles (3x) ━━━\n");
    const cycleTimes: { hibernate: number; wake: number }[] = [];

    for (let cycle = 1; cycle <= 3; cycle++) {
      cyan(`Cycle ${cycle}/3: Incrementing counter...`);

      // Increment counter before hibernate
      const incResp = await fetch(`https://${sandbox.domain}/increment`);
      const incData = await incResp.json();
      dim(`Counter = ${incData.counter}`);

      // Hibernate
      cyan(`Cycle ${cycle}/3: Hibernating...`);
      const hibStart = Date.now();
      await sandbox.hibernate();
      const hibMs = Date.now() - hibStart;

      await sleep(1000);

      // Wake
      cyan(`Cycle ${cycle}/3: Waking...`);
      const wakeStart = Date.now();
      await sandbox.wake();
      const wakeMs = Date.now() - wakeStart;

      // Need to wait a moment for server to be ready after restore
      await sleep(2000);

      cycleTimes.push({ hibernate: hibMs, wake: wakeMs });
      dim(`Hibernate: ${hibMs}ms, Wake: ${wakeMs}ms`);

      // Verify counter persisted
      const postWakeResp = await fetch(`https://${sandbox.domain}/health`);
      if (postWakeResp.ok) {
        const postWake = await postWakeResp.json();
        check(`Cycle ${cycle}: Counter persisted (${postWake.counter})`, postWake.counter === cycle);
      } else {
        check(`Cycle ${cycle}: Server responded after wake`, false, `status ${postWakeResp.status}`);
      }
    }

    console.log();
    dim("Cycle timing summary:");
    cycleTimes.forEach((t, i) => dim(`  Cycle ${i + 1}: hibernate=${t.hibernate}ms, wake=${t.wake}ms`));
    console.log();

    // ── Test 2: Large state persistence ─────────────────────────────
    bold("━━━ Test 2: Large state persistence across hibernation ━━━\n");

    // Write a 500KB file
    const largeData = "A".repeat(500 * 1024);
    cyan("Writing 500KB file...");
    await sandbox.files.write("/tmp/large-state.bin", largeData);

    // Write 100 small files
    cyan("Writing 100 small files...");
    await sandbox.commands.run(
      "for i in $(seq 1 100); do echo \"file-content-$i\" > /tmp/batch-$i.txt; done",
    );

    // Hibernate with all this state
    cyan("Hibernating with large state...");
    const bigHibStart = Date.now();
    await sandbox.hibernate();
    const bigHibMs = Date.now() - bigHibStart;
    dim(`Hibernate with large state: ${bigHibMs}ms`);

    await sleep(1000);

    // Wake
    cyan("Waking...");
    const bigWakeStart = Date.now();
    await sandbox.wake();
    const bigWakeMs = Date.now() - bigWakeStart;
    dim(`Wake with large state: ${bigWakeMs}ms`);
    await sleep(1500);

    // Verify large file
    const largeContent = await sandbox.files.read("/tmp/large-state.bin");
    check("500KB file persisted", largeContent.length === largeData.length, `got ${largeContent.length} bytes`);
    check("500KB file content intact", largeContent === largeData);

    // Verify batch files (spot check)
    const file1 = await sandbox.files.read("/tmp/batch-1.txt");
    check("Batch file 1 persisted", file1.trim() === "file-content-1", file1.trim());
    const file50 = await sandbox.files.read("/tmp/batch-50.txt");
    check("Batch file 50 persisted", file50.trim() === "file-content-50", file50.trim());
    const file100 = await sandbox.files.read("/tmp/batch-100.txt");
    check("Batch file 100 persisted", file100.trim() === "file-content-100", file100.trim());
    console.log();

    // ── Test 3: Rapid auto-wake ─────────────────────────────────────
    bold("━━━ Test 3: Rapid auto-wake via HTTP ━━━\n");

    // Hibernate
    cyan("Hibernating for auto-wake test...");
    await sandbox.hibernate();
    await sleep(1000);

    // Immediately hit the domain - should trigger auto-wake
    cyan("Sending HTTP request to sleeping sandbox...");
    const autoWakeStart = Date.now();
    const autoWakeResp = await fetch(`https://${sandbox.domain}/health`);
    const autoWakeMs = Date.now() - autoWakeStart;

    check("Auto-wake HTTP returned 200", autoWakeResp.ok, `status ${autoWakeResp.status}`);
    if (autoWakeResp.ok) {
      const autoWakeData = await autoWakeResp.json();
      check("Counter still intact after auto-wake", autoWakeData.counter === 3, `counter=${autoWakeData.counter}`);
      dim(`Auto-wake latency: ${autoWakeMs}ms`);
    }
    console.log();

    // ── Test 4: Process state survives ──────────────────────────────
    bold("━━━ Test 4: Process state survives hibernation ━━━\n");

    // Update sandbox reference after auto-wake restored it
    sandbox = await Sandbox.connect(sandbox.sandboxId);

    // Get PID of node server before hibernate
    const pidBefore = await sandbox.commands.run("cat /tmp/server.log | head -1");
    dim(`Server log: ${pidBefore.stdout.trim()}`);

    // Increment counter to 4
    await fetch(`https://${sandbox.domain}/increment`);
    const preHibHealth = await fetch(`https://${sandbox.domain}/health`);
    const preHibData = await preHibHealth.json();
    check("Counter at 4 before final hibernate", preHibData.counter === 4, `counter=${preHibData.counter}`);

    // One more hibernate/wake cycle (use auto-wake via HTTP to avoid restore conflict)
    await sandbox.hibernate();
    await sleep(1000);

    // Auto-wake by hitting the domain instead of explicit wake()
    const finalHealth = await fetch(`https://${sandbox.domain}/health`);
    if (finalHealth.ok) {
      const finalData = await finalHealth.json();
      check("Counter persisted through final cycle", finalData.counter === 4, `counter=${finalData.counter}`);
      green(`Final state: counter=${finalData.counter}, uptime=${finalData.uptime}`);
    } else {
      check("Final health check", false, `status ${finalHealth.status}`);
    }
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    if (sandbox) {
      try {
        // Re-connect in case wake changed state
        const s = await Sandbox.connect(sandbox.sandboxId);
        await s.kill();
        green("Sandbox killed");
      } catch {
        dim("Sandbox may have already been cleaned up");
      }
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
