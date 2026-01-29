/**
 * Docker-based sandbox runtime with Btrfs snapshots
 * Uses a Btrfs-backed volume for instant copy-on-write snapshots
 */

import { spawn, execSync } from "child_process";
import { mkdir, rm, access } from "fs/promises";
import { join } from "path";
import type {
  SandboxRuntime,
  SandboxConfig,
  SandboxInstance,
  ExecutionResult,
  SnapshotInfo,
} from "./types";

interface DockerConfig {
  image: string;
  btrfsPath: string; // Btrfs-mounted directory for workspaces
  networkMode: string;
}

const DEFAULT_CONFIG: DockerConfig = {
  image: process.env.DOCKER_IMAGE || "ubuntu:22.04",
  btrfsPath: process.env.DOCKER_BTRFS_PATH || "/tmp/sandbox-docker",
  networkMode: process.env.DOCKER_NETWORK || "bridge", // bridge allows networking by default
};

interface ActiveContainer {
  containerId: string;
  containerName: string;
  workspaceSubvol: string; // Btrfs subvolume for workspace
  snapshotsDir: string;
}

const activeContainers = new Map<string, ActiveContainer>();

export class DockerRuntime implements SandboxRuntime {
  readonly name = "docker";
  private config: DockerConfig;
  private btrfsReady = false;

  constructor(config: Partial<DockerConfig> = {}) {
    this.config = { ...DEFAULT_CONFIG, ...config };
  }

  async isAvailable(): Promise<boolean> {
    // Check Docker
    const dockerOk = await new Promise<boolean>((resolve) => {
      const proc = spawn("docker", ["info"], { stdio: "pipe" });
      proc.on("close", (code) => resolve(code === 0));
      proc.on("error", () => resolve(false));
    });

    if (!dockerOk) return false;

    // Try to setup Btrfs (optional optimization)
    await this.ensureBtrfs();
    // Return true even without Btrfs - we'll use cp fallback
    return true;
  }

  private async ensureBtrfs(): Promise<void> {
    try {
      // Check if btrfsPath exists and is Btrfs
      await access(this.config.btrfsPath);
      const fsType = execSync(`stat -f -c %T ${this.config.btrfsPath} 2>/dev/null || stat -f ${this.config.btrfsPath} | grep -o 'btrfs' || echo "unknown"`, {
        encoding: "utf-8",
      }).trim();

      if (fsType.includes("btrfs")) {
        this.btrfsReady = true;
        console.log(`[Docker] Using existing Btrfs at ${this.config.btrfsPath}`);
        return;
      }
    } catch {}

    // Try to create Btrfs loopback
    console.log(`[Docker] Setting up Btrfs at ${this.config.btrfsPath}...`);

    try {
      const imgPath = `${this.config.btrfsPath}.img`;

      // Create 10GB sparse image
      execSync(`sudo truncate -s 10G ${imgPath}`);
      execSync(`sudo mkfs.btrfs ${imgPath}`);
      execSync(`sudo mkdir -p ${this.config.btrfsPath}`);
      execSync(`sudo mount -o loop ${imgPath} ${this.config.btrfsPath}`);
      execSync(`sudo chmod 777 ${this.config.btrfsPath}`);

      this.btrfsReady = true;
      console.log(`[Docker] Btrfs ready at ${this.config.btrfsPath}`);
    } catch (e: any) {
      console.warn(`[Docker] Failed to setup Btrfs: ${e.message}`);
      console.warn(`[Docker] Falling back to regular copies (slower)`);
      this.btrfsReady = false;
    }
  }

  async createSandbox(config: SandboxConfig): Promise<SandboxInstance> {
    const id = `docker_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
    const containerName = `sandbox-${id}`;
    const sandboxDir = join(this.config.btrfsPath, id);
    const workspaceSubvol = join(sandboxDir, "workspace");
    const snapshotsDir = join(sandboxDir, "snapshots");

    console.log(`[Docker] Creating sandbox ${id}...`);

    await mkdir(sandboxDir, { recursive: true });
    await mkdir(snapshotsDir, { recursive: true });

    // Create Btrfs subvolume for workspace
    if (this.btrfsReady) {
      execSync(`btrfs subvolume create ${workspaceSubvol}`);
    } else {
      await mkdir(workspaceSubvol, { recursive: true });
    }

    // Make workspace writable
    execSync(`chmod 777 ${workspaceSubvol}`);

    // Build docker run command
    const args = [
      "run",
      "-d",
      "--name", containerName,
      "--memory", `${config.memoryMb || 512}m`,
      "--cpus", `${config.cpuCores || 1}`,
      "--network", this.config.networkMode,
      "-v", `${workspaceSubvol}:/workspace`,
      "-w", "/workspace",
      this.config.image,
      "tail", "-f", "/dev/null",
    ];

    return new Promise((resolve, reject) => {
      const proc = spawn("docker", args, { stdio: "pipe" });
      let stdout = "";
      let stderr = "";

      proc.stdout?.on("data", (data) => { stdout += data.toString(); });
      proc.stderr?.on("data", (data) => { stderr += data.toString(); });

      proc.on("close", (code) => {
        if (code !== 0) {
          reject(new Error(`Docker run failed: ${stderr}`));
          return;
        }

        const containerId = stdout.trim().slice(0, 12);

        activeContainers.set(id, {
          containerId,
          containerName,
          workspaceSubvol,
          snapshotsDir,
        });

        console.log(`[Docker] Sandbox ${id} ready (container: ${containerId})`);

        resolve({
          id,
          name: config.name,
          status: "running",
        });
      });

      proc.on("error", reject);
    });
  }

  async destroySandbox(id: string): Promise<void> {
    const container = activeContainers.get(id);
    if (!container) {
      throw new Error(`Sandbox ${id} not found`);
    }

    console.log(`[Docker] Destroying sandbox ${id}...`);

    // Stop container first (so volume isn't busy)
    try {
      execSync(`docker stop -t 2 ${container.containerName} 2>/dev/null || true`);
      execSync(`docker rm -f ${container.containerName} 2>/dev/null || true`);
    } catch {}

    // Delete Btrfs subvolumes
    if (this.btrfsReady) {
      try {
        // Delete all snapshot subvolumes
        const snapshots = await this.listSnapshots(id);
        for (const snap of snapshots) {
          const snapPath = join(container.snapshotsDir, snap.name);
          execSync(`btrfs subvolume delete ${snapPath} 2>/dev/null || true`);
        }
        // Delete workspace subvolume
        execSync(`btrfs subvolume delete ${container.workspaceSubvol} 2>/dev/null || true`);
      } catch {}
    }

    // Cleanup directory
    const sandboxDir = join(this.config.btrfsPath, id);
    try {
      await rm(sandboxDir, { recursive: true, force: true });
    } catch {}

    activeContainers.delete(id);
    console.log(`[Docker] Sandbox ${id} destroyed`);
  }

  async execute(id: string, command: string, timeout = 30000): Promise<ExecutionResult> {
    const container = activeContainers.get(id);
    if (!container) {
      throw new Error(`Sandbox ${id} not found`);
    }

    return new Promise((resolve) => {
      const args = ["exec", container.containerName, "/bin/sh", "-c", command];
      const proc = spawn("docker", args, { stdio: "pipe" });

      let stdout = "";
      let stderr = "";
      let killed = false;

      const timer = setTimeout(() => {
        killed = true;
        proc.kill("SIGKILL");
      }, timeout);

      proc.stdout?.on("data", (data) => { stdout += data.toString(); });
      proc.stderr?.on("data", (data) => { stderr += data.toString(); });

      proc.on("close", (code) => {
        clearTimeout(timer);
        resolve({
          stdout,
          stderr: killed ? stderr + "\nExecution timed out" : stderr,
          exitCode: killed ? 124 : (code ?? 1),
        });
      });

      proc.on("error", (err) => {
        clearTimeout(timer);
        resolve({ stdout: "", stderr: err.message, exitCode: 1 });
      });
    });
  }

  async snapshot(id: string, name: string): Promise<SnapshotInfo> {
    const container = activeContainers.get(id);
    if (!container) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const snapshotPath = join(container.snapshotsDir, name);

    if (this.btrfsReady) {
      // Instant Btrfs snapshot
      execSync(`btrfs subvolume snapshot ${container.workspaceSubvol} ${snapshotPath}`);
    } else {
      // Fallback to copy
      await mkdir(snapshotPath, { recursive: true });
      execSync(`cp -r ${container.workspaceSubvol}/. ${snapshotPath}/`);
    }

    return { name, createdAt: new Date() };
  }

  async restore(id: string, snapshotName: string): Promise<void> {
    const container = activeContainers.get(id);
    if (!container) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const snapshotPath = join(container.snapshotsDir, snapshotName);

    if (this.btrfsReady) {
      // Delete current workspace subvolume and snapshot from backup
      execSync(`btrfs subvolume delete ${container.workspaceSubvol}`);
      execSync(`btrfs subvolume snapshot ${snapshotPath} ${container.workspaceSubvol}`);
      execSync(`chmod 777 ${container.workspaceSubvol}`);
    } else {
      // Fallback to copy
      execSync(`rm -rf ${container.workspaceSubvol}/*`);
      execSync(`cp -r ${snapshotPath}/. ${container.workspaceSubvol}/`);
    }
  }

  async listSnapshots(id: string): Promise<SnapshotInfo[]> {
    const container = activeContainers.get(id);
    if (!container) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const { readdir, stat } = await import("fs/promises");

    try {
      const dirs = await readdir(container.snapshotsDir);
      const snapshots: SnapshotInfo[] = [];

      for (const name of dirs) {
        const s = await stat(join(container.snapshotsDir, name));
        if (s.isDirectory()) {
          snapshots.push({ name, createdAt: s.mtime });
        }
      }

      return snapshots;
    } catch {
      return [];
    }
  }

  async wipe(id: string): Promise<void> {
    const container = activeContainers.get(id);
    if (!container) {
      throw new Error(`Sandbox ${id} not found`);
    }

    if (this.btrfsReady) {
      // Recreate subvolume (fastest way to wipe)
      execSync(`btrfs subvolume delete ${container.workspaceSubvol}`);
      execSync(`btrfs subvolume create ${container.workspaceSubvol}`);
      execSync(`chmod 777 ${container.workspaceSubvol}`);
    } else {
      execSync(`rm -rf ${container.workspaceSubvol}/*`);
    }
  }

  async exportToFile(id: string, snapshotName?: string): Promise<string> {
    const container = activeContainers.get(id);
    if (!container) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const sandboxDir = join(this.config.btrfsPath, id);
    const exportPath = join(sandboxDir, `export_${Date.now()}.tar.gz`);
    const sourceDir = snapshotName
      ? join(container.snapshotsDir, snapshotName)
      : container.workspaceSubvol;

    execSync(`tar -czf ${exportPath} -C ${sourceDir} .`);

    return exportPath;
  }

  async importFromFile(id: string, snapshotName: string, filePath: string): Promise<void> {
    const container = activeContainers.get(id);
    if (!container) {
      throw new Error(`Sandbox ${id} not found`);
    }

    const snapshotPath = join(container.snapshotsDir, snapshotName);

    if (this.btrfsReady) {
      execSync(`btrfs subvolume create ${snapshotPath}`);
    } else {
      await mkdir(snapshotPath, { recursive: true });
    }

    execSync(`tar -xzf ${filePath} -C ${snapshotPath}`);
  }

  async healthCheck(id: string): Promise<boolean> {
    const container = activeContainers.get(id);
    if (!container) return false;

    try {
      const result = execSync(
        `docker inspect -f '{{.State.Running}}' ${container.containerName} 2>/dev/null`,
        { encoding: "utf-8" }
      );
      return result.trim() === "true";
    } catch {
      return false;
    }
  }
}

export const dockerRuntime = new DockerRuntime();
