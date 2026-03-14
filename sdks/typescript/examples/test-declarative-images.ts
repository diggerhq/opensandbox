/**
 * Declarative Image Builder & Snapshots Test
 *
 * Demonstrates and tests both patterns for defining sandbox environments:
 *
 *   Pattern 1 — On-demand images (cached by content hash):
 *     const image = Image.base('ubuntu').aptInstall(['curl']).pipInstall(['requests'])
 *     const sandbox = await Sandbox.create({ image })
 *
 *   Pattern 2 — Pre-built named snapshots:
 *     await snapshots.create({ name: 'my-env', image })
 *     const sandbox = await Sandbox.create({ snapshot: 'my-env' })
 *
 * Tests:
 *   1.  Build an image with apt + pip + env + workdir
 *   2.  Create sandbox from image (first build — cold)
 *   3.  Verify packages are installed
 *   4.  Verify env vars are set
 *   5.  Verify workdir was created
 *   6.  Create a second sandbox from the same image (cache hit — fast)
 *   7.  Verify cache hit was significantly faster
 *   8.  Create a named snapshot from an image
 *   9.  List snapshots
 *  10.  Create sandbox from named snapshot
 *  11.  Verify snapshot sandbox has correct state
 *  12.  Delete snapshot
 *  13.  Image immutability — chaining produces new instances
 *  14.  runCommands step
 *
 * Usage:
 *   npx tsx examples/test-declarative-images.ts
 */

import { Sandbox, Image, Snapshots } from "../src/index";

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
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║     Declarative Image Builder & Snapshots Test   ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  const sandboxes: Sandbox[] = [];
  const snapshots = new Snapshots();

  try {
    // ── Test: Image class immutability ─────────────────────────────
    bold("━━━ Test: Image builder basics ━━━\n");

    const base = Image.base();
    const withCurl = base.aptInstall(["curl"]);
    const withPython = withCurl.pipInstall(["requests"]);

    check("Image.base() uses default base", base.toJSON().base === "base");
    check("Image is immutable — aptInstall returns new instance", base.toJSON().steps.length === 0);
    check("Chained image has 1 step", withCurl.toJSON().steps.length === 1);
    check("Further chained image has 2 steps", withPython.toJSON().steps.length === 2);

    // Cache keys should differ
    check("Different images have different cache keys", base.cacheKey() !== withCurl.cacheKey());
    check("Same image produces same cache key", base.cacheKey() === Image.base().cacheKey());

    // File operations
    const withFile = base.addFile("/workspace/config.json", '{"key": "value"}');
    check("addFile creates a step", withFile.toJSON().steps.length === 1);
    check("addFile step type is correct", withFile.toJSON().steps[0].type === "add_file");
    console.log();

    // ── Pattern 1: On-demand image creation ───────────────────────
    bold("━━━ Pattern 1: On-demand image (first build — cold) ━━━\n");

    const image = Image.base()
      .aptInstall(["curl", "jq"])
      .runCommands("mkdir -p /workspace/project")
      .env({ MY_VAR: "hello-from-image", PROJECT_ROOT: "/workspace/project" })
      .workdir("/workspace/project");

    dim(`Image manifest: ${JSON.stringify(image.toJSON(), null, 2)}`);
    dim(`Cache key: ${image.cacheKey()}`);
    dim("Creating sandbox from image (this will build on first run)...");

    const t1Start = Date.now();
    const sandbox1 = await Sandbox.create({ image, timeout: 300 });
    const t1Elapsed = Date.now() - t1Start;
    sandboxes.push(sandbox1);

    green(`Sandbox created: ${sandbox1.sandboxId} (${t1Elapsed}ms)`);
    check("Sandbox created successfully", sandbox1.status === "running" || sandbox1.status === "creating");
    console.log();

    // ── Verify installed packages ─────────────────────────────────
    bold("━━━ Verify: Packages installed ━━━\n");

    const curlCheck = await sandbox1.commands.run("which curl");
    check("curl is installed", curlCheck.exitCode === 0, `exit=${curlCheck.exitCode}`);

    const jqCheck = await sandbox1.commands.run("which jq");
    check("jq is installed", jqCheck.exitCode === 0, `exit=${jqCheck.exitCode}`);
    console.log();

    // ── Verify env vars ───────────────────────────────────────────
    bold("━━━ Verify: Environment variables ━━━\n");

    const envCheck = await sandbox1.commands.run("bash -lc 'echo $MY_VAR'");
    check("MY_VAR is set", envCheck.stdout.trim() === "hello-from-image", `got: "${envCheck.stdout.trim()}"`);

    const rootCheck = await sandbox1.commands.run("bash -lc 'echo $PROJECT_ROOT'");
    check("PROJECT_ROOT is set", rootCheck.stdout.trim() === "/workspace/project", `got: "${rootCheck.stdout.trim()}"`);
    console.log();

    // ── Verify workdir ────────────────────────────────────────────
    bold("━━━ Verify: Working directory ━━━\n");

    const dirCheck = await sandbox1.commands.run("test -d /workspace/project && echo exists");
    check("/workspace/project exists", dirCheck.stdout.trim() === "exists");
    console.log();

    // ── Cache hit: second sandbox from same image ─────────────────
    bold("━━━ Pattern 1: Cache hit (second sandbox, same image) ━━━\n");

    dim("Creating second sandbox from same image (should be cached)...");
    const t2Start = Date.now();
    const sandbox2 = await Sandbox.create({ image, timeout: 300 });
    const t2Elapsed = Date.now() - t2Start;
    sandboxes.push(sandbox2);

    green(`Second sandbox created: ${sandbox2.sandboxId} (${t2Elapsed}ms)`);

    // Cache hit should be faster (no build step)
    if (t1Elapsed > 5000) {
      // Only check speedup if cold build took meaningful time
      check(
        `Cache hit is faster (${t2Elapsed}ms vs ${t1Elapsed}ms cold build)`,
        t2Elapsed < t1Elapsed,
        `cold=${t1Elapsed}ms, cached=${t2Elapsed}ms`
      );
    } else {
      dim(`Cold build was fast (${t1Elapsed}ms) — skipping speedup check`);
    }

    // Verify cached sandbox also has the packages
    const curlCheck2 = await sandbox2.commands.run("which curl");
    check("Cached sandbox also has curl", curlCheck2.exitCode === 0);
    console.log();

    // Kill sandboxes before creating more (avoid hitting org concurrency limits)
    for (const sb of sandboxes) {
      try { await sb.kill(); } catch { /* best effort */ }
    }
    sandboxes.length = 0;

    // ── Pattern 2: Pre-built named snapshot ───────────────────────
    bold("━━━ Pattern 2: Create named snapshot ━━━\n");

    const snapshotImage = Image.base()
      .runCommands(
        "echo 'snapshot-marker' > /workspace/snapshot-test.txt",
        "mkdir -p /workspace/data"
      )
      .env({ SNAPSHOT_ENV: "from-snapshot" });

    dim("Creating snapshot 'test-env'...");
    const snapshotInfo = await snapshots.create({
      name: "test-env",
      image: snapshotImage,
    });
    green(`Snapshot created: ${snapshotInfo.name} (status=${snapshotInfo.status})`);
    check("Snapshot status is ready", snapshotInfo.status === "ready");
    console.log();

    // ── List snapshots ────────────────────────────────────────────
    bold("━━━ Verify: List snapshots ━━━\n");

    const snapshotList = await snapshots.list();
    const found = snapshotList.find((s) => s.name === "test-env");
    check("Snapshot appears in list", found !== undefined);
    check("Snapshot has correct name", found?.name === "test-env");
    console.log();

    // ── Get snapshot by name ──────────────────────────────────────
    bold("━━━ Verify: Get snapshot by name ━━━\n");

    const fetched = await snapshots.get("test-env");
    check("Fetched snapshot matches", fetched.name === "test-env");
    check("Fetched snapshot is ready", fetched.status === "ready");
    console.log();

    // ── Create sandbox from named snapshot ────────────────────────
    bold("━━━ Pattern 2: Create sandbox from named snapshot ━━━\n");

    dim("Creating sandbox from snapshot 'test-env'...");
    const t3Start = Date.now();
    const sandbox3 = await Sandbox.create({ snapshot: "test-env", timeout: 300 });
    const t3Elapsed = Date.now() - t3Start;
    sandboxes.push(sandbox3);

    green(`Sandbox from snapshot: ${sandbox3.sandboxId} (${t3Elapsed}ms)`);
    check("Sandbox created successfully", sandbox3.status === "running" || sandbox3.status === "creating");

    // Verify snapshot state
    const markerCheck = await sandbox3.commands.run("cat /workspace/snapshot-test.txt");
    check("Snapshot marker file exists", markerCheck.stdout.trim() === "snapshot-marker", `got: "${markerCheck.stdout.trim()}"`);

    const snapshotEnvCheck = await sandbox3.commands.run("bash -lc 'echo $SNAPSHOT_ENV'");
    check("Snapshot env var is set", snapshotEnvCheck.stdout.trim() === "from-snapshot", `got: "${snapshotEnvCheck.stdout.trim()}"`);

    const dataDirCheck = await sandbox3.commands.run("test -d /workspace/data && echo exists");
    check("/workspace/data exists", dataDirCheck.stdout.trim() === "exists");
    console.log();

    // ── Delete snapshot ───────────────────────────────────────────
    bold("━━━ Cleanup: Delete snapshot ━━━\n");

    await snapshots.delete("test-env");
    green("Snapshot 'test-env' deleted");

    const listAfterDelete = await snapshots.list();
    const deletedFound = listAfterDelete.find((s) => s.name === "test-env");
    check("Snapshot no longer in list", deletedFound === undefined);
    console.log();

    // Kill snapshot sandbox before creating more
    for (const sb of sandboxes) {
      try { await sb.kill(); } catch { /* best effort */ }
    }
    sandboxes.length = 0;

    // ── Test: runCommands with multiple commands ──────────────────
    bold("━━━ Test: runCommands with multiple commands ━━━\n");

    const multiImage = Image.base()
      .runCommands(
        "echo hello > /workspace/a.txt",
        "echo world > /workspace/b.txt",
        "cat /workspace/a.txt /workspace/b.txt > /workspace/combined.txt"
      );

    dim("Creating sandbox with multi-command image...");
    const sandbox4 = await Sandbox.create({ image: multiImage, timeout: 300 });
    sandboxes.push(sandbox4);

    const combined = await sandbox4.commands.run("cat /workspace/combined.txt");
    check("Multiple runCommands executed", combined.stdout.includes("hello") && combined.stdout.includes("world"), `got: "${combined.stdout.trim()}"`);
    console.log();

    // Kill sandboxes before next test
    for (const sb of sandboxes) {
      try { await sb.kill(); } catch { /* best effort */ }
    }
    sandboxes.length = 0;

    // ── Test: addFile step ──────────────────────────────────────────
    bold("━━━ Test: addFile — bake files into the image ━━━\n");

    const fileImage = Image.base()
      .addFile("/workspace/config.json", '{"env": "production", "debug": false}')
      .addFile("/workspace/setup.sh", "#!/bin/bash\necho 'Hello from setup!'")
      .runCommands("chmod +x /workspace/setup.sh");

    dim("Creating sandbox with embedded files...");
    const sandbox5 = await Sandbox.create({ image: fileImage, timeout: 300 });
    sandboxes.push(sandbox5);

    const configCheck = await sandbox5.commands.run("cat /workspace/config.json");
    check("addFile wrote config.json", configCheck.stdout.includes('"env": "production"'), `got: "${configCheck.stdout.trim()}"`);

    const setupCheck = await sandbox5.commands.run("/workspace/setup.sh");
    check("addFile wrote executable script", setupCheck.stdout.trim() === "Hello from setup!", `got: "${setupCheck.stdout.trim()}"`);
    console.log();

    // Kill sandboxes before next test
    for (const sb of sandboxes) {
      try { await sb.kill(); } catch { /* best effort */ }
    }
    sandboxes.length = 0;

    // ── Test: Build log streaming ───────────────────────────────────
    bold("━━━ Test: Build log streaming (onBuildLog callback) ━━━\n");

    const logImage = Image.base()
      .runCommands("echo 'build step 1'", "echo 'build step 2'");

    const buildLogs: string[] = [];
    dim("Creating sandbox with build log streaming...");
    const sandbox6 = await Sandbox.create({
      image: logImage,
      timeout: 300,
      onBuildLog: (log) => {
        buildLogs.push(log);
        dim(`  build: ${log}`);
      },
    });
    sandboxes.push(sandbox6);

    check("Build log callback was called", buildLogs.length > 0, `got ${buildLogs.length} logs`);
    check("Sandbox created via SSE stream", sandbox6.status === "running" || sandbox6.status === "creating");
    console.log();

    // Kill sandboxes before next test
    for (const sb of sandboxes) {
      try { await sb.kill(); } catch { /* best effort */ }
    }
    sandboxes.length = 0;

    // ── Test: Snapshot build log streaming ──────────────────────────
    bold("━━━ Test: Snapshot build log streaming (onBuildLogs) ━━━\n");

    const snapshotLogImage = Image.base()
      .runCommands("echo 'snapshot build'");

    const snapshotLogs: string[] = [];
    dim("Creating snapshot with build log streaming...");
    const streamedSnapshot = await snapshots.create({
      name: "test-streamed",
      image: snapshotLogImage,
      onBuildLogs: (log) => {
        snapshotLogs.push(log);
        dim(`  snapshot build: ${log}`);
      },
    });

    check("Snapshot build log callback was called", snapshotLogs.length > 0, `got ${snapshotLogs.length} logs`);
    check("Streamed snapshot has name", streamedSnapshot.name === "test-streamed");
    check("Streamed snapshot is ready", streamedSnapshot.status === "ready");

    // Cleanup streamed snapshot
    await snapshots.delete("test-streamed");
    green("Cleaned up test-streamed snapshot");
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    // Cleanup: kill all sandboxes
    bold("━━━ Cleanup ━━━\n");
    for (const sb of sandboxes) {
      try {
        await sb.kill();
        dim(`Killed ${sb.sandboxId}`);
      } catch { /* best effort */ }
    }
    // Clean up any leftover test snapshots
    try { await snapshots.delete("test-env"); } catch { /* may already be deleted */ }
    try { await snapshots.delete("test-streamed"); } catch { /* may already be deleted */ }
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
