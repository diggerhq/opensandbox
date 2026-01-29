import Sandbox from "./sandbox.js"
import type { CreateVMResponse, SnapshotsResult, VMInfo } from "./types.js"

type CreateResult = 
    | { success: true; sandbox: Sandbox }
    | { success: false; error: string };

type ListResult =
    | { success: true; vms: VMInfo[] }
    | { success: false; error: string };

class Client {
    constructor(
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
     * List all snapshots across all VMs for the current user.
     */
    async snapshots(): Promise<SnapshotsResult> {
        try {
            const response = await fetch(`${this.base_url}/snapshots`, {
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
     * Create a new sandbox VM.
     */
    async create(name: string): Promise<CreateResult> {
        try {
            const response = await fetch(`${this.base_url}/vms`, {
                method: "POST",
                headers: this.headers,
                body: JSON.stringify({ name }),
            });

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data: CreateVMResponse = await response.json();
            const sandbox = new Sandbox(data.name, this.api_key, this.base_url);
            
            return { success: true, sandbox };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * List all VMs for the current user.
     */
    async list(): Promise<ListResult> {
        try {
            const response = await fetch(`${this.base_url}/vms`, {
                method: "GET",
                headers: this.headers,
            });

            if (!response.ok) {
                const data = await response.json();
                return { success: false, error: data.error ?? `HTTP ${response.status}` };
            }

            const data = await response.json();
            return { success: true, vms: data.vms };
        } catch (err) {
            return { success: false, error: err instanceof Error ? err.message : "Unknown error" };
        }
    }

    /**
     * Get a reference to an existing sandbox by name.
     */
    sandbox(name: string): Sandbox {
        return new Sandbox(name, this.api_key, this.base_url);
    }
}

export default Client
