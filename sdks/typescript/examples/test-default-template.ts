/**
 * Default Template Verification Test
 *
 * Verifies that the "default" template image contains all expected packages:
 *   1. Python 3 + pip + venv
 *   2. Node.js 20 + npm
 *   3. Build tools (gcc, g++, make, cmake)
 *   4. Git + git-lfs
 *   5. Common utilities (curl, wget, jq, tar, zip, unzip, etc.)
 *   6. System libraries (libssl, libffi, zlib, sqlite3)
 *   7. Locale is set to en_US.UTF-8
 *
 * Usage:
 *   npx tsx examples/test-default-template.ts
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

/** Run a command and check it exits 0 and stdout contains expected substring */
async function expectCommand(
  sandbox: Sandbox,
  desc: string,
  cmd: string,
  opts?: { contains?: string; versionMin?: string },
) {
  const result = await sandbox.commands.run(cmd);
  const out = result.stdout.trim() || result.stderr.trim();
  dim(`$ ${cmd}`);
  dim(`  → ${out}`);

  if (result.exitCode !== 0) {
    check(desc, false, `exit code ${result.exitCode}`);
    return out;
  }

  if (opts?.contains) {
    check(desc, out.toLowerCase().includes(opts.contains.toLowerCase()),
      `expected "${opts.contains}" in output`);
  } else {
    check(desc, true);
  }
  return out;
}

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║     Default Template Verification Test           ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let sandbox: Sandbox | null = null;

  try {
    sandbox = await Sandbox.create({ template: "default", timeout: 120 });
    green(`Created sandbox: ${sandbox.sandboxId}`);
    console.log();

    // ── 1. Python ───────────────────────────────────────────────────
    bold("━━━ 1. Python 3 ━━━\n");

    await expectCommand(sandbox, "python3 is installed", "python3 --version", { contains: "Python 3" });
    await expectCommand(sandbox, "python symlink works", "python --version", { contains: "Python 3" });
    await expectCommand(sandbox, "pip3 is installed", "pip3 --version", { contains: "pip" });
    await expectCommand(sandbox, "python3-venv is available", "python3 -m venv --help 2>&1 | head -1", { contains: "usage" });

    // Verify pip can install a package
    const pipInstall = await sandbox.commands.run("pip3 install --no-cache-dir cowsay 2>&1", { timeout: 30 });
    check("pip install works", pipInstall.exitCode === 0, `exit ${pipInstall.exitCode}`);
    const cowsay = await sandbox.commands.run("python3 -c 'import cowsay; print(\"pip-ok\")'");
    check("installed Python package importable", cowsay.stdout.trim().includes("pip-ok"));
    console.log();

    // ── 2. Node.js ──────────────────────────────────────────────────
    bold("━━━ 2. Node.js 20 + npm ━━━\n");

    const nodeVersion = await expectCommand(sandbox, "node is installed", "node --version");
    check("Node.js is v20.x", nodeVersion.startsWith("v20"), `got ${nodeVersion}`);
    await expectCommand(sandbox, "npm is installed", "npm --version");

    // Verify npm can install a package
    await sandbox.commands.run("mkdir -p /tmp/npm-test && cd /tmp/npm-test && npm init -y 2>&1", { timeout: 10 });
    const npmInstall = await sandbox.commands.run("cd /tmp/npm-test && npm install is-odd 2>&1", { timeout: 30 });
    check("npm install works", npmInstall.exitCode === 0, `exit ${npmInstall.exitCode}`);
    const nodeRun = await sandbox.commands.run('node -e "console.log(require(\'is-odd\')(3))"', { cwd: "/tmp/npm-test" });
    check("installed npm package works", nodeRun.stdout.trim() === "true", `got "${nodeRun.stdout.trim()}"`);
    console.log();

    // ── 3. Build tools ──────────────────────────────────────────────
    bold("━━━ 3. Build tools ━━━\n");

    await expectCommand(sandbox, "gcc is installed", "gcc --version 2>&1 | head -1", { contains: "gcc" });
    await expectCommand(sandbox, "g++ is installed", "g++ --version 2>&1 | head -1", { contains: "g++" });
    await expectCommand(sandbox, "make is installed", "make --version 2>&1 | head -1", { contains: "make" });
    await expectCommand(sandbox, "cmake is installed", "cmake --version 2>&1 | head -1", { contains: "cmake" });
    await expectCommand(sandbox, "pkg-config is installed", "pkg-config --version");

    // Verify compilation works end-to-end
    await sandbox.files.write("/tmp/hello.c", '#include <stdio.h>\nint main() { printf("compiled-ok\\n"); return 0; }');
    const compile = await sandbox.commands.run("gcc -o /tmp/hello /tmp/hello.c && /tmp/hello");
    check("C compilation + execution works", compile.stdout.trim() === "compiled-ok", `got "${compile.stdout.trim()}"`);
    console.log();

    // ── 4. Git ──────────────────────────────────────────────────────
    bold("━━━ 4. Git ━━━\n");

    await expectCommand(sandbox, "git is installed", "git --version", { contains: "git version" });
    await expectCommand(sandbox, "git-lfs is installed", "git lfs version 2>&1 | head -1", { contains: "git-lfs" });
    console.log();

    // ── 5. Networking & utilities ────────────────────────────────────
    bold("━━━ 5. Networking & common utilities ━━━\n");

    await expectCommand(sandbox, "curl is installed", "curl --version 2>&1 | head -1", { contains: "curl" });
    await expectCommand(sandbox, "wget is installed", "wget --version 2>&1 | head -1", { contains: "wget" });
    await expectCommand(sandbox, "ssh client is installed", "ssh -V 2>&1", { contains: "OpenSSH" });
    await expectCommand(sandbox, "jq is installed", "jq --version", { contains: "jq" });
    await expectCommand(sandbox, "tar is installed", "tar --version 2>&1 | head -1", { contains: "tar" });
    await expectCommand(sandbox, "zip is installed", "zip --version 2>&1 | head -2 | tail -1", { contains: "zip" });
    await expectCommand(sandbox, "unzip is installed", "unzip -v 2>&1 | head -1", { contains: "unzip" });
    await expectCommand(sandbox, "rsync is installed", "rsync --version 2>&1 | head -1", { contains: "rsync" });
    await expectCommand(sandbox, "htop is installed", "htop --version 2>&1 | head -1", { contains: "htop" });
    await expectCommand(sandbox, "tree is installed", "tree --version 2>&1", { contains: "tree" });
    console.log();

    // ── 6. System libraries ─────────────────────────────────────────
    bold("━━━ 6. System libraries ━━━\n");

    await expectCommand(sandbox, "sqlite3 is installed", "sqlite3 --version 2>&1 | head -1");
    await expectCommand(sandbox, "libssl headers present", "test -f /usr/include/openssl/ssl.h && echo ok", { contains: "ok" });
    await expectCommand(sandbox, "libffi headers present", "test -f /usr/include/ffi.h && echo ok || (test -d /usr/include/*/ffi.h 2>/dev/null && echo ok || dpkg -L libffi-dev 2>/dev/null | grep ffi.h | head -1)", { contains: "ffi" });
    await expectCommand(sandbox, "zlib headers present", "test -f /usr/include/zlib.h && echo ok", { contains: "ok" });
    console.log();

    // ── 7. Locale ───────────────────────────────────────────────────
    bold("━━━ 7. Locale ━━━\n");

    await expectCommand(sandbox, "en_US.UTF-8 locale is available", "locale -a 2>&1 | grep -i en_US.utf8", { contains: "en_US" });
    console.log();

    // ── 8. Workspace ────────────────────────────────────────────────
    bold("━━━ 8. Workspace directory ━━━\n");

    await expectCommand(sandbox, "/workspace exists", "test -d /workspace && echo ok", { contains: "ok" });
    console.log();

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
