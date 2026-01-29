/**
 * Firecracker microVM runtime with Btrfs snapshots
 * Requires Linux with KVM support (/dev/kvm)
 */

import { spawn, execSync } from "child_process";
import { mkdir, access, rm, copyFile, writeFile } from "fs/promises";
import { randomUUID } from "crypto";
import { join } from "path";
import type {
  SandboxRuntime,
  SandboxConfig,
  SandboxInstance,
  ExecutionResult,
  SnapshotInfo,
} from "./types";

interface FirecrackerConfig {
  firecrackerBin: string;
  kernelPath: string;
  rootfsPath: string;
  btrfsPath: string; // Btrfs-mounted directory for workspaces
}

const DEFAULT_CONFIG: FirecrackerConfig = {
  firecrackerBin: process.env.FC_BIN || "/usr/local/bin/firecracker",
  kernelPath: process.env.FC_KERNEL || "/var/lib/firecracker/vmlinux",
  rootfsPath: process.env.FC_ROOTFS || "/var/lib/firecracker/rootfs.ext4",
  btrfsPath: process.env.FC_BTRFS_PATH || "/var/lib/firecracker-btrfs",
};

interface ActiveVM {
  vmDir: string;
  workspaceSubvol: string;
  snapshotsDir: string;
  socketPath: string;
  agentPort: number;
  process?: ReturnType<typeof spawn>;
}

const activeVMs = new Map<string, ActiveVM>();
let nextAgentPort = 4000;

export class FirecrackerRuntime implements SandboxRuntime {
  readonly name = "firecracker";
  private config: FirecrackerConfig;
  private btrfsReady = false;

  constructor(config: Partial<FirecrackerConfig> = {}) {
    this.config = { ...DEFAULT_CONFIG, ...config };
  }

  async isAvailable(): Promise<boolean> {
    if (process.platform !== "linux") {
      console.warn("[Firecracker] Only runs on Linux");
      return false;
    }

    try {
      await access("/dev/kvm");
    } catch {
      console.warn("[Firecracker] /dev/kvm not accessible");
      return false;
    }

    try {
      execSync(`${this.config.firecrackerBin} --version`, { stdio: "pipe" });
      await access(this.config.kernelPath);
      await access(this.config.rootfsPath);
    } catch (e) {
      console.warn("[Firecracker] Not fully configured:", e);
      return false;
    }

    // Setup Btrfs
    await this.ensureBtrfs();
    return true;
  }

  private async ensureBtrfs(): Promise<void> {
    try {
      await access(this.config.btrfsPath);
      // Check if it's actually Btrfs
      const fsType = execSync(`stat -f -c %T ${this.config.btrfsPath} 2>/dev/null || echo "unknown"`, {
        encoding: "utf-8",
      }).trim();

      if (fsType === "btrfs") {
        this.btrfsReady = true;
        console.log(`[Firecracker] Using existing Btrfs at ${this.config.btrfsPath}`);
        return;
      }
    } catch {}

    // Create Btrfs loopback
    console.log(`[Firecracker] Setting up Btrfs at ${this.config.btrfsPath}...`);

    try {
      const imgPath = `${this.config.btrfsPath}.img`;

      execSync(`sudo truncate -s 20G ${imgPath}`);
      execSync(`sudo mkfs.btrfs ${imgPath}`);
      execSync(`sudo mkdir -p ${this.config.btrfsPath}`);
      execSync(`sudo mount -o loop ${imgPath} ${this.config.btrfsPath}`);
      execSync(`sudo chmod 777 ${this.config.btrfsPath}`);

      this.btrfsReady = true;
      console.log(`[Firecracker] Btrfs ready at ${this.config.btrfsPath}`);
    } catch (e: any) {
      console.warn(`[Firecracker] Failed to setup Btrfs: ${e.message}`);
      this.btrfsReady = false;
    }
  }

  async createSandbox(config: SandboxConfig): Promise<SandboxInstance> {
    const id = `fc-${randomUUID().slice(0, 8)}`;
    const vmDir = join(this.config.btrfsPath, id);
    const socketPath = join(vmDir, "firecracker.sock");
    const workspaceSubvol = join(vmDir, "workspace");
    const snapshotsDir = join(vmDir, "snapshots");
    const agentPort = nextAgentPort++;

    console.log(`[Firecracker] Creating sandbox ${id}...`);

    await mkdir(vmDir, { recursive: true });
    await mkdir(snapshotsDir, { recursive: true });

    // Create Btrfs subvolume for workspace
    if (this.btrfsReady) {
      execSync(`btrfs subvolume create ${workspaceSubvol}`);
    } else {
      await mkdir(workspaceSubvol, { recursive: true });
    }

    // Copy rootfs to VM directory
    const rootfsCopy = join(vmDir, "rootfs.ext4");
    await copyFile(this.config.rootfsPath, rootfsCopy);

    // Create VM config with networking for agent
    const vmConfig = {
      "boot-source": {
        kernel_image_path: this.config.kernelPath,
        boot_args: `console=ttyS0 reboot=k panic=1 pci=off ip=172.16.0.2::172.16.0.1:255.255.255.0::eth0:off`,
      },
      drives: [
        {
          drive_id: "rootfs",
          path_on_host: rootfsCopy,
          is_root_device: true,
          is_read_only: false,
        },
      ],
      "machine-config": {
        vcpu_count: config.cpuCores || 1,
        mem_size_mib: config.memoryMb || 512,
      },
      "network-interfaces": [
        {
          iface_id: "eth0",
          guest_mac: "AA:FC:00:00:00:01",
          host_dev_name: `fc-${id}-tap`,
        },
      ],
    };

    const configPath = join(vmDir, "vm-config.json");
    await writeFile(configPath, JSON.stringify(vmConfig));

    // Setup TAP device for networking
    try {
      execSync(`sudo ip tuntap add fc-${id}-tap mode tap`);
      execSync(`sudo ip addr add 172.16.0.1/24 dev fc-${id}-tap`);
      execSync(`sudo ip link set fc-${id}-tap up`);
      // Forward agent port
      execSync(`sudo iptables -t nat -A PREROUTING -p tcp --dport ${agentPort} -j DNAT --to-destination 172.16.0.2:3000`);
      execSync(`sudo iptables -A FORWARD -p tcp -d 172.16.0.2 --dport 3000 -j ACCEPT`);
    } catch (e: any) {
      console.warn(`[Firecracker] Network setup warning: ${e.message}`);
    }

    // Start Firecracker
    const fc = spawn(
      this.config.firecrackerBin,
      ["--api-sock", socketPath, "--config-file", configPath],
      {
        stdio: ["ignore", "pipe", "pipe"],
        cwd: vmDir,
      }
    );

    activeVMs.set(id, {
      vmDir,
      workspaceSubvol,
      snapshotsDir,
      socketPath,
      agentPort,
      process: fc,
    });

    // Wait for VM and agent to be ready
    console.log(`[Firecracker] Waiting for agent on port ${agentPort}...`);
    await this.waitForAgent(agentPort, 30000);

    console.log(`[Firecracker] Sandbox ${id} ready`);

    return {
      id,
      name: config.name,
      status: "running",
      agentUrl: `http://localhost:${agentPort}`,
    };
  }

  private async waitForAgent(port: number, timeoutMs: number): Promise<void> {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      try {
        const res = await fetch(`http://localhost:${port}/health`);
        if (res.ok) return;
      } catch {}
      await new Promise((r) => setTimeout(r, 1000));
    }
    throw new Error("Timeout waiting for agent");
  }

  async destroySandbox(id: string): Promise<void> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    console.log(`[Firecracker] Destroying sandbox ${id}...`);

    // Kill the process
    if (vm.process) {
      vm.process.kill("SIGKILL");
    }

    // Cleanup networking
    try {
      execSync(`sudo iptables -t nat -D PREROUTING -p tcp --dport ${vm.agentPort} -j DNAT --to-destination 172.16.0.2:3000 2>/dev/null || true`);
      execSync(`sudo ip link delete fc-${id}-tap 2>/dev/null || true`);
    } catch {}

    // Delete Btrfs subvolumes
    if (this.btrfsReady) {
      try {
        const snapshots = await this.listSnapshots(id);
        for (const snap of snapshots) {
          execSync(`btrfs subvolume delete ${join(vm.snapshotsDir, snap.name)} 2>/dev/null || true`);
        }
        execSync(`btrfs subvolume delete ${vm.workspaceSubvol} 2>/dev/null || true`);
      } catch {}
    }

    // Cleanup directory
    try {
      await rm(vm.vmDir, { recursive: true, force: true });
    } catch {}

    activeVMs.delete(id);
    console.log(`[Firecracker] Sandbox ${id} destroyed`);
  }

  async execute(id: string, command: string, timeout = 30000): Promise<ExecutionResult> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    // Execute via agent
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeout);

    try {
      const res = await fetch(`http://localhost:${vm.agentPort}/exec`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ command, timeout: Math.floor(timeout / 1000) }),
        signal: controller.signal,
      });

      clearTimeout(timer);

      if (!res.ok) {
        throw new Error(`Agent error: ${res.status}`);
      }

      return res.json();
    } catch (e: any) {
      clearTimeout(timer);
      if (e.name === "AbortError") {
        return { stdout: "", stderr: "Execution timed out", exitCode: 124 };
      }
      return { stdout: "", stderr: e.message, exitCode: 1 };
    }
  }

  async snapshot(id: string, name: string): Promise<SnapshotInfo> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    // Use agent for Btrfs snapshot (it has access to /workspace)
    const res = await fetch(`http://localhost:${vm.agentPort}/snapshot`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });

    if (!res.ok) {
      const error = await res.json();
      throw new Error(error.error || "Snapshot failed");
    }

    return { name, createdAt: new Date() };
  }

  async restore(id: string, snapshotName: string): Promise<void> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const res = await fetch(`http://localhost:${vm.agentPort}/restore`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: snapshotName }),
    });

    if (!res.ok) {
      const error = await res.json();
      throw new Error(error.error || "Restore failed");
    }
  }

  async listSnapshots(id: string): Promise<SnapshotInfo[]> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    try {
      const res = await fetch(`http://localhost:${vm.agentPort}/snapshots`);
      if (!res.ok) return [];
      const data = await res.json();
      return data.snapshots.map((s: any) => ({
        name: s.name,
        createdAt: new Date(s.created_at),
      }));
    } catch {
      return [];
    }
  }

  async wipe(id: string): Promise<void> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const res = await fetch(`http://localhost:${vm.agentPort}/wipe`, {
      method: "POST",
    });

    if (!res.ok) {
      throw new Error(`Agent error: ${res.status}`);
    }
  }

  async exportToFile(id: string, snapshotName?: string): Promise<string> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const res = await fetch(`http://localhost:${vm.agentPort}/export`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: snapshotName || "workspace" }),
    });

    if (!res.ok) {
      const error = await res.json();
      throw new Error(error.error || "Export failed");
    }

    const exportInfo = await res.json();
    return `http://localhost:${vm.agentPort}/download/${exportInfo.name}`;
  }

  async importFromFile(id: string, snapshotName: string, filePath: string): Promise<void> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const { readFile } = await import("fs/promises");
    const data = await readFile(filePath);

    const res = await fetch(`http://localhost:${vm.agentPort}/upload/${snapshotName}`, {
      method: "PUT",
      headers: {
        "Content-Type": "application/gzip",
        "Content-Length": data.length.toString(),
      },
      body: data,
    });

    if (!res.ok) {
      const error = await res.json();
      throw new Error(error.error || "Import failed");
    }
  }

  async healthCheck(id: string): Promise<boolean> {
    const vm = activeVMs.get(id);
    if (!vm) return false;

    try {
      const res = await fetch(`http://localhost:${vm.agentPort}/health`);
      return res.ok;
    } catch {
      return false;
    }
  }
}

export const firecrackerRuntime = new FirecrackerRuntime();
