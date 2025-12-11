/**
 * Runtime abstraction for sandbox execution backends
 */

export interface SandboxConfig {
  name: string;
  cpuCores?: number;
  memoryMb?: number;
  timeout?: number; // ms
}

export interface ExecutionResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

export interface SnapshotInfo {
  name: string;
  createdAt: Date;
}

export interface SandboxInstance {
  id: string;
  name: string;
  status: "creating" | "running" | "stopped" | "error";
  agentUrl?: string; // For agent-based runtimes
}

/**
 * Runtime interface - implement this to add a new backend
 */
export interface SandboxRuntime {
  /** Runtime name (e.g., "virtualbox", "firecracker") */
  readonly name: string;

  /** Check if this runtime is available on the current system */
  isAvailable(): Promise<boolean>;

  /** Create and start a new sandbox */
  createSandbox(config: SandboxConfig): Promise<SandboxInstance>;

  /** Destroy a sandbox */
  destroySandbox(id: string): Promise<void>;

  /** Execute a command in a sandbox */
  execute(id: string, command: string, timeout?: number): Promise<ExecutionResult>;

  /** Take a filesystem snapshot */
  snapshot(id: string, name: string): Promise<SnapshotInfo>;

  /** Restore to a snapshot */
  restore(id: string, snapshotName: string): Promise<void>;

  /** List snapshots for a sandbox */
  listSnapshots(id: string): Promise<SnapshotInfo[]>;

  /** Wipe the workspace */
  wipe(id: string): Promise<void>;

  /** Export workspace/snapshot to a file, return path */
  exportToFile(id: string, snapshotName?: string): Promise<string>;

  /** Import from a file */
  importFromFile(id: string, snapshotName: string, filePath: string): Promise<void>;

  /** Health check for a sandbox */
  healthCheck(id: string): Promise<boolean>;
}

/**
 * Runtime registry - manages available runtimes
 */
export class RuntimeRegistry {
  private runtimes = new Map<string, SandboxRuntime>();
  private defaultRuntime: string | null = null;

  register(runtime: SandboxRuntime): void {
    this.runtimes.set(runtime.name, runtime);
    if (!this.defaultRuntime) {
      this.defaultRuntime = runtime.name;
    }
  }

  setDefault(name: string): void {
    if (!this.runtimes.has(name)) {
      throw new Error(`Runtime "${name}" not registered`);
    }
    this.defaultRuntime = name;
  }

  get(name?: string): SandboxRuntime {
    const runtimeName = name || this.defaultRuntime;
    if (!runtimeName) {
      throw new Error("No runtime available");
    }
    const runtime = this.runtimes.get(runtimeName);
    if (!runtime) {
      throw new Error(`Runtime "${runtimeName}" not found`);
    }
    return runtime;
  }

  list(): string[] {
    return Array.from(this.runtimes.keys());
  }

  async findAvailable(): Promise<SandboxRuntime | null> {
    for (const runtime of this.runtimes.values()) {
      if (await runtime.isAvailable()) {
        return runtime;
      }
    }
    return null;
  }
}

export const runtimeRegistry = new RuntimeRegistry();

