import { Readable } from "stream";
import { S3Client, GetObjectCommand } from "@aws-sdk/client-s3";
import { Upload } from "@aws-sdk/lib-storage";
import {
  createVM,
  getVMById,
  getVMByKeyAndName,
  getVMsByApiKey,
  updateVMStatus,
  deleteVM,
  getNextPorts,
  VMRecord,
  addSnapshot,
  getSnapshotsForVM,
  getSnapshotsByApiKey,
  deleteSnapshotsForVM,
  deleteSnapshotsAfter,
  updateSnapshotS3Key,
  SnapshotRecord,
} from "./db";
import { getRuntime, SandboxRuntime, SandboxInstance } from "./runtime";

// S3 config
const S3_BUCKET = process.env.S3_BUCKET;
const S3_REGION = process.env.S3_REGION || "us-east-1";
const S3_PREFIX = process.env.S3_PREFIX || "snapshots/";

const s3 = S3_BUCKET ? new S3Client({ region: S3_REGION }) : null;

// Map DB VM id -> runtime sandbox id
const vmToSandbox = new Map<number, string>();

// Cached runtime instance
let runtime: SandboxRuntime | null = null;

async function getActiveRuntime(): Promise<SandboxRuntime> {
  if (!runtime) {
    runtime = await getRuntime();
    console.log(`[VM Manager] Using runtime: ${runtime.name}`);
  }
  return runtime;
}

/**
 * Create a new sandbox VM
 */
export async function spinUpVM(apiKeyId: number, name: string): Promise<VMRecord> {
  // Check if VM already exists for this API key
  const existing = getVMByKeyAndName(apiKeyId, name);
  if (existing) {
    throw new Error(`VM "${name}" already exists`);
  }

  const rt = await getActiveRuntime();

  // Get next available ports (for DB tracking)
  const { agentPort, sshPort } = getNextPorts();

  // Create DB record
  const vm = createVM(apiKeyId, name, rt.name, agentPort, sshPort);

  try {
    console.log(`[VM] Creating sandbox "${name}" with ${rt.name}...`);

    // Create sandbox via runtime
    const sandbox = await rt.createSandbox({
      name: `key${apiKeyId}_${name}`,
      cpuCores: 1,
      memoryMb: 512,
    });

    // Track mapping
    vmToSandbox.set(vm.id, sandbox.id);

    // Take initial snapshot
    console.log(`[VM] Taking initial snapshot...`);
    await rt.snapshot(sandbox.id, "initial");
    addSnapshot(vm.id, "initial", null);

    updateVMStatus(vm.id, "running");
    console.log(`[VM] "${name}" is ready (sandbox: ${sandbox.id})`);

    return getVMById(vm.id)!;
  } catch (error: any) {
    console.error(`[VM] Error creating "${name}":`, error.message);
    updateVMStatus(vm.id, "error");
    throw error;
  }
}

/**
 * Shut down and destroy a VM
 */
export async function spinDownVM(apiKeyId: number, name: string): Promise<void> {
  const vm = getVMByKeyAndName(apiKeyId, name);
  if (!vm) {
    throw new Error(`VM "${name}" not found`);
  }

  const rt = await getActiveRuntime();
  const sandboxId = vmToSandbox.get(vm.id);

  console.log(`[VM] Shutting down "${name}"...`);

  if (sandboxId) {
    try {
      await rt.destroySandbox(sandboxId);
    } catch (error: any) {
      console.error(`[VM] Error destroying sandbox:`, error.message);
    }
    vmToSandbox.delete(vm.id);
  }

  // Remove from database
  deleteSnapshotsForVM(vm.id);
  deleteVM(vm.id);
  console.log(`[VM] "${name}" destroyed`);
}

/**
 * List VMs for an API key
 */
export function listVMs(apiKeyId: number): VMRecord[] {
  return getVMsByApiKey(apiKeyId);
}

/**
 * List all snapshots for an API key (across all VMs)
 */
export function listAllSnapshots(apiKeyId: number): SnapshotRecord[] {
  return getSnapshotsByApiKey(apiKeyId);
}

/**
 * Get info about a specific VM
 */
export async function getVMInfo(apiKeyId: number, name: string): Promise<VMRecord & { agentHealthy: boolean }> {
  const vm = getVMByKeyAndName(apiKeyId, name);
  if (!vm) {
    throw new Error(`VM "${name}" not found`);
  }

  const rt = await getActiveRuntime();
  const sandboxId = vmToSandbox.get(vm.id);

  let agentHealthy = false;
  if (sandboxId) {
    agentHealthy = await rt.healthCheck(sandboxId);
  }

  return { ...vm, agentHealthy };
}

/**
 * Run a command on a VM and take a snapshot afterward
 */
export async function runOnVM(
  apiKeyId: number,
  name: string,
  command: string,
  timeout = 30
): Promise<{ stdout: string; stderr: string; exitCode: number; snapshot: string }> {
  const vm = getVMByKeyAndName(apiKeyId, name);
  if (!vm) {
    throw new Error(`VM "${name}" not found`);
  }

  const rt = await getActiveRuntime();
  const sandboxId = vmToSandbox.get(vm.id);
  if (!sandboxId) {
    throw new Error(`VM "${name}" sandbox not found`);
  }

  // Execute command
  const result = await rt.execute(sandboxId, command, timeout * 1000);

  // Take snapshot
  const snapshotName = `cmd_${Date.now()}`;
  await rt.snapshot(sandboxId, snapshotName);
  addSnapshot(vm.id, snapshotName, command);

  return { ...result, snapshot: snapshotName };
}

/**
 * Wipe workspace on a VM
 */
export async function wipeVM(apiKeyId: number, name: string): Promise<void> {
  const vm = getVMByKeyAndName(apiKeyId, name);
  if (!vm) {
    throw new Error(`VM "${name}" not found`);
  }

  const rt = await getActiveRuntime();
  const sandboxId = vmToSandbox.get(vm.id);
  if (!sandboxId) {
    throw new Error(`VM "${name}" sandbox not found`);
  }

  await rt.wipe(sandboxId);
}

/**
 * List all snapshots for a VM
 */
export function listSnapshots(apiKeyId: number, name: string): SnapshotRecord[] {
  const vm = getVMByKeyAndName(apiKeyId, name);
  if (!vm) {
    throw new Error(`VM "${name}" not found`);
  }
  return getSnapshotsForVM(vm.id);
}

/**
 * Restore a VM to a specific snapshot
 */
export async function restoreSnapshot(apiKeyId: number, vmName: string, snapshotName: string): Promise<void> {
  const vm = getVMByKeyAndName(apiKeyId, vmName);
  if (!vm) {
    throw new Error(`VM "${vmName}" not found`);
  }

  const rt = await getActiveRuntime();
  const sandboxId = vmToSandbox.get(vm.id);
  if (!sandboxId) {
    throw new Error(`VM "${vmName}" sandbox not found`);
  }

  console.log(`[VM] Restoring "${vmName}" to snapshot "${snapshotName}"...`);

  await rt.restore(sandboxId, snapshotName);
  deleteSnapshotsAfter(vm.id, snapshotName);

  console.log(`[VM] "${vmName}" restored to "${snapshotName}"`);
}

interface ExportResult {
  bucket: string;
  key: string;
  size: number;
  url: string;
}

/**
 * Export a snapshot to S3
 */
export async function exportToS3(
  apiKeyId: number,
  vmName: string,
  snapshotName = "workspace",
  s3Key?: string
): Promise<ExportResult> {
  if (!s3 || !S3_BUCKET) {
    throw new Error("S3 not configured (set S3_BUCKET env var)");
  }

  const vm = getVMByKeyAndName(apiKeyId, vmName);
  if (!vm) {
    throw new Error(`VM "${vmName}" not found`);
  }

  const rt = await getActiveRuntime();
  const sandboxId = vmToSandbox.get(vm.id);
  if (!sandboxId) {
    throw new Error(`VM "${vmName}" sandbox not found`);
  }

  console.log(`[VM] Exporting "${snapshotName}" from "${vmName}" to S3...`);

  // Get export file/URL from runtime
  const exportUrl = await rt.exportToFile(sandboxId, snapshotName === "workspace" ? undefined : snapshotName);

  // Stream to S3
  const finalKey = s3Key || `${S3_PREFIX}key${apiKeyId}/${vmName}/${snapshotName}_${Date.now()}.tar.gz`;

  // If it's a URL, fetch and stream
  if (exportUrl.startsWith("http")) {
    const res = await fetch(exportUrl);
    if (!res.ok || !res.body) {
      throw new Error("Failed to download export");
    }

    const upload = new Upload({
      client: s3,
      params: {
        Bucket: S3_BUCKET,
        Key: finalKey,
        Body: Readable.fromWeb(res.body as any),
        ContentType: "application/gzip",
      },
    });

    await upload.done();
  } else {
    // It's a local file path
    const { createReadStream, statSync } = await import("fs");
    const stream = createReadStream(exportUrl);

    const upload = new Upload({
      client: s3,
      params: {
        Bucket: S3_BUCKET,
        Key: finalKey,
        Body: stream,
        ContentType: "application/gzip",
      },
    });

    await upload.done();
  }

  // Update snapshot record with S3 key
  if (snapshotName !== "workspace") {
    updateSnapshotS3Key(vm.id, snapshotName, finalKey);
  }

  console.log(`[VM] Exported to s3://${S3_BUCKET}/${finalKey}`);

  return {
    bucket: S3_BUCKET,
    key: finalKey,
    size: 0, // Would need to track this
    url: `s3://${S3_BUCKET}/${finalKey}`,
  };
}

/**
 * Import a snapshot from S3
 */
export async function importFromS3(
  apiKeyId: number,
  vmName: string,
  snapshotName: string,
  s3Key: string
): Promise<void> {
  if (!s3 || !S3_BUCKET) {
    throw new Error("S3 not configured (set S3_BUCKET env var)");
  }

  const vm = getVMByKeyAndName(apiKeyId, vmName);
  if (!vm) {
    throw new Error(`VM "${vmName}" not found`);
  }

  const rt = await getActiveRuntime();
  const sandboxId = vmToSandbox.get(vm.id);
  if (!sandboxId) {
    throw new Error(`VM "${vmName}" sandbox not found`);
  }

  console.log(`[VM] Importing "${snapshotName}" to "${vmName}" from S3...`);

  // Download from S3 to temp file
  const getCommand = new GetObjectCommand({
    Bucket: S3_BUCKET,
    Key: s3Key,
  });

  const s3Response = await s3.send(getCommand);
  if (!s3Response.Body) {
    throw new Error("Failed to get object from S3");
  }

  // Write to temp file
  const { writeFile, unlink } = await import("fs/promises");
  const tempPath = `/tmp/import_${Date.now()}.tar.gz`;

  const chunks: Buffer[] = [];
  for await (const chunk of s3Response.Body as any) {
    chunks.push(chunk);
  }
  await writeFile(tempPath, Buffer.concat(chunks));

  // Import via runtime
  await rt.importFromFile(sandboxId, snapshotName, tempPath);

  // Cleanup temp file
  await unlink(tempPath);

  // Add to DB
  addSnapshot(vm.id, snapshotName, `[imported from s3://${s3Key}]`, s3Key);

  console.log(`[VM] Imported "${snapshotName}" to "${vmName}"`);
}
