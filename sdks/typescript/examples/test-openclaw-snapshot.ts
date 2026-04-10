/**
 * Build a reusable OpenClaw snapshot (golden image equivalent).
 *
 * This creates a pre-built sandbox image with Node.js 22 and OpenClaw
 * pre-installed. Provisioning new employees boots from this snapshot in seconds.
 *
 * Usage:
 *   npx tsx examples/test-openclaw-snapshot.ts
 *
 * Only needs to run once, or when you want to update the base OpenClaw version.
 */

import { Sandbox } from "@opencomputer/sdk";
import { Image, Snapshots } from "@opencomputer/sdk/node";

const OC_API_URL = process.env.OPENCOMPUTER_API_URL || "https://app.opencomputer.dev";
const OC_API_KEY = process.env.OPENCOMPUTER_API_KEY;
const OPENCLAW_SNAPSHOT = "openclaw-test-" + Date.now();

async function main() {
  const apiKey = OC_API_KEY;
  if (!apiKey) {
    console.error("Set OPENCOMPUTER_API_KEY");
    process.exit(1);
  }

  console.log("Building OpenClaw snapshot...");
  console.log("This installs Node.js 22 and OpenClaw into a reusable image.\n");

  const image = Image.base()
    .runCommands(
      "sudo apt-get update && sudo apt-get install -y curl ca-certificates gnupg",
    )
    // Install Node.js 22 via NodeSource
    .runCommands(
      "curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -",
      "sudo apt-get install -y nodejs",
    )
    // Install OpenClaw globally — npm prefix set to ~/npm-global so the
    // 200MB package lands on the large home disk, not the small rootfs.
    .runCommands(
      "mkdir -p ~/npm-global",
      "npm config set prefix ~/npm-global",
      "npm install -g openclaw@latest",
      "echo 'export PATH=$HOME/npm-global/bin:$PATH' | sudo tee /etc/profile.d/npm-global.sh",
    )
    // Create OpenClaw home directory
    .runCommands("mkdir -p ~/.openclaw/workspace")
    // Startup script
    .addFile(
      "/home/sandbox/start-openclaw.sh",
      `#!/bin/bash
set -e
export PATH=$HOME/npm-global/bin:$PATH

# Source environment if present (ANTHROPIC_API_KEY, etc.)
[ -f ~/.openclaw/env ] && source ~/.openclaw/env

# Start OpenClaw gateway in background
cd ~
nohup openclaw gateway --port 18789 > ~/.openclaw/gateway.log 2>&1 &

# Wait for gateway to be ready
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
    .runCommands("sudo chmod +x ~/start-openclaw.sh");

  const snapshots = new Snapshots({ apiKey, apiUrl: OC_API_URL });

  console.log("Creating snapshot (this may take a few minutes)...\n");

  await snapshots.create({
    name: OPENCLAW_SNAPSHOT,
    image,
    onBuildLogs: (line: string) => {
      process.stdout.write(`  ${line}\n`);
    },
  });

  console.log(`\nSnapshot "${OPENCLAW_SNAPSHOT}" created successfully.`);
  console.log(
    "New employee sandboxes will boot from this snapshot in seconds.",
  );
}

main().catch((err) => {
  console.error("Failed to build snapshot:", err.message);
  process.exit(1);
});
