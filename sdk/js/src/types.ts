// API Response Types

// Commands
export type CommandResult = 
    | { success: true; stdout: string; stderr: string; exitCode: number; snapshot: string }
    | { success: false; error: string };

// Snapshots
export type Snapshot = {
    id: number;
    vm_id: number;
    snapshot_name: string;
    command: string | null;
    s3_key: string | null;
    created_at: string;
};

export type SnapshotsResult =
    | { success: true; snapshots: Snapshot[] }
    | { success: false; error: string };

// VMs
export type VMInfo = {
    id: number;
    user_id: number;
    name: string;
    base_vm: string;
    status: string;
    agent_port: number;
    ssh_port: number;
    created_at: string;
    updated_at: string;
    agentHealthy?: boolean;
};

export type VMInfoResult =
    | { success: true; vm: VMInfo }
    | { success: false; error: string };

export type CreateVMResponse = {
    id: number;
    user_id: number;
    name: string;
    base_vm: string;
    status: string;
    agent_port: number;
    ssh_port: number;
    created_at: string;
    updated_at: string;
};

// S3 Export/Import
export type ExportInfo = {
    bucket: string;
    key: string;
    size: number;
    sha256: string;
    url: string;
};

export type ExportResult =
    | { success: true; export: ExportInfo }
    | { success: false; error: string };

// Generic
export type SimpleResult =
    | { success: true; message: string }
    | { success: false; error: string };
