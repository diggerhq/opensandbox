/**
 * Disk Size Test
 *
 * Verifies the `diskMB` sandbox creation parameter:
 *   1. Default (no arg) → 20GB workspace
 *   2. Explicit 20GB   → 20GB workspace
 *   3. Explicit 30GB   → 30GB workspace (requires org `max_disk_mb` ≥ 30720)
 *   4. Below minimum    → rejected with 400
 *
 * Larger disk sizes are in closed beta — contact us to raise your org's
 * max_disk_mb ceiling: https://cal.com/team/digger/opencomputer-founder-chat
 *
 * Usage:
 *   npx tsx examples/test-disk-size.ts
 *
 * Environment:
 *   OPENCOMPUTER_API_URL  (default: http://localhost:8080)
 *   OPENCOMPUTER_API_KEY  (default: test-key)
 */

import { Sandbox } from "../src/index";

const API_URL = process.env.OPENCOMPUTER_API_URL ?? "http://localhost:8080";
const API_KEY = process.env.OPENCOMPUTER_API_KEY ?? "test-key";

function green(msg: string) { console.log(`\x1b[32m✓ ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m✗ ${msg}\x1b[0m`); }
function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }

let passed = 0;
let failed = 0;

async function runCase(
  label: string,
  diskMB: number | undefined,
  expect: { size?: string; reject?: boolean },
) {
  bold(`\n${label}`);
  try {
    const sb = await Sandbox.create({
      apiUrl: API_URL,
      apiKey: API_KEY,
      timeout: 120,
      ...(diskMB !== undefined ? { diskMB } : {}),
    });
    dim(`created ${sb.sandboxId}`);
    try {
      const r = await sb.exec.run("df -h /home/sandbox | tail -1");
      dim(`df: ${r.stdout.trim()}`);
      if (expect.reject) {
        red(`expected rejection but sandbox was created`);
        failed++;
      } else if (expect.size && r.stdout.includes(expect.size)) {
        green(`workspace reports ${expect.size}`);
        passed++;
      } else {
        red(`expected ${expect.size} in df output`);
        failed++;
      }
    } finally {
      await sb.kill();
    }
  } catch (e: any) {
    if (expect.reject) {
      green(`rejected as expected (${e.message.split("\n")[0]})`);
      passed++;
    } else {
      red(`unexpected error: ${e.message}`);
      failed++;
    }
  }
}

(async () => {
  bold("OpenComputer SDK — disk size test");
  dim(`API: ${API_URL}`);

  await runCase("default (no diskMB)", undefined, { size: "20G" });
  await runCase("explicit 20GB", 20480, { size: "20G" });
  await runCase("30GB", 30720, { size: "30G" });
  await runCase("below minimum (8GB)", 8192, { reject: true });

  console.log();
  bold(`Results: ${passed} passed, ${failed} failed`);
  process.exit(failed > 0 ? 1 : 0);
})();
