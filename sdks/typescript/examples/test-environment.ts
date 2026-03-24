/**
 * Environment & Sandbox Configuration Test
 *
 * Verifies the sandbox environment is set up correctly for normal development:
 *   - Running as root (can install packages, modify system files)
 *   - HOME=/root (standard root home)
 *   - /workspace is a separate persistent disk
 *   - npm, pip, git, python, node all work
 *   - npm install works in /workspace
 *
 * Usage:
 *   npx tsx examples/test-environment.ts
 */

import { Sandbox } from "../src/index";

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

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       Environment & Sandbox Config Test          ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let sandbox: Sandbox | null = null;

  try {
    sandbox = await Sandbox.create({ template: "base", timeout: 120 });
    green(`Created sandbox: ${sandbox.sandboxId}`);
    console.log();

    // ── Test 1: Running as root ─────────────────────────────────
    bold("━━━ Test 1: Running as root ━━━\n");

    const whoami = await sandbox.commands.run("whoami");
    check("Running as root", whoami.stdout.trim() === "root",
      `got "${whoami.stdout.trim()}"`);

    const idResult = await sandbox.commands.run("id -u");
    check("UID is 0", idResult.stdout.trim() === "0",
      `got "${idResult.stdout.trim()}"`);

    // ── Test 2: HOME is /root ───────────────────────────────────
    bold("━━━ Test 2: HOME environment ━━━\n");

    const homeResult = await sandbox.commands.run("echo $HOME");
    check("HOME is /root", homeResult.stdout.trim() === "/root",
      `got "${homeResult.stdout.trim()}"`);

    // ── Test 3: /workspace is a separate disk ───────────────────
    bold("━━━ Test 3: /workspace filesystem ━━━\n");

    const dfResult = await sandbox.commands.run("df -h /workspace | tail -1");
    const dfLine = dfResult.stdout.trim();
    dim(`df output: ${dfLine}`);
    check("/workspace is NOT on /dev/root", !dfLine.startsWith("/dev/root"));
    check("/workspace is mounted", dfLine.includes("/workspace"));

    const wsExists = await sandbox.commands.run("test -d /workspace && echo yes || echo no");
    check("/workspace directory exists", wsExists.stdout.trim() === "yes");
    console.log();

    // ── Test 4: Can install system packages ─────────────────────
    bold("━━━ Test 4: Package installation (apt) ━━━\n");

    const aptResult = await sandbox.commands.run(
      "apt-get update -qq > /dev/null 2>&1 && apt-get install -y -qq cowsay > /dev/null 2>&1 && /usr/games/cowsay moo | head -1",
      { timeout: 30 }
    );
    check("apt install works", aptResult.exitCode === 0,
      `exit code ${aptResult.exitCode}`);
    console.log();

    // ── Test 5: npm install in /workspace ────────────────────────
    bold("━━━ Test 5: npm install in /workspace ━━━\n");

    await sandbox.commands.run("mkdir -p /workspace/npm-test");
    await sandbox.files.write("/workspace/npm-test/package.json", JSON.stringify({
      name: "test",
      version: "1.0.0",
      dependencies: { "is-odd": "3.0.1" }
    }));

    const installResult = await sandbox.commands.run(
      "cd /workspace/npm-test && npm install 2>&1 | tail -1",
      { timeout: 30 }
    );
    dim(`npm install exit code: ${installResult.exitCode}`);
    check("npm install succeeds", installResult.exitCode === 0);

    const nodeModules = await sandbox.commands.run(
      "test -d /workspace/npm-test/node_modules && echo yes || echo no"
    );
    check("node_modules created in /workspace", nodeModules.stdout.trim() === "yes");

    const requireTest = await sandbox.commands.run(
      "cd /workspace/npm-test && node -e \"require('is-odd'); console.log('ok')\"",
    );
    check("Installed package works", requireTest.stdout.trim() === "ok");
    console.log();

    // ── Test 6: pip install ─────────────────────────────────────
    bold("━━━ Test 6: pip install ━━━\n");

    const pipResult = await sandbox.commands.run(
      "pip3 install -q requests 2>/dev/null && python3 -c \"import requests; print(requests.__version__)\"",
      { timeout: 30 }
    );
    check("pip install + import works", pipResult.exitCode === 0 && pipResult.stdout.trim().length > 0,
      `exit code ${pipResult.exitCode}`);
    dim(`requests version: ${pipResult.stdout.trim()}`);
    console.log();

    // ── Test 7: Tools available ─────────────────────────────────
    bold("━━━ Test 7: Development tools ━━━\n");

    const tools = ["git", "python3", "node", "npm", "curl", "jq"];
    for (const tool of tools) {
      const r = await sandbox.commands.run(`which ${tool}`);
      check(`${tool} available`, r.exitCode === 0, r.stdout.trim());
    }
    console.log();

    // Clean up
    await sandbox.commands.run("rm -rf /workspace/npm-test");

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    if (sandbox) {
      await sandbox.kill();
      green("Sandbox killed");
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
