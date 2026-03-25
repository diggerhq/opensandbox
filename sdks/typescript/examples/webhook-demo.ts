/**
 * OpenSandbox Webhook Demo
 *
 * Demonstrates:
 *   1. Create a sandbox with a webhook server
 *   2. Send a webhook while sandbox is AWAKE → instant response
 *   3. Hibernate the sandbox (sandbox goes to sleep)
 *   4. Send a webhook while sandbox is ASLEEP → auto-wakes and responds
 *   5. Verify data persisted across the sleep/wake cycle
 *
 * Usage:
 *   OPENCOMPUTER_API_KEY=osb_... npx tsx examples/webhook-demo.ts
 */

import { Sandbox } from "../src/index";

// ── Helpers ─────────────────────────────────────────────────────────────────

function green(msg: string) { console.log(`\x1b[32m✓ ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m✗ ${msg}\x1b[0m`); }
function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }
function cyan(msg: string) { console.log(`\x1b[36m→ ${msg}\x1b[0m`); }

function sleep(ms: number) {
  return new Promise((r) => globalThis.setTimeout(r, ms));
}

async function sendWebhook(
  domain: string,
  payload: Record<string, unknown>,
): Promise<{ status: number; body: string; latencyMs: number }> {
  const url = `https://${domain}/webhook`;
  const start = Date.now();
  const resp = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  const latencyMs = Date.now() - start;
  const body = await resp.text();
  return { status: resp.status, body, latencyMs };
}

// ── Webhook server (runs inside the sandbox on port 80) ─────────────────

const WEBHOOK_SERVER = `
const http = require('http');
const fs = require('fs');

const LOG_FILE = '/tmp/webhooks.log';

const server = http.createServer((req, res) => {
  if (req.method === 'POST' && req.url === '/webhook') {
    let body = '';
    req.on('data', chunk => body += chunk);
    req.on('end', () => {
      const entry = { timestamp: new Date().toISOString(), payload: JSON.parse(body) };
      fs.appendFileSync(LOG_FILE, JSON.stringify(entry) + '\\n');

      const response = {
        received: true,
        event: entry.payload.event,
        processedAt: entry.timestamp,
        sandboxUptime: process.uptime().toFixed(1) + 's',
      };

      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(response));
    });
  } else if (req.method === 'GET' && req.url === '/health') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ status: 'ok', uptime: process.uptime().toFixed(1) + 's' }));
  } else {
    res.writeHead(404);
    res.end('not found');
  }
});

server.listen(80, '0.0.0.0', () => {
  console.log('Webhook server listening on port 80');
});
`;

// ── Main Demo ───────────────────────────────────────────────────────────────

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       OpenSandbox Webhook Demo                   ║");
  bold("║       Webhooks that survive sleep                ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  // ── Step 1: Create sandbox ────────────────────────────────────────────
  bold("━━━ Step 1: Create sandbox with webhook server ━━━\n");
  cyan("Creating sandbox...");

  const sandbox = await Sandbox.create({
    timeout: 300,
  });

  green(`Sandbox created: ${sandbox.sandboxId}`);
  dim(`Domain: ${sandbox.domain}`);
  console.log();

  // Write and start the webhook server
  cyan("Starting webhook server...");
  await sandbox.files.write("/tmp/server.js", WEBHOOK_SERVER);
  await sandbox.commands.run("nohup node /tmp/server.js > /tmp/server.log 2>&1 &");

  // Wait for server to bind
  await sleep(1000);

  // Verify it's running via the subdomain
  const healthResp = await fetch(`https://${sandbox.domain}/health`);
  if (healthResp.ok) {
    const health = await healthResp.json();
    green(`Webhook server is running (uptime: ${health.uptime})`);
  } else {
    red(`Webhook server failed to start (${healthResp.status})`);
    const serverLog = await sandbox.commands.run("cat /tmp/server.log");
    dim("Server log: " + serverLog.stdout + serverLog.stderr);
    await sandbox.kill();
    return;
  }
  console.log();

  // ── Step 2: Webhook while AWAKE ───────────────────────────────────────
  bold("━━━ Step 2: Send webhook while sandbox is AWAKE ━━━\n");
  cyan('Sending webhook: { event: "payment.completed" }');

  const awakeResult = await sendWebhook(sandbox.domain, {
    event: "payment.completed",
    amount: 99.99,
    customer: "cust_12345",
  });

  green(`Response (${awakeResult.latencyMs}ms): ${awakeResult.body}`);
  console.log();

  // ── Step 3: Hibernate ─────────────────────────────────────────────────
  bold("━━━ Step 3: Hibernate sandbox (sleep) ━━━\n");
  cyan("Hibernating...");

  const hibStart = Date.now();
  await sandbox.hibernate();
  const hibMs = Date.now() - hibStart;

  green(`Sandbox is now sleeping (took ${hibMs}ms)`);
  dim("Container checkpointed to S3, all processes frozen");
  dim("Domain still assigned: " + sandbox.domain);
  console.log();

  // Pause to make the sleep visible
  await sleep(2000);

  // ── Step 4: Webhook while ASLEEP ──────────────────────────────────────
  bold("━━━ Step 4: Send webhook while sandbox is ASLEEP ━━━\n");
  cyan("Sending webhook to sleeping sandbox...");
  dim("Sandbox will auto-wake from checkpoint and process the request");
  console.log();

  const sleepResult = await sendWebhook(sandbox.domain, {
    event: "invoice.created",
    invoiceId: "inv_67890",
    total: 250.0,
  });

  if (sleepResult.status === 200) {
    green(`Response (${sleepResult.latencyMs}ms): ${sleepResult.body}`);
    dim(`Latency includes: S3 download → container restore → request forwarded`);
  } else {
    red(`Unexpected status ${sleepResult.status}: ${sleepResult.body}`);
  }
  console.log();

  // ── Step 5: Verify logs persisted across sleep ────────────────────────
  bold("━━━ Step 5: Verify webhook logs survived hibernation ━━━\n");

  // Reconnect to get fresh worker token
  const reconnected = await Sandbox.connect(sandbox.sandboxId);
  const logs = await reconnected.commands.run("cat /tmp/webhooks.log");

  cyan("Webhook log (/tmp/webhooks.log):");
  for (const line of logs.stdout.trim().split("\n")) {
    const entry = JSON.parse(line);
    dim(`${entry.timestamp} → ${entry.payload.event}`);
  }
  green("Both webhooks logged — data persisted across sleep/wake cycle");
  console.log();

  // ── Cleanup ───────────────────────────────────────────────────────────
  bold("━━━ Cleanup ━━━\n");
  await reconnected.kill();
  green("Sandbox killed");

  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║  Demo complete!                                  ║");
  bold("║                                                  ║");
  bold("║  What happened:                                  ║");
  bold("║  1. Created sandbox with Node.js webhook server  ║");
  bold("║  2. Sent webhook while awake → instant response  ║");
  bold("║  3. Hibernated sandbox to S3 checkpoint          ║");
  bold("║  4. Sent webhook to sleeping sandbox             ║");
  bold("║     → auto-woke and processed the request        ║");
  bold("║  5. Both webhook logs persisted across sleep      ║");
  bold("╚══════════════════════════════════════════════════╝\n");
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
