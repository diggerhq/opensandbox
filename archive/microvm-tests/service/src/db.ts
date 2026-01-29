import Database from "better-sqlite3";
import path from "path";
import crypto from "crypto";

const DB_PATH = process.env.DB_PATH || path.join(__dirname, "..", "vms.db");

const db = new Database(DB_PATH);

// Initialize schema
db.exec(`
  CREATE TABLE IF NOT EXISTS api_keys (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    key TEXT UNIQUE NOT NULL,
    name TEXT,
    created_at TEXT DEFAULT (datetime('now'))
  );

  CREATE TABLE IF NOT EXISTS vms (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    api_key_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    base_vm TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'creating',
    agent_port INTEGER,
    ssh_port INTEGER,
    created_at TEXT DEFAULT (datetime('now')),
    updated_at TEXT DEFAULT (datetime('now')),
    FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE,
    UNIQUE(api_key_id, name)
  );

  CREATE TABLE IF NOT EXISTS snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    vm_id INTEGER NOT NULL,
    snapshot_name TEXT NOT NULL,
    command TEXT,
    s3_key TEXT,
    created_at TEXT DEFAULT (datetime('now')),
    FOREIGN KEY (vm_id) REFERENCES vms(id) ON DELETE CASCADE,
    UNIQUE(vm_id, snapshot_name)
  );

  CREATE INDEX IF NOT EXISTS idx_api_keys_key ON api_keys(key);
  CREATE INDEX IF NOT EXISTS idx_vms_api_key ON vms(api_key_id);
  CREATE INDEX IF NOT EXISTS idx_snapshots_vm ON snapshots(vm_id);
`);

// ==================== API Keys ====================

export interface ApiKeyRecord {
  id: number;
  key: string;
  name: string | null;
  created_at: string;
}

export function generateApiKey(): string {
  return `ws_${crypto.randomBytes(24).toString("hex")}`;
}

export function createApiKey(name?: string): ApiKeyRecord {
  const key = generateApiKey();
  const stmt = db.prepare("INSERT INTO api_keys (key, name) VALUES (?, ?)");
  stmt.run(key, name || null);
  return getApiKeyByKey(key)!;
}

export function getApiKeyByKey(key: string): ApiKeyRecord | undefined {
  const stmt = db.prepare("SELECT * FROM api_keys WHERE key = ?");
  return stmt.get(key) as ApiKeyRecord | undefined;
}

export function getApiKeyById(id: number): ApiKeyRecord | undefined {
  const stmt = db.prepare("SELECT * FROM api_keys WHERE id = ?");
  return stmt.get(id) as ApiKeyRecord | undefined;
}

export function listApiKeys(): ApiKeyRecord[] {
  const stmt = db.prepare("SELECT * FROM api_keys ORDER BY created_at DESC");
  return stmt.all() as ApiKeyRecord[];
}

// ==================== VMs ====================

export interface VMRecord {
  id: number;
  api_key_id: number;
  name: string;
  base_vm: string;
  status: "creating" | "running" | "stopped" | "error";
  agent_port: number | null;
  ssh_port: number | null;
  created_at: string;
  updated_at: string;
}

export function createVM(
  apiKeyId: number,
  name: string,
  baseVM: string,
  agentPort: number,
  sshPort: number
): VMRecord {
  const stmt = db.prepare(`
    INSERT INTO vms (api_key_id, name, base_vm, status, agent_port, ssh_port)
    VALUES (?, ?, ?, 'creating', ?, ?)
  `);
  stmt.run(apiKeyId, name, baseVM, agentPort, sshPort);
  return getVMByKeyAndName(apiKeyId, name)!;
}

export function getVMById(id: number): VMRecord | undefined {
  const stmt = db.prepare("SELECT * FROM vms WHERE id = ?");
  return stmt.get(id) as VMRecord | undefined;
}

export function getVMByKeyAndName(apiKeyId: number, name: string): VMRecord | undefined {
  const stmt = db.prepare("SELECT * FROM vms WHERE api_key_id = ? AND name = ?");
  return stmt.get(apiKeyId, name) as VMRecord | undefined;
}

export function getVMsByApiKey(apiKeyId: number): VMRecord[] {
  const stmt = db.prepare("SELECT * FROM vms WHERE api_key_id = ? ORDER BY created_at DESC");
  return stmt.all(apiKeyId) as VMRecord[];
}

export function getAllVMs(): VMRecord[] {
  const stmt = db.prepare("SELECT * FROM vms ORDER BY created_at DESC");
  return stmt.all() as VMRecord[];
}

export function updateVMStatus(id: number, status: VMRecord["status"]): void {
  const stmt = db.prepare(`
    UPDATE vms SET status = ?, updated_at = datetime('now') WHERE id = ?
  `);
  stmt.run(status, id);
}

export function deleteVM(id: number): boolean {
  const stmt = db.prepare("DELETE FROM vms WHERE id = ?");
  const result = stmt.run(id);
  return result.changes > 0;
}

export function getNextPorts(): { agentPort: number; sshPort: number } {
  const stmt = db.prepare(`
    SELECT MAX(agent_port) as maxAgent, MAX(ssh_port) as maxSSH FROM vms
  `);
  const result = stmt.get() as { maxAgent: number | null; maxSSH: number | null };

  return {
    agentPort: (result.maxAgent || 3332) + 1,
    sshPort: (result.maxSSH || 2221) + 1,
  };
}

// ==================== Snapshots ====================

export interface SnapshotRecord {
  id: number;
  vm_id: number;
  snapshot_name: string;
  command: string | null;
  s3_key: string | null;
  created_at: string;
}

export function addSnapshot(
  vmId: number,
  snapshotName: string,
  command: string | null,
  s3Key?: string
): SnapshotRecord {
  const stmt = db.prepare(`
    INSERT INTO snapshots (vm_id, snapshot_name, command, s3_key)
    VALUES (?, ?, ?, ?)
  `);
  stmt.run(vmId, snapshotName, command, s3Key || null);
  return getSnapshot(vmId, snapshotName)!;
}

export function getSnapshot(vmId: number, snapshotName: string): SnapshotRecord | undefined {
  const stmt = db.prepare("SELECT * FROM snapshots WHERE vm_id = ? AND snapshot_name = ?");
  return stmt.get(vmId, snapshotName) as SnapshotRecord | undefined;
}

export function getSnapshotsForVM(vmId: number): SnapshotRecord[] {
  const stmt = db.prepare("SELECT * FROM snapshots WHERE vm_id = ? ORDER BY created_at ASC");
  return stmt.all(vmId) as SnapshotRecord[];
}

export function getSnapshotsByApiKey(apiKeyId: number): SnapshotRecord[] {
  const stmt = db.prepare(`
    SELECT s.* FROM snapshots s
    JOIN vms v ON s.vm_id = v.id
    WHERE v.api_key_id = ?
    ORDER BY s.created_at DESC
  `);
  return stmt.all(apiKeyId) as SnapshotRecord[];
}

export function deleteSnapshotsForVM(vmId: number): void {
  const stmt = db.prepare("DELETE FROM snapshots WHERE vm_id = ?");
  stmt.run(vmId);
}

export function deleteSnapshotsAfter(vmId: number, snapshotName: string): void {
  const stmt = db.prepare(`
    DELETE FROM snapshots 
    WHERE vm_id = ? AND created_at > (
      SELECT created_at FROM snapshots WHERE vm_id = ? AND snapshot_name = ?
    )
  `);
  stmt.run(vmId, vmId, snapshotName);
}

export function updateSnapshotS3Key(vmId: number, snapshotName: string, s3Key: string): void {
  const stmt = db.prepare(`
    UPDATE snapshots SET s3_key = ? WHERE vm_id = ? AND snapshot_name = ?
  `);
  stmt.run(s3Key, vmId, snapshotName);
}

export default db;
