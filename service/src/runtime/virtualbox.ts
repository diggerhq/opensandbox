/**
 * VirtualBox runtime - uses VBoxManage + agent inside VM
 */

import { exec } from "child_process";
import { promisify } from "util";
import { join } from "path";
import type {
  SandboxRuntime,
  SandboxConfig,
  SandboxInstance,
  ExecutionResult,
  SnapshotInfo,
} from "./types";

const execAsync = promisify(exec);

interface VirtualBoxConfig {
  baseVM: string;
  vmUser: string;
  vmPassword: string;
  agentPath: string;
}

const DEFAULT_CONFIG: VirtualBoxConfig = {
  baseVM: process.env.BASE_VM || "dev-1",
  vmUser: process.env.VM_USER || "user",
  vmPassword: process.env.VM_PASSWORD || "root",
  agentPath: join(__dirname, "..", "..", "agent", "agent.py"),
};

// Track active VMs: sandboxId -> { vboxName, agentPort, sshPort }
const activeVMs = new Map<string, { vboxName: string; agentPort: number; sshPort: number }>();

// Port allocation
let nextAgentPort = 3333;
let nextSshPort = 2222;

function allocatePorts(): { agentPort: number; sshPort: number } {
  const ports = { agentPort: nextAgentPort++, sshPort: nextSshPort++ };
  return ports;
}

export class VirtualBoxRuntime implements SandboxRuntime {
  readonly name = "virtualbox";
  private config: VirtualBoxConfig;

  constructor(config: Partial<VirtualBoxConfig> = {}) {
    this.config = { ...DEFAULT_CONFIG, ...config };
  }

  async isAvailable(): Promise<boolean> {
    try {
      await execAsync("VBoxManage --version");
      await execAsync(`VBoxManage showvminfo "${this.config.baseVM}"`);
      return true;
    } catch {
      return false;
    }
  }

  async createSandbox(config: SandboxConfig): Promise<SandboxInstance> {
    const id = `vbox_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
    const vboxName = `sandbox_${id}`;
    const { agentPort, sshPort } = allocatePorts();

    console.log(`[VirtualBox] Creating sandbox ${id}...`);

    try {
      // Clone from base VM
      await execAsync(
        `VBoxManage clonevm "${this.config.baseVM}" --name "${vboxName}" --register --options link --snapshot "base"`
      );

      // Set up port forwarding
      await execAsync(`VBoxManage modifyvm "${vboxName}" --natpf1 delete "ssh" 2>/dev/null || true`);
      await execAsync(`VBoxManage modifyvm "${vboxName}" --natpf1 delete "agent" 2>/dev/null || true`);
      await execAsync(`VBoxManage modifyvm "${vboxName}" --natpf1 "ssh,tcp,,${sshPort},,22"`);
      await execAsync(`VBoxManage modifyvm "${vboxName}" --natpf1 "agent,tcp,,${agentPort},,3000"`);

      // Apply resource limits if specified
      if (config.cpuCores) {
        await execAsync(`VBoxManage modifyvm "${vboxName}" --cpus ${config.cpuCores}`);
      }
      if (config.memoryMb) {
        await execAsync(`VBoxManage modifyvm "${vboxName}" --memory ${config.memoryMb}`);
      }

      // Start VM
      await execAsync(`VBoxManage startvm "${vboxName}" --type headless`);

      // Wait for agent
      await this.waitForAgent(agentPort, 60000);

      activeVMs.set(id, { vboxName, agentPort, sshPort });

      console.log(`[VirtualBox] Sandbox ${id} ready on port ${agentPort}`);

      return {
        id,
        name: config.name,
        status: "running",
        agentUrl: `http://localhost:${agentPort}`,
      };
    } catch (error: any) {
      console.error(`[VirtualBox] Failed to create sandbox:`, error.message);
      // Cleanup on failure
      try {
        await execAsync(`VBoxManage controlvm "${vboxName}" poweroff 2>/dev/null`);
        await execAsync(`VBoxManage unregistervm "${vboxName}" --delete 2>/dev/null`);
      } catch {}
      throw error;
    }
  }

  async destroySandbox(id: string): Promise<void> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    console.log(`[VirtualBox] Destroying sandbox ${id}...`);

    try {
      await execAsync(`VBoxManage controlvm "${vm.vboxName}" poweroff 2>/dev/null`);
      await new Promise((r) => setTimeout(r, 2000));
    } catch {}

    try {
      await execAsync(`VBoxManage unregistervm "${vm.vboxName}" --delete`);
    } catch (error: any) {
      console.error(`[VirtualBox] Error deleting VM:`, error.message);
    }

    activeVMs.delete(id);
    console.log(`[VirtualBox] Sandbox ${id} destroyed`);
  }

  async execute(id: string, command: string, timeout = 30000): Promise<ExecutionResult> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const res = await fetch(`http://localhost:${vm.agentPort}/exec`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ command, timeout: Math.floor(timeout / 1000) }),
    });

    if (!res.ok) {
      throw new Error(`Agent error: ${res.status}`);
    }

    return res.json();
  }

  async snapshot(id: string, name: string): Promise<SnapshotInfo> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

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

    const res = await fetch(`http://localhost:${vm.agentPort}/snapshots`);
    if (!res.ok) {
      throw new Error(`Agent error: ${res.status}`);
    }

    const data = await res.json();
    return data.snapshots.map((s: any) => ({
      name: s.name,
      createdAt: new Date(s.created_at),
    }));
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

  async exportToFile(id: string, snapshotName = "workspace"): Promise<string> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    // Tell agent to create export file
    const exportRes = await fetch(`http://localhost:${vm.agentPort}/export`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: snapshotName }),
    });

    if (!exportRes.ok) {
      const error = await exportRes.json();
      throw new Error(error.error || "Export failed");
    }

    const exportInfo = await exportRes.json();
    
    // Return the download URL (caller can stream from this)
    return `http://localhost:${vm.agentPort}/download/${exportInfo.name}`;
  }

  async importFromFile(id: string, snapshotName: string, filePath: string): Promise<void> {
    const vm = activeVMs.get(id);
    if (!vm) {
      throw new Error(`Sandbox ${id} not found`);
    }

    // Read file and upload to agent
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
    if (!vm) {
      return false;
    }

    try {
      const res = await fetch(`http://localhost:${vm.agentPort}/health`);
      return res.ok;
    } catch {
      return false;
    }
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
}

export const virtualboxRuntime = new VirtualBoxRuntime();

