/**
 * Preview URL E2E Test — creates a sandbox, starts an HTTP server,
 * creates a preview URL, and tries to access it.
 *
 * Usage:
 *   OPENCOMPUTER_API_KEY=<key> npx tsx examples/test-preview-urls-e2e.ts
 */

import { Sandbox } from "../src/index";

function green(msg: string) { console.log(`\x1b[32m✓ ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m✗ ${msg}\x1b[0m`); }
function yellow(msg: string) { console.log(`\x1b[33m⚠ ${msg}\x1b[0m`); }
function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       Preview URL E2E Test                       ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let sandbox: Sandbox | null = null;

  try {
    // 1. Create sandbox
    bold("━━━ Step 1: Create sandbox ━━━\n");
    sandbox = await Sandbox.create({ template: "base", timeout: 600 });
    green(`Sandbox: ${sandbox.sandboxId}`);
    dim(`Domain: ${sandbox.domain}`);
    console.log();

    // 2. Start an HTTP server inside the sandbox
    bold("━━━ Step 2: Start HTTP server inside sandbox ━━━\n");

    // Create a simple HTML file
    await sandbox.files.write("/tmp/index.html",
      `<html><body><h1>Hello from sandbox ${sandbox.sandboxId}</h1></body></html>`
    );

    // Try python3 first, then python, then busybox httpd
    const startServer = await sandbox.commands.run(
      "which python3 && nohup python3 -m http.server 8080 --directory /tmp > /dev/null 2>&1 & " +
      "|| which python && nohup python -m http.server 8080 --directory /tmp > /dev/null 2>&1 & " +
      "|| (cd /tmp && nohup busybox httpd -f -p 8080 &)"
    );
    dim(`Server start: exit=${startServer.exitCode}`);

    // Wait for server to start
    await new Promise(r => setTimeout(r, 2000));

    // Verify via the default domain (which we know works)
    dim("Verifying via default domain...");
    const check = await sandbox.commands.run("curl -s http://localhost:8080/index.html || wget -qO- http://localhost:8080/index.html || echo 'NO_HTTP_CLIENT'");
    if (check.stdout.includes("Hello from sandbox")) {
      green("HTTP server running inside sandbox");
    } else {
      yellow("HTTP server may not be running — checking what's available...");
      const which = await sandbox.commands.run("which python3 python busybox curl wget 2>&1; ls /usr/bin/python* 2>&1 || true");
      dim(`Available: ${which.stdout.trim()}`);
    }
    console.log();

    // 3. Create preview URL
    bold("━━━ Step 3: Create preview URL ━━━\n");
    const preview = await sandbox.createPreviewURL();
    green(`Preview URL: https://${preview.hostname}`);
    dim(`SSL status: ${preview.sslStatus}`);
    dim(`CF hostname ID: ${preview.cfHostnameId}`);
    console.log();

    // 4. Wait for CF to activate hostname, then test routing
    bold("━━━ Step 4: Test access via preview URL ━━━\n");
    dim("Waiting for CF custom hostname to activate (may take 30-90s)...");
    console.log();

    const maxAttempts = 20;
    let success = false;
    for (let i = 0; i < maxAttempts; i++) {
      dim(`Attempt ${i + 1}/${maxAttempts}: fetching http://${preview.hostname}/index.html ...`);
      try {
        const resp = await fetch(`http://${preview.hostname}/index.html`, {
          signal: AbortSignal.timeout(10000),
          redirect: "manual",
        });
        dim(`Status: ${resp.status}`);
        if (resp.status === 200) {
          const body = await resp.text();
          if (body.includes("Hello from sandbox")) {
            green(`Preview URL is LIVE! Got sandbox response.`);
            dim(`Body: ${body.trim().substring(0, 100)}`);
          } else {
            green(`Preview URL is routing! (got 200, body may differ)`);
            dim(`Body: ${body.substring(0, 200)}`);
          }
          success = true;
          break;
        } else if (resp.status === 409) {
          dim("CF hostname still initializing...");
        } else if (resp.status === 502) {
          const body = await resp.text().catch(() => "");
          if (body.includes("sandbox") && body.includes("not available")) {
            yellow("Proxy routing works but sandbox unavailable (may need HTTP server)");
            dim(`Body: ${body.substring(0, 200)}`);
          } else {
            dim(`502 — origin error: ${body.substring(0, 100)}`);
          }
        } else if (resp.status === 301 || resp.status === 302) {
          dim(`Redirect to: ${resp.headers.get("location")}`);
        } else {
          const body = await resp.text().catch(() => "");
          dim(`Body: ${body.substring(0, 200)}`);
        }
      } catch (err: any) {
        dim(`Error: ${err.message}`);
      }

      if (i < maxAttempts - 1) {
        await new Promise(r => setTimeout(r, 5000));
      }
    }

    if (!success) {
      yellow("Preview URL didn't return 200 within the test window.");
      dim("CF hostname may need more time. Try manually:");
      dim(`  curl -v http://${preview.hostname}/index.html`);
    }
    console.log();

    // 5. Check SSL status
    bold("━━━ Step 5: Check SSL status ━━━\n");
    const updated = await sandbox.getPreviewURL();
    if (updated) {
      dim(`SSL status: ${updated.sslStatus}`);
      if (updated.sslStatus === "active") {
        green("SSL is active! Try: https://" + updated.hostname);
      } else {
        yellow(`SSL still ${updated.sslStatus} — may take a few minutes.`);
      }
    }
    console.log();

    // Auto-cleanup instead of waiting
    bold("━━━ Cleaning up ━━━\n");

  } catch (err: any) {
    red(`Error: ${err.message}`);
    if (err.stack) dim(err.stack);
  } finally {
    if (sandbox) {
      try {
        await sandbox.deletePreviewURL();
        green("Preview URL deleted");
      } catch {}
      await sandbox.kill();
      green("Sandbox killed");
    }
  }
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
