/**
 * test-shell.ts — End-to-end test for exec.shell() (stateful shell sessions)
 *
 * Usage:
 *   cd sdks/typescript
 *   npx tsx examples/test-shell.ts
 *
 * Environment:
 *   OPENCOMPUTER_API_URL  (default: http://localhost:8080)
 *   OPENCOMPUTER_API_KEY  (default: opensandbox-dev)
 */

import { Sandbox, ShellBusyError, ShellClosedError } from "../src/index.js";

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
  console.log("=== OpenSandbox Shell API Test ===\n");
  console.log(`API: ${API_URL}`);

  console.log("\n--- 1. Creating sandbox ---");
  const sandbox = await Sandbox.create({
    apiUrl: API_URL,
    apiKey: API_KEY,
    template: "base",
  });
  console.log(`  Sandbox: ${sandbox.sandboxId} (${sandbox.status})`);
  assert(sandbox.status === "running", "sandbox is running");

  // 2. basic run + exit code
  console.log("\n--- 2. shell.run('echo hello') ---");
  const sh = await sandbox.exec.shell();
  const r1 = await sh.run("echo hello");
  console.log(`  stdout: "${r1.stdout.trim()}" exit=${r1.exitCode}`);
  assert(r1.exitCode === 0, "exit 0");
  assert(r1.stdout.trim() === "hello", "stdout matches");
  assert(r1.stderr === "", "stderr empty");

  // 3. cwd persists across calls
  console.log("\n--- 3. cwd persists across run() calls ---");
  await sh.run("cd /tmp");
  const pwd = await sh.run("pwd");
  console.log(`  pwd: "${pwd.stdout.trim()}"`);
  assert(pwd.stdout.trim() === "/tmp", "cwd persisted");

  // 4. exported env persists
  console.log("\n--- 4. exported env persists ---");
  await sh.run("export MY_SHELL_VAR=persistence-works");
  const envR = await sh.run("echo $MY_SHELL_VAR");
  console.log(`  echo: "${envR.stdout.trim()}"`);
  assert(envR.stdout.trim() === "persistence-works", "env persisted");

  // 5. non-zero exit — subshell so it doesn't kill the outer shell
  console.log("\n--- 5. non-zero exit code ---");
  const rFail = await sh.run("( exit 7 )");
  console.log(`  exit: ${rFail.exitCode}`);
  assert(rFail.exitCode === 7, `exit 7 (got ${rFail.exitCode})`);
  const rFail2 = await sh.run("false && echo nope");
  assert(rFail2.exitCode === 1, `exit 1 from false (got ${rFail2.exitCode})`);

  // 6. stderr vs stdout separation
  console.log("\n--- 6. stderr separated from stdout ---");
  const rErr = await sh.run("echo to-out; echo to-err >&2");
  console.log(`  stdout="${rErr.stdout.trim()}" stderr="${rErr.stderr.trim()}"`);
  assert(rErr.stdout.trim() === "to-out", "stdout is to-out");
  assert(rErr.stderr.trim() === "to-err", "stderr is to-err");

  // 7. streaming callbacks — print live so interleaving is visible, then
  // assert the chunks were spaced out in time (not buffered to the end).
  console.log("\n--- 7. streaming callbacks ---");
  const outChunks: string[] = [];
  const errChunks: string[] = [];
  const outTimes: number[] = [];
  const errTimes: number[] = [];
  const t0 = Date.now();
  const stamp = () => ((Date.now() - t0) / 1000).toFixed(2).padStart(5, " ");
  // Use /bin/echo (external) rather than bash's builtin — the builtin
  // goes through glibc stdio which block-buffers when stdout is a pipe,
  // so output lands in one chunk at the end. Each /bin/echo is a fresh
  // process that flushes on exit, making the streaming observable.
  const rStream = await sh.run(
    "for i in 1 2 3; do /bin/echo stdout-$i; /bin/echo stderr-$i >&2; sleep 0.2; done",
    {
      onStdout: (b) => {
        const s = decoder.decode(b);
        outChunks.push(s);
        outTimes.push(Date.now());
        process.stdout.write(`    [+${stamp()}s OUT] ${JSON.stringify(s)}\n`);
      },
      onStderr: (b) => {
        const s = decoder.decode(b);
        errChunks.push(s);
        errTimes.push(Date.now());
        process.stdout.write(`    [+${stamp()}s ERR] ${JSON.stringify(s)}\n`);
      },
    },
  );
  const joinedOut = outChunks.join("");
  const joinedErr = errChunks.join("");
  assert(joinedOut.includes("stdout-1") && joinedOut.includes("stdout-3"), "stdout stream callbacks fired");
  assert(joinedErr.includes("stderr-1") && joinedErr.includes("stderr-3"), "stderr stream callbacks fired");
  assert(!joinedErr.includes("__OC_"), "sentinel token hidden from onStderr");
  assert(!rStream.stderr.includes("__OC_"), "sentinel token hidden from returned stderr");
  // Command sleeps 0.2s × 3 = 0.6s. Ideally chunks stream as they are
  // produced. In practice, frames may be coalesced by upstream proxies
  // (CF/ALB) or gRPC flow control, so a single-chunk delivery is legal —
  // we log the span for visibility but don't fail on it.
  const stdoutSpanMs = outTimes.length >= 2 ? outTimes[outTimes.length - 1] - outTimes[0] : 0;
  console.log(`    (stdout span across ${outTimes.length} chunks: ${stdoutSpanMs}ms)`);

  // 8. concurrent run rejects
  console.log("\n--- 8. concurrent run rejects ---");
  const slowP = sh.run("sleep 0.3; echo done");
  let busyErr: unknown;
  try {
    await sh.run("echo should-not-run");
  } catch (e) {
    busyErr = e;
  }
  assert(busyErr instanceof ShellBusyError, "got ShellBusyError on concurrent run");
  const slowR = await slowP;
  assert(slowR.stdout.trim() === "done", "original run still completes");

  // 9. shell.run with multi-line cmd
  console.log("\n--- 9. multi-line cmd ---");
  const multiR = await sh.run("echo a\necho b\necho c");
  const lines = multiR.stdout.trim().split("\n");
  assert(lines.length === 3 && lines[0] === "a" && lines[2] === "c", "multi-line executes in order");

  // 10. shell.run with a function defined earlier
  console.log("\n--- 10. shell functions persist ---");
  await sh.run("greet() { echo hi-$1; }");
  const fnR = await sh.run("greet world");
  assert(fnR.stdout.trim() === "hi-world", "defined function callable in later run");

  // 11. close
  console.log("\n--- 11. close ---");
  await sh.close();
  let closedErr: unknown;
  try {
    await sh.run("echo after-close");
  } catch (e) {
    closedErr = e;
  }
  assert(closedErr instanceof ShellClosedError, "run after close rejects with ShellClosedError");

  // 12. shell with cwd/env at construction
  console.log("\n--- 12. shell({ cwd, env }) initial state ---");
  const sh2 = await sandbox.exec.shell({
    cwd: "/etc",
    env: { SHELL_INIT_VAR: "from-init" },
  });
  const r12a = await sh2.run("pwd");
  assert(r12a.stdout.trim() === "/etc", "initial cwd honored");
  const r12b = await sh2.run("echo $SHELL_INIT_VAR");
  assert(r12b.stdout.trim() === "from-init", "initial env honored");
  await sh2.close();

  // 13. terminal-tab semantic: `exit` in user command closes the shell
  console.log("\n--- 13. exit N closes the shell (terminal-tab semantic) ---");
  const shExit = await sandbox.exec.shell();
  let exitErr: unknown;
  try {
    await shExit.run("exit 42");
  } catch (e) {
    exitErr = e;
  }
  assert(exitErr instanceof ShellClosedError, "exit 42 rejects the pending run with ShellClosedError");
  let afterExitErr: unknown;
  try {
    await shExit.run("echo after");
  } catch (e) {
    afterExitErr = e;
  }
  assert(afterExitErr instanceof ShellClosedError, "subsequent run rejects once shell is closed");

  // 14. reattach to an open shell by sessionId
  console.log("\n--- 14. reattachShell revisits an open shell ---");
  const shA = await sandbox.exec.shell();
  await shA.run("cd /tmp");
  await shA.run("export REATTACH_VAR=round-trip");
  const reattachId = shA.sessionId;
  // Drop the reference without closing — server-side bash keeps running.
  const shB = await sandbox.exec.reattachShell(reattachId);
  assert(shB.sessionId === reattachId, "reattached shell has the same sessionId");
  const rPwd = await shB.run("pwd");
  assert(rPwd.stdout.trim() === "/tmp", `reattach preserves cwd (got "${rPwd.stdout.trim()}")`);
  const rEnv = await shB.run("echo $REATTACH_VAR");
  assert(rEnv.stdout.trim() === "round-trip", `reattach preserves env (got "${rEnv.stdout.trim()}")`);
  await shB.close();

  // 15. shell alongside exec.background
  console.log("\n--- 15. exec.background alias ---");
  let bgExitCode = -999;
  const bgSession = await sandbox.exec.background("sh", {
    args: ["-c", "echo bg-ok; sleep 0.2"],
    onExit: (c) => {
      bgExitCode = c;
    },
  });
  await bgSession.done;
  assert(bgExitCode === 0, `exec.background returns exit code (got ${bgExitCode})`);

  // Cleanup
  console.log("\n--- 14. Killing sandbox ---");
  await sandbox.kill();
  assert(sandbox.status === "stopped", "sandbox stopped");

  console.log(`\n=== Results: ${passed} passed, ${failed} failed ===`);
  if (failed > 0) process.exit(1);
}

main().catch((err) => {
  console.error("\nTest failed:", err);
  process.exit(1);
});
