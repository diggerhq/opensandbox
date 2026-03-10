/**
 * test-exec.ts — End-to-end test for the session-based exec API
 *
 * Usage:
 *   cd sdks/typescript
 *   npx tsx examples/test-exec.ts
 *
 * Environment:
 *   OPENCOMPUTER_API_URL  (default: http://localhost:8080)
 *   OPENCOMPUTER_API_KEY  (default: opensandbox-dev)
 */

import { Sandbox } from "../src/index.js";

const API_URL = process.env.OPENCOMPUTER_API_URL || "http://localhost:8080";
const API_KEY = process.env.OPENCOMPUTER_API_KEY || "opensandbox-dev";

const decoder = new TextDecoder();
let passed = 0;
let failed = 0;

function assert(condition: boolean, msg: string) {
  if (condition) {
    passed++;
    console.log(`  ✓ ${msg}`);
  } else {
    failed++;
    console.log(`  ✗ ${msg}`);
  }
}

async function main() {
  console.log("=== OpenSandbox Exec API Test ===\n");
  console.log(`API: ${API_URL}`);

  // 1. Create sandbox
  console.log("\n--- 1. Creating sandbox ---");
  const sandbox = await Sandbox.create({
    apiUrl: API_URL,
    apiKey: API_KEY,
    template: "base",
  });
  console.log(`  Sandbox: ${sandbox.sandboxId} (${sandbox.status})`);
  assert(sandbox.status === "running", "sandbox is running");

  // 2. exec.run() — quick command
  console.log("\n--- 2. exec.run('echo hello world') ---");
  const result = await sandbox.exec.run("echo hello world");
  console.log(`  stdout: "${result.stdout.trim()}"`);
  assert(result.exitCode === 0, `exit code is 0 (got ${result.exitCode})`);
  assert(result.stdout.trim() === "hello world", "stdout matches");

  // 3. exec.run() — ls
  console.log("\n--- 3. exec.run('ls /') ---");
  const lsResult = await sandbox.exec.run("ls /");
  assert(lsResult.exitCode === 0, "exit code is 0");
  assert(lsResult.stdout.includes("usr"), "stdout contains 'usr'");
  assert(lsResult.stdout.includes("bin"), "stdout contains 'bin'");

  // 4. exec.run() with env vars
  console.log("\n--- 4. exec.run with env vars ---");
  const envResult = await sandbox.exec.run("echo $MY_VAR-$FOO", {
    env: { MY_VAR: "hello", FOO: "bar" },
  });
  console.log(`  output: "${envResult.stdout.trim()}"`);
  assert(envResult.stdout.trim() === "hello-bar", "env vars passed correctly");

  // 5. exec.run() with cwd
  console.log("\n--- 5. exec.run('pwd') with cwd=/tmp ---");
  const cwdResult = await sandbox.exec.run("pwd", { cwd: "/tmp" });
  console.log(`  pwd: "${cwdResult.stdout.trim()}"`);
  assert(cwdResult.stdout.trim() === "/tmp", "cwd is /tmp");

  // 6. exec.run() — non-zero exit code
  console.log("\n--- 6. exec.run('exit 42') ---");
  const failResult = await sandbox.exec.run("exit 42");
  console.log(`  exit: ${failResult.exitCode}`);
  assert(
    failResult.exitCode === 42,
    `exit code is 42 (got ${failResult.exitCode})`,
  );

  // 7. exec.run() — stderr
  console.log("\n--- 7. exec.run stderr ---");
  const stderrResult = await sandbox.exec.run("echo error-msg >&2");
  console.log(`  stderr: "${stderrResult.stderr.trim()}"`);
  assert(stderrResult.stderr.trim() === "error-msg", "stderr captured");

  // 8. exec.start() — streaming output
  console.log("\n--- 8. exec.start() — streaming ---");
  const lines: string[] = [];
  let streamExitCode = -1;
  const session = await sandbox.exec.start("sh", {
    args: ["-c", "for i in 1 2 3; do echo line-$i; sleep 0.1; done"],
    onStdout: (data) => {
      const text = decoder.decode(data);
      text
        .split("\n")
        .filter(Boolean)
        .forEach((l) => lines.push(l));
    },
    onExit: (code) => {
      streamExitCode = code;
    },
  });
  console.log(`  session: ${session.sessionId}`);
  await session.done;
  assert(lines.includes("line-1"), "got line-1");
  assert(lines.includes("line-3"), "got line-3");
  assert(streamExitCode === 0, `stream exit code is 0 (got ${streamExitCode})`);

  // 9. exec.list()
  console.log("\n--- 9. exec.list() ---");
  const sessions = await sandbox.exec.list();
  console.log(`  ${sessions.length} session(s)`);
  assert(sessions.length > 0, "has sessions");

  // 10. exec.start() + kill
  console.log("\n--- 10. exec.start('sleep 60') + kill ---");
  let killExitCode = -999;
  const sleepSession = await sandbox.exec.start("sleep", {
    args: ["60"],
    onExit: (code) => {
      killExitCode = code;
    },
  });
  await new Promise((resolve) => setTimeout(resolve, 500));
  await sleepSession.kill();
  await sleepSession.done;
  console.log(`  exit after kill: ${killExitCode}`);
  assert(killExitCode !== -999, "got exit callback after kill");

  // 11. File write + read via exec
  console.log("\n--- 11. Write file + cat ---");
  await sandbox.files.write("/tmp/test.txt", "Hello from SDK!\n");
  const catResult = await sandbox.exec.run("cat /tmp/test.txt");
  console.log(`  cat: "${catResult.stdout.trim()}"`);
  assert(catResult.stdout.trim() === "Hello from SDK!", "file content matches");

  // 12. Multi-command shell script
  console.log("\n--- 12. Multi-command script ---");
  const scriptResult = await sandbox.exec.run(
    "echo hostname=$(hostname); echo user=$(whoami); echo arch=$(uname -m)",
  );
  console.log(`  ${scriptResult.stdout.trim()}`);
  assert(scriptResult.stdout.includes("hostname="), "has hostname");
  assert(scriptResult.stdout.includes("user="), "has user");

  // 13. Network connectivity
  console.log("\n--- 13. Network connectivity ---");
  const extPing = await sandbox.exec.run("ping -c 1 -W 3 8.8.8.8 2>&1", { timeout: 10 });
  const extReachable = extPing.stdout.includes("bytes from");
  console.log(`  ping 8.8.8.8: ${extReachable ? "reachable" : "unreachable"}`);
  assert(extReachable, "external ping works");

  const dnsResult = await sandbox.exec.run("getent hosts google.com 2>&1 | head -1", { timeout: 10 });
  const dnsOk = dnsResult.exitCode === 0 && dnsResult.stdout.trim().length > 0;
  console.log(`  DNS: ${dnsOk ? dnsResult.stdout.trim() : "failed"}`);
  assert(dnsOk, "DNS resolution works");

  // 14. apt-get update (streaming via start + await done)
  console.log("\n--- 14. apt-get update (streaming) ---");
  const aptLines: string[] = [];
  const aptSession = await sandbox.exec.start("sh", {
    args: ["-c", "apt-get update 2>&1 | head -10"],
    timeout: 30,
    onStdout: (data) => {
      const text = decoder.decode(data);
      for (const line of text.split("\n").filter(Boolean)) {
        aptLines.push(line);
        process.stdout.write(`  > ${line}\n`);
      }
    },
  });
  const aptExitCode = await aptSession.done;
  console.log(`  exit: ${aptExitCode} (${aptLines.length} lines)`);
  assert(aptExitCode === 0, "apt-get update succeeds");
  assert(aptLines.some((l) => l.includes("Hit:") || l.includes("Get:")), "apt-get fetched package lists");

  // 15. Cleanup
  console.log("\n--- 15. Killing sandbox ---");
  await sandbox.kill();
  assert(sandbox.status === "stopped", "sandbox stopped");

  // Summary
  console.log(`\n=== Results: ${passed} passed, ${failed} failed ===`);
  if (failed > 0) process.exit(1);
}

main().catch((err) => {
  console.error("\nTest failed:", err);
  process.exit(1);
});
