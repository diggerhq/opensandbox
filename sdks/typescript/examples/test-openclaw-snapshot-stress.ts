/**
 * Stress test OpenClaw snapshot builds — runs N full builds, each with a unique
 * image (cache-busted) so every build goes through the full install flow.
 *
 * Usage:
 *   OPENCOMPUTER_API_KEY=... npx tsx examples/test-openclaw-snapshot-stress.ts [count]
 *
 * Default: 5 builds
 */

import { Image, Snapshots } from "@opencomputer/sdk/node";

const API_URL = process.env.OPENCOMPUTER_API_URL || "https://app.opencomputer.dev";
const API_KEY = process.env.OPENCOMPUTER_API_KEY;
const COUNT = parseInt(process.argv[2] || "5", 10);

if (!API_KEY) {
  console.error("Set OPENCOMPUTER_API_KEY");
  process.exit(1);
}

const snapshots = new Snapshots({ apiKey: API_KEY, apiUrl: API_URL });

async function buildOne(i: number): Promise<{ name: string; durationMs: number; status: string }> {
  const name = `openclaw-stress-${Date.now()}-${i}`;

  // Cache-bust: unique marker file per build forces a fresh image
  const image = Image.base()
    .runCommands(
      "sudo apt-get update && sudo apt-get install -y curl ca-certificates gnupg",
    )
    .runCommands(
      "curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -",
      "sudo apt-get install -y nodejs",
    )
    .runCommands(
      "mkdir -p ~/npm-global",
      "npm config set prefix ~/npm-global",
      "npm install -g openclaw@latest",
      "echo 'export PATH=$HOME/npm-global/bin:$PATH' | sudo tee /etc/profile.d/npm-global.sh",
    )
    .runCommands("mkdir -p ~/.openclaw/workspace")
    .addFile(
      "/home/sandbox/start-openclaw.sh",
      `#!/bin/bash
set -e
export PATH=$HOME/npm-global/bin:$PATH
[ -f ~/.openclaw/env ] && source ~/.openclaw/env
cd ~
nohup openclaw gateway --port 18789 > ~/.openclaw/gateway.log 2>&1 &
for i in $(seq 1 30); do
  if openclaw gateway status > /dev/null 2>&1; then
    echo "OpenClaw gateway is ready"
    exit 0
  fi
  sleep 1
done
echo "Warning: gateway did not become ready within 30s"
exit 1
`,
    )
    .runCommands("sudo chmod +x ~/start-openclaw.sh")
    // Cache-bust: unique echo per build
    .runCommands(`echo "build-${name}" > /tmp/build-marker`);

  const t0 = Date.now();
  const info = await snapshots.create({
    name,
    image,
    onBuildLogs: (line: string) => {
      process.stdout.write(`  [${i + 1}] ${line}\n`);
    },
  });

  return { name, durationMs: Date.now() - t0, status: info.status };
}

async function main() {
  console.log(`OpenClaw snapshot stress test: ${COUNT} full builds`);
  console.log(`API: ${API_URL}\n`);

  const results: { name: string; durationMs: number; status: string; error?: string }[] = [];

  for (let i = 0; i < COUNT; i++) {
    console.log(`\n── Build ${i + 1}/${COUNT} ──`);
    try {
      const result = await buildOne(i);
      results.push(result);
      console.log(`  ✓ ${result.name} — ${(result.durationMs / 1000).toFixed(1)}s (${result.status})`);
    } catch (err: any) {
      results.push({ name: `build-${i}`, durationMs: 0, status: "failed", error: err.message });
      console.log(`  ✗ FAILED: ${err.message}`);
    }
  }

  console.log(`\n══ Results ══`);
  const passed = results.filter((r) => r.status === "ready");
  const failed = results.filter((r) => r.status !== "ready");
  for (const r of results) {
    const dur = r.durationMs ? `${(r.durationMs / 1000).toFixed(1)}s` : "—";
    const icon = r.status === "ready" ? "✓" : "✗";
    console.log(`  ${icon} ${r.name} ${dur} ${r.error || ""}`);
  }
  const durations = passed.map((r) => r.durationMs / 1000);
  if (durations.length > 0) {
    const avg = durations.reduce((a, b) => a + b, 0) / durations.length;
    console.log(`\n  ${passed.length}/${COUNT} passed, avg ${avg.toFixed(1)}s`);
  }
  if (failed.length > 0) {
    process.exit(1);
  }
}

main().catch((err) => {
  console.error("Fatal:", err.message);
  process.exit(1);
});
