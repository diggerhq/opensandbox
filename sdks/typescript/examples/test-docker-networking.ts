/**
 * Docker Networking Test
 *
 * Tests Docker bridge networking inside a sandbox VM:
 *   1. Kernel module loading (bridge, veth, netfilter)
 *   2. Docker daemon startup with bridge networking
 *   3. Container → internet connectivity (ping + DNS)
 *   4. Host → container latency
 *   5. Container ↔ container latency
 *   6. Container → internet throughput
 *   7. Docker images land on data disk (/home/sandbox/.docker)
 *
 * Usage:
 *   npx tsx examples/test-docker-networking.ts
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
    red(`${desc}${detail ? ` — ${detail}` : ""}`);
    failed++;
  }
}

/** Run a command as root with a timeout, return stdout */
async function run(sandbox: Sandbox, cmd: string, timeout = 30): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  const result = await sandbox.commands.run(`sudo bash -c '${cmd.replace(/'/g, "'\\''")}'`, { timeout });
  return result;
}

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       Docker Networking Test                     ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let sandbox: Sandbox | null = null;

  try {
    sandbox = await Sandbox.create({ timeout: 300 });
    green(`Created sandbox: ${sandbox.sandboxId}`);
    console.log();

    // ── Test 1: Kernel modules ───────────────────────────────────────
    bold("━━━ Test 1: Kernel module loading ━━━\n");

    const modules = ["bridge", "veth", "br_netfilter", "ip_tables", "nf_nat", "nf_conntrack"];
    for (const mod of modules) {
      const result = await run(sandbox, `modprobe ${mod} 2>&1 && echo OK`);
      check(`modprobe ${mod}`, result.stdout.includes("OK"), result.stdout.trim());
    }
    console.log();

    // ── Test 2: Install Docker ───────────────────────────────────────
    bold("━━━ Test 2: Install Docker ━━━\n");

    dim("Installing docker.io (this takes ~30s)...");
    const install = await run(sandbox, "apt-get update -qq 2>/dev/null && apt-get install -y -qq docker.io 2>&1 | tail -1", 180);
    check("Docker installed", install.exitCode === 0, install.stderr);

    const daemonJson = await run(sandbox, "cat /etc/docker/daemon.json");
    check("daemon.json has data-root on data disk", daemonJson.stdout.includes("/home/sandbox/.docker"));
    check("daemon.json does not disable bridge", !daemonJson.stdout.includes('"bridge": "none"'));
    dim(`daemon.json: ${daemonJson.stdout.trim()}`);
    console.log();

    // ── Test 3: Start Docker daemon ──────────────────────────────────
    bold("━━━ Test 3: Docker daemon with bridge networking ━━━\n");

    await run(sandbox, "echo 1 > /proc/sys/net/ipv4/ip_forward");
    await run(sandbox, "/usr/bin/dockerd </dev/null >/tmp/dockerd.log 2>&1 & sleep 1");

    // Wait for dockerd to be ready
    dim("Waiting for dockerd...");
    let dockerReady = false;
    for (let i = 0; i < 20; i++) {
      const ver = await run(sandbox, "/usr/bin/docker version --format '{{.Server.Version}}' 2>/dev/null");
      if (ver.exitCode === 0 && ver.stdout.trim()) {
        dockerReady = true;
        dim(`Docker version: ${ver.stdout.trim()}`);
        break;
      }
      await new Promise(r => setTimeout(r, 1000));
    }
    check("Docker daemon started with bridge networking", dockerReady);

    if (!dockerReady) {
      const log = await run(sandbox, "tail -10 /tmp/dockerd.log");
      dim(`dockerd log: ${log.stdout}`);
      throw new Error("Docker daemon failed to start");
    }

    const info = await run(sandbox, "/usr/bin/docker info --format '{{.Driver}} / {{json .Plugins.Network}}' 2>/dev/null");
    check("Bridge network driver available", info.stdout.includes("bridge"));
    dim(`Docker info: ${info.stdout.trim()}`);
    console.log();

    // ── Test 4: Container → Internet ─────────────────────────────────
    bold("━━━ Test 4: Container → Internet ━━━\n");

    const ping = await run(sandbox, "/usr/bin/docker run --rm alpine:3.19 ping -c5 -W3 8.8.8.8 2>&1", 60);
    check("Container can ping 8.8.8.8", ping.stdout.includes("0% packet loss"));
    const avgMatch = ping.stdout.match(/min\/avg\/max = [\d.]+\/([\d.]+)/);
    if (avgMatch) dim(`Avg latency to 8.8.8.8: ${avgMatch[1]}ms`);

    const dns = await run(sandbox, "/usr/bin/docker run --rm alpine:3.19 ping -c3 -W3 google.com 2>&1", 30);
    check("Container DNS resolution works", dns.stdout.includes("0% packet loss"));
    const dnsMatch = dns.stdout.match(/min\/avg\/max = [\d.]+\/([\d.]+)/);
    if (dnsMatch) dim(`Avg latency to google.com: ${dnsMatch[1]}ms`);

    const dl = await run(sandbox, "/usr/bin/docker run --rm alpine:3.19 wget -O /dev/null http://speedtest.tele2.net/10MB.zip 2>&1", 60);
    check("Container can download from internet", dl.stdout.includes("saved") || dl.exitCode === 0);
    console.log();

    // ── Test 5: Host → Container latency ─────────────────────────────
    bold("━━━ Test 5: Host ↔ Container latency ━━━\n");

    await run(sandbox, "/usr/bin/docker run -d --name ping-target alpine:3.19 sleep 60 2>/dev/null");
    const cip = await run(sandbox, "/usr/bin/docker inspect -f '{{.NetworkSettings.IPAddress}}' ping-target 2>/dev/null");
    const containerIP = cip.stdout.trim();
    check("Container got bridge IP", containerIP.startsWith("172.17."));
    dim(`Container IP: ${containerIP}`);

    const hostPing = await run(sandbox, `ping -c10 -W3 ${containerIP} 2>&1`);
    check("Host can ping container", hostPing.stdout.includes("0% packet loss"));
    const hostMatch = hostPing.stdout.match(/min\/avg\/max(?:\/mdev)? = [\d.]+\/([\d.]+)/);
    if (hostMatch) dim(`Host → container avg latency: ${hostMatch[1]}ms`);

    await run(sandbox, "/usr/bin/docker rm -f ping-target 2>/dev/null");
    console.log();

    // ── Test 6: Container ↔ Container ────────────────────────────────
    bold("━━━ Test 6: Container ↔ Container ━━━\n");

    await run(sandbox, "/usr/bin/docker network create bench-net 2>/dev/null");
    await run(sandbox, "/usr/bin/docker run -d --name ctr-a --network bench-net alpine:3.19 sleep 60 2>/dev/null");

    const c2c = await run(sandbox, "/usr/bin/docker run --rm --network bench-net alpine:3.19 ping -c10 ctr-a 2>&1", 30);
    check("Container ↔ container ping works", c2c.stdout.includes("0% packet loss"));
    const c2cMatch = c2c.stdout.match(/min\/avg\/max = [\d.]+\/([\d.]+)/);
    if (c2cMatch) dim(`Container ↔ container avg latency: ${c2cMatch[1]}ms`);

    await run(sandbox, "/usr/bin/docker rm -f ctr-a 2>/dev/null");
    await run(sandbox, "/usr/bin/docker network rm bench-net 2>/dev/null");
    console.log();

    // ── Test 7: Docker images on data disk ───────────────────────────
    bold("━━━ Test 7: Docker data on data disk ━━━\n");

    const dockerDir = await run(sandbox, "du -sh /home/sandbox/.docker 2>/dev/null");
    check("Docker data stored on data disk", dockerDir.stdout.includes("/home/sandbox/.docker"));
    dim(`Docker data size: ${dockerDir.stdout.trim()}`);

    const disk = await run(sandbox, "df -h / /home/sandbox");
    dim(`Disk usage:\n${disk.stdout}`);
    console.log();

  } catch (err) {
    red(`Error: ${err}`);
    failed++;
  } finally {
    if (sandbox) {
      await sandbox.kill();
      dim(`Sandbox ${sandbox.sandboxId} destroyed`);
    }
  }

  // ── Summary ──────────────────────────────────────────────────────
  bold("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━");
  bold(`Results: ${passed} passed, ${failed} failed`);
  if (failed > 0) {
    process.exit(1);
  }
}

main();
