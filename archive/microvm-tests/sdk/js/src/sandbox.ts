import type { CommandResult, SnapshotsResult, VMInfoResult, SimpleResult, ExportResult } from "./types.js"

class Sandbox {
    constructor(
        public readonly name: string,
        private readonly api_key: string,
        private readonly base_url: string = "http://localhost:4000"
    ) {}

    private get headers(): HeadersInit {
        return {
            "Content-Type": "application/json",
            "x-api-key": this.api_key,
        };
    }

    /**
     * Execute a shell command. A snapshot is automatically taken after each successful command.
     */
    async run(command: string): Promise<CommandResult> {
        try {
            const response = await fetch(`${this.base_url}/vms/${this.name}/run`, {
                method: "POST",
                headers: this.headers,
                body: JSON.stringify({ command }),
            });

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data = await response.json();
            return { 
                success: true, 
                stdout: data.stdout, 
                stderr: data.stderr, 
                exitCode: data.exitCode,
                snapshot: data.snapshot,
            };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * Get detailed information about the VM.
     */
    async info(): Promise<VMInfoResult> {
        try {
            const response = await fetch(`${this.base_url}/vms/${this.name}`, {
                method: "GET",
                headers: this.headers,
            });

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const vm = await response.json();
            return { success: true, vm };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * Delete the VM and all its snapshots permanently.
     */
    async destroy(): Promise<SimpleResult> {
        try {
            const response = await fetch(`${this.base_url}/vms/${this.name}`, {
                method: "DELETE",
                headers: this.headers,
            });

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data = await response.json();
            return { success: true, message: data.message };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * Clear all files in the workspace without creating a snapshot.
     */
    async wipe(): Promise<SimpleResult> {
        try {
            const response = await fetch(`${this.base_url}/vms/${this.name}/wipe`, {
                method: "POST",
                headers: this.headers,
            });

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data = await response.json();
            return { success: true, message: data.message };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * List all snapshots for this sandbox.
     */
    async snapshots(): Promise<SnapshotsResult> {
        try {
            const response = await fetch(`${this.base_url}/vms/${this.name}/snapshots`, {
                method: "GET",
                headers: this.headers,
            });

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data = await response.json();
            return { success: true, snapshots: data.snapshots };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * Restore workspace to a previous snapshot. All snapshots after this point are deleted.
     */
    async restore(snapshot_name: string): Promise<SimpleResult> {
        try {
            const response = await fetch(
                `${this.base_url}/vms/${this.name}/snapshots/${snapshot_name}/restore`,
                {
                    method: "POST",
                    headers: this.headers,
                }
            );

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data = await response.json();
            return { success: true, message: data.message };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * Export current workspace (or a snapshot) to S3.
     * @param snapshot - Snapshot name to export, defaults to "workspace" (current state)
     * @param key - Optional custom S3 key path
     */
    async export(options: { snapshot?: string; key?: string } = {}): Promise<ExportResult> {
        try {
            const response = await fetch(`${this.base_url}/vms/${this.name}/export`, {
                method: "POST",
                headers: this.headers,
                body: JSON.stringify(options),
            });

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data = await response.json();
            return { 
                success: true, 
                export: {
                    bucket: data.bucket,
                    key: data.key,
                    size: data.size,
                    sha256: data.sha256,
                    url: data.url,
                },
            };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * Export a specific snapshot to S3.
     * @param snapshot_name - Name of the snapshot to export
     * @param key - Optional custom S3 key path
     */
    async exportSnapshot(snapshot_name: string, key?: string): Promise<ExportResult> {
        try {
            const response = await fetch(
                `${this.base_url}/vms/${this.name}/snapshots/${snapshot_name}/export`,
                {
                    method: "POST",
                    headers: this.headers,
                    body: JSON.stringify(key ? { key } : {}),
                }
            );

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data = await response.json();
            return { 
                success: true, 
                export: {
                    bucket: data.bucket,
                    key: data.key,
                    size: data.size,
                    sha256: data.sha256,
                    url: data.url,
                },
            };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * Import a snapshot from S3.
     * @param name - Name for the new snapshot
     * @param key - S3 key to import from
     */
    async import(name: string, key: string): Promise<SimpleResult> {
        try {
            const response = await fetch(`${this.base_url}/vms/${this.name}/import`, {
                method: "POST",
                headers: this.headers,
                body: JSON.stringify({ name, key }),
            });

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data = await response.json();
            return { success: true, message: data.message };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }
}

export default Sandbox
