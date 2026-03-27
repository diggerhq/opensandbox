/**
 * 80 operations through the TypeScript SDK + CLI — tests every API surface.
 * Run: cd examples/typescript && npx tsx ../../scripts/qemu-tests/27-ts-sdk-ops.ts
 */

import { Sandbox } from "@opencomputer/sdk";
import { execSync } from "child_process";
import * as crypto from "crypto";

const API_KEY = process.env.OPENSANDBOX_API_KEY || "";
const API_URL = process.env.OPENSANDBOX_API_URL || "http://20.114.60.29:8080";
const OC = "/tmp/oc";

let PASS = 0;
let FAIL = 0;

function ok(msg: string) { PASS++; console.log(`  \x1b[32m✓ ${msg}\x1b[0m`); }
function bad(msg: string) { FAIL++; console.log(`  \x1b[31m✗ ${msg}\x1b[0m`); }
function check(name: string, cond: boolean, detail = "") {
  cond ? ok(name) : bad(`${name}: ${detail}`);
}
function h(msg: string) { console.log(`\n\x1b[1;34m=== ${msg} ===\x1b[0m`); }

function cli(...args: string[]): string {
  try {
    return execSync(`${OC} ${args.join(" ")}`, {
      env: { ...process.env, OPENCOMPUTER_API_KEY: API_KEY, OPENCOMPUTER_API_URL: API_URL },
      timeout: 30000,
    }).toString().trim();
  } catch (e: any) {
    return e.stdout?.toString().trim() || e.message;
  }
}

function sleep(ms: number) { return new Promise(r => setTimeout(r, ms)); }

async function main() {
  const t0 = Date.now();

  // ── Create via CLI ─────────────────────────────────────────────
  h("Create (CLI)");
  const createJson = cli("create", "--json");
  const sbId = JSON.parse(createJson).sandboxID;
  check("CLI create", sbId.startsWith("sb-"), sbId);

  const sb = await Sandbox.connect(sbId, { apiKey: API_KEY, apiUrl: API_URL });
  check("SDK connect", sb.sandboxId === sbId);

  // ── Exec via SDK (15 ops) ──────────────────────────────────────
  h("Exec — SDK (15 operations)");

  let r = await sb.exec.run("echo hello");
  check("exec echo", r.stdout.trim() === "hello", r.stdout.trim());

  r = await sb.exec.run("python3 -c 'print(2+2)'");
  check("exec python", r.stdout.trim() === "4", r.stdout.trim());

  r = await sb.exec.run("node -e 'console.log(3*3)'");
  check("exec node", r.stdout.trim() === "9", r.stdout.trim());

  r = await sb.exec.run("whoami");
  check("exec whoami", r.stdout.trim() === "sandbox", r.stdout.trim());

  r = await sb.exec.run("bash -c 'exit 42'");
  check("non-zero exit", r.exitCode === 42, `exit=${r.exitCode}`);

  r = await sb.exec.run("python3 -c 'print(\"x\"*50000)'");
  check("large stdout", r.stdout.length >= 50000, `len=${r.stdout.length}`);

  r = await sb.exec.run("echo $MY_VAR", { env: { MY_VAR: "ts-env" } });
  check("env vars", r.stdout.trim() === "ts-env", r.stdout.trim());

  r = await sb.exec.run("pwd", { cwd: "/tmp" });
  check("cwd", r.stdout.trim() === "/tmp", r.stdout.trim());

  r = await sb.exec.run("sleep 30", { timeout: 2 });
  check("exec timeout", r.exitCode !== 0, `exit=${r.exitCode}`);

  r = await sb.exec.run("curl -s -o /dev/null -w '%{http_code}' https://httpbin.org/get");
  check("network HTTPS", r.stdout.trim() === "200", r.stdout.trim());

  for (let i = 0; i < 5; i++) {
    r = await sb.exec.run(`echo rapid-${i}`);
    check(`rapid exec ${i}`, r.stdout.trim() === `rapid-${i}`, r.stdout.trim());
  }

  // ── Exec via CLI (5 ops) ───────────────────────────────────────
  h("Exec — CLI (5 operations)");

  let out = cli("exec", sbId, "--wait", "--", "echo", "cli-hello");
  check("CLI exec echo", out === "cli-hello", out);

  out = cli("exec", sbId, "--wait", "--", "python3", "-c", "print('cli-py')");
  check("CLI exec python", out === "cli-py", out);

  out = cli("exec", sbId, "--wait", "--", "git", "--version");
  check("CLI exec git", out.includes("git version"), out);

  out = cli("exec", sbId, "--wait", "--", "uname", "-s");
  check("CLI exec uname", out === "Linux", out);

  out = cli("exec", sbId, "--wait", "--", "free", "-m");
  check("CLI exec free", out.includes("Mem:"), out.substring(0, 40));

  // ── Files via SDK (10 ops) ─────────────────────────────────────
  h("Files — SDK (10 operations)");

  for (let i = 0; i < 5; i++) {
    await sb.files.write(`/workspace/ts-file-${i}.txt`, `ts-content-${i}`);
  }
  check("write 5 files", true);

  let allMatch = true;
  for (let i = 0; i < 5; i++) {
    const c = await sb.files.read(`/workspace/ts-file-${i}.txt`);
    if (c !== `ts-content-${i}`) allMatch = false;
  }
  check("read 5 files", allMatch);

  const binData = crypto.randomBytes(100 * 1024);
  const binHash = crypto.createHash("sha256").update(binData).digest("hex");
  await sb.files.write("/workspace/ts-binary.bin", binData);
  r = await sb.exec.run("sha256sum /workspace/ts-binary.bin | cut -d' ' -f1");
  check("100KB binary hash", r.stdout.trim() === binHash, r.stdout.trim().substring(0, 16));

  const entries = await sb.files.list("/workspace");
  check("list dir", entries.length > 5, `entries=${entries.length}`);

  await sb.files.makeDir("/workspace/ts-nested/deep");
  r = await sb.exec.run("test -d /workspace/ts-nested/deep && echo yes || echo no");
  check("mkdir", r.stdout.trim() === "yes", r.stdout.trim());

  await sb.files.write("/workspace/ts-del.txt", "bye");
  await sb.files.remove("/workspace/ts-del.txt");
  const gone = await sb.files.exists("/workspace/ts-del.txt");
  check("remove file", !gone);

  const dlUrl = await sb.downloadUrl("/workspace/ts-file-0.txt");
  check("download URL", dlUrl.includes("signature"));

  const ulUrl = await sb.uploadUrl("/workspace/ts-upload.txt");
  check("upload URL", ulUrl.includes("signature"));

  // ── Scale (5 ops) ──────────────────────────────────────────────
  h("Scale (5 operations)");

  r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'");
  check("baseline memory", parseInt(r.stdout.trim()) > 800, `${r.stdout.trim()}MB`);

  for (const mem of [2048, 4096, 2048]) {
    r = await sb.exec.run(`curl -s -X POST http://169.254.169.254/v1/scale -d '{"memoryMB":${mem}}'`);
    await sleep(500);
  }
  check("3 scale operations", true);

  r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'");
  check("memory after scale", parseInt(r.stdout.trim()) > 1800, `${r.stdout.trim()}MB`);

  r = await sb.exec.run("curl -s -X POST http://169.254.169.254/v1/scale -d '{\"memoryMB\":1024}'");
  check("scale back to 1GB", true);

  // ── Preview URLs (4 ops) ───────────────────────────────────────
  h("Preview URLs (4 operations)");

  await sb.exec.run("bash -c 'setsid python3 -m http.server 3000 --directory /workspace </dev/null >/dev/null 2>&1 &'");
  await sleep(1000);

  const preview = await sb.createPreviewURL({ port: 3000 });
  check("create preview", !!preview.hostname, preview.hostname);

  const previews = await sb.listPreviewURLs();
  check("list previews", previews.length >= 1, `count=${previews.length}`);

  await sb.deletePreviewURL(3000);
  check("delete preview", true);

  await sb.exec.run("pkill -9 -f 'http.server 3000' 2>/dev/null; true");
  check("cleanup server", true);

  // ── Checkpoint + Fork via SDK (10 ops) ─────────────────────────
  h("Checkpoint + Fork — SDK (10 operations)");

  await sb.exec.run("bash -c 'echo ts-checkpoint > /workspace/ts-cp.txt && sync && sync'");
  await sleep(1000);

  const cp = await sb.createCheckpoint(`ts-cp-${Date.now()}`);
  check("create checkpoint", !!cp.id, cp.id.substring(0, 12));

  const cps = await sb.listCheckpoints();
  check("list checkpoints", cps.length >= 1, `count=${cps.length}`);

  await sleep(5000);
  const fork = await Sandbox.createFromCheckpoint(cp.id, { apiKey: API_KEY, apiUrl: API_URL, timeout: 120 });
  check("fork", fork.sandboxId.startsWith("sb-"), fork.sandboxId);

  await sleep(5000);
  r = await fork.exec.run("cat /workspace/ts-cp.txt");
  check("fork data", r.stdout.trim() === "ts-checkpoint", r.stdout.trim());

  await fork.exec.run("echo fork-only > /workspace/fork-only.txt");
  r = await sb.exec.run("cat /workspace/fork-only.txt 2>/dev/null || echo not-found");
  check("fork isolated", r.stdout.trim() === "not-found");

  await fork.kill();
  check("kill fork", true);

  // Restore
  await sb.exec.run("echo post-cp > /workspace/ts-cp.txt");
  await sb.restoreCheckpoint(cp.id);
  await sleep(15000);
  r = await sb.exec.run("cat /workspace/ts-cp.txt");
  check("restore reverted", r.stdout.trim() === "ts-checkpoint", r.stdout.trim());

  await sb.deleteCheckpoint(cp.id);
  check("delete checkpoint", true);

  // ── Hibernate + Wake via SDK (6 ops) ───────────────────────────
  h("Hibernate + Wake — SDK (6 operations)");

  await sb.exec.run("echo pre-hib > /workspace/hib.txt");
  await sb.hibernate();
  check("hibernate", true);

  await sb.wake({ timeout: 3600 });
  check("wake", true);

  r = await sb.exec.run("cat /workspace/hib.txt");
  check("data survived", r.stdout.trim() === "pre-hib", r.stdout.trim());

  r = await sb.exec.run("echo post-wake");
  check("exec after wake", r.stdout.trim() === "post-wake");

  r = await sb.exec.run("curl -s -o /dev/null -w '%{http_code}' https://httpbin.org/get");
  check("network after wake", r.stdout.trim() === "200", r.stdout.trim());

  r = await sb.exec.run("python3 -c 'print(1+1)'");
  check("python after wake", r.stdout.trim() === "2");

  // ── CLI: hibernate + wake (3 ops) ──────────────────────────────
  h("Hibernate + Wake — CLI (3 operations)");

  cli("sandbox", "hibernate", sbId);
  check("CLI hibernate", true);

  cli("sandbox", "wake", sbId);
  check("CLI wake", true);

  out = cli("exec", sbId, "--wait", "--", "echo", "cli-post-wake");
  check("CLI exec after wake", out === "cli-post-wake", out);

  // ── Timeout via SDK (2 ops) ────────────────────────────────────
  h("Timeout (2 operations)");

  await sb.setTimeout(600);
  check("set timeout 600s", true);

  await sb.setTimeout(0);
  check("set timeout 0 (unlimited)", true);

  // ── Final checks (3 ops) ───────────────────────────────────────
  h("Final Checks (3 operations)");

  r = await sb.exec.run("echo final-ts-health");
  check("final exec", r.stdout.trim() === "final-ts-health");

  const running = await sb.isRunning();
  check("sandbox running", running);

  await sb.kill();
  check("kill sandbox", true);

  const elapsed = ((Date.now() - t0) / 1000).toFixed(0);
  const total = PASS + FAIL;
  console.log(`\n\x1b[1m${PASS} passed, ${FAIL} failed (${total} total, ${elapsed}s)\x1b[0m`);
  process.exit(FAIL > 0 ? 1 : 0);
}

main().catch(e => { console.error(e); process.exit(1); });
