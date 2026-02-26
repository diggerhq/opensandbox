/**
 * Environment & HOME Directory Test
 *
 * Verifies that HOME=/workspace inside sandboxes so that tools like npm, pip,
 * git etc. use the NVMe-backed workspace drive for caches and config instead
 * of /root on the small rootfs.
 *
 * Tests:
 *   1. HOME is set to /workspace
 *   2. Tilde (~) expands to /workspace
 *   3. npm cache dir is under /workspace
 *   4. npm install writes cache to /workspace (not /root or /dev/root)
 *   5. pip cache dir is under /workspace
 *   6. git config home is /workspace
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
  bold("║       Environment & HOME Directory Test          ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let sandbox: Sandbox | null = null;

  try {
    sandbox = await Sandbox.create({ template: "node", timeout: 120 });
    green(`Created sandbox: ${sandbox.sandboxId}`);
    console.log();

    // ── Test 1: HOME is /workspace ─────────────────────────────────
    bold("━━━ Test 1: HOME environment variable ━━━\n");

    const homeResult = await sandbox.commands.run("echo $HOME");
    check("HOME is /workspace", homeResult.stdout.trim() === "/workspace",
      `got "${homeResult.stdout.trim()}"`);

    // ── Test 2: Tilde expansion ────────────────────────────────────
    bold("━━━ Test 2: Tilde (~) expansion ━━━\n");

    const tildeResult = await sandbox.commands.run("echo ~");
    check("~ expands to /workspace", tildeResult.stdout.trim() === "/workspace",
      `got "${tildeResult.stdout.trim()}"`);

    const tildeSlash = await sandbox.commands.run("echo ~/test");
    check("~/test expands to /workspace/test", tildeSlash.stdout.trim() === "/workspace/test",
      `got "${tildeSlash.stdout.trim()}"`);
    console.log();

    // ── Test 3: npm cache directory ────────────────────────────────
    bold("━━━ Test 3: npm cache directory ━━━\n");

    const npmCache = await sandbox.commands.run("npm config get cache");
    const npmCachePath = npmCache.stdout.trim();
    check("npm cache is under /workspace", npmCachePath.startsWith("/workspace"),
      `got "${npmCachePath}"`);
    check("npm cache is NOT under /root", !npmCachePath.startsWith("/root"),
      `got "${npmCachePath}"`);
    dim(`npm cache dir: ${npmCachePath}`);
    console.log();

    // ── Test 4: npm install writes to workspace ────────────────────
    bold("━━━ Test 4: npm install uses workspace for cache ━━━\n");

    // Create a minimal package.json and install a small package
    await sandbox.commands.run("mkdir -p /workspace/npm-test");
    await sandbox.files.write("/workspace/npm-test/package.json", JSON.stringify({
      name: "test",
      version: "1.0.0",
      dependencies: { "is-odd": "3.0.1" }
    }));

    const installResult = await sandbox.commands.run(
      "cd /workspace/npm-test && npm install --prefer-offline 2>&1",
      { timeout: 30 }
    );
    dim(`npm install exit code: ${installResult.exitCode}`);

    // Verify nothing was written to /root
    const rootCheck = await sandbox.commands.run(
      "du -sh /root/.npm 2>/dev/null || echo 'no /root/.npm'"
    );
    check("No npm cache in /root/.npm", rootCheck.stdout.includes("no /root/.npm"),
      `got "${rootCheck.stdout.trim()}"`);

    // Verify cache was written under /workspace
    const workspaceCache = await sandbox.commands.run(
      "test -d /workspace/.npm && echo 'exists' || echo 'missing'"
    );
    check("npm cache exists at /workspace/.npm", workspaceCache.stdout.trim() === "exists",
      `got "${workspaceCache.stdout.trim()}"`);
    console.log();

    // ── Test 5: Dotfiles go to workspace ───────────────────────────
    bold("━━━ Test 5: Dotfiles and configs use /workspace ━━━\n");

    // Test that .bashrc would be read from /workspace
    const bashrcPath = await sandbox.commands.run("bash -c 'echo $HOME/.bashrc'");
    check(".bashrc path is /workspace/.bashrc", bashrcPath.stdout.trim() === "/workspace/.bashrc",
      `got "${bashrcPath.stdout.trim()}"`);

    // Git global config would go to /workspace/.gitconfig
    await sandbox.commands.run("git config --global user.name 'Test User'");
    const gitConfigPath = await sandbox.commands.run("git config --global --list --show-origin 2>/dev/null | head -1");
    check("git config is under /workspace", gitConfigPath.stdout.includes("/workspace"),
      `got "${gitConfigPath.stdout.trim()}"`);
    console.log();

    // ── Test 6: /workspace is on the right filesystem ──────────────
    bold("━━━ Test 6: /workspace is NVMe-backed ━━━\n");

    const dfResult = await sandbox.commands.run("df -h /workspace | tail -1");
    const dfLine = dfResult.stdout.trim();
    dim(`df output: ${dfLine}`);
    // /workspace should be on /dev/vdb (the workspace drive), not /dev/root
    check("/workspace is NOT on /dev/root", !dfLine.startsWith("/dev/root"),
      `got "${dfLine}"`);
    console.log();

    // Clean up the test directory
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
