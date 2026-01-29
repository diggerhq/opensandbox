import express, { Request, Response, NextFunction } from "express";
import cors from "cors";
import dotenv from "dotenv";
import {
  spinUpVM,
  spinDownVM,
  listVMs,
  listAllSnapshots,
  getVMInfo,
  runOnVM,
  wipeVM,
  listSnapshots,
  restoreSnapshot,
  exportToS3,
  importFromS3,
} from "./vm-manager";
import { createApiKey, getApiKeyByKey, ApiKeyRecord } from "./db";

dotenv.config();

const app = express();

app.use(cors());
app.use(express.json());

const PORT = process.env.PORT || 4000;

// Extend Request to include auth context
declare global {
  namespace Express {
    interface Request {
      auth?: ApiKeyRecord;
    }
  }
}

/**
 * Middleware to validate API key
 */
function requireApiKey(req: Request, res: Response, next: NextFunction): void {
  const apiKey = req.headers["x-api-key"] as string;

  if (!apiKey) {
    res.status(401).json({ error: "API key required (x-api-key header)" });
    return;
  }

  const auth = getApiKeyByKey(apiKey);
  if (!auth) {
    res.status(403).json({ error: "Invalid API key" });
    return;
  }

  req.auth = auth;
  next();
}

// ==================== Public Routes ====================

// Health check
app.get("/health", (_req, res) => {
  res.json({ status: "ok", timestamp: new Date().toISOString() });
});

// Create API key
app.post("/api-keys", (req, res) => {
  const { name } = req.body;
  try {
    const apiKey = createApiKey(name);
    res.status(201).json({
      id: apiKey.id,
      key: apiKey.key,
      name: apiKey.name,
      created_at: apiKey.created_at,
      message: "Save your API key - it won't be shown again!",
    });
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// ==================== Authenticated Routes ====================

app.use(requireApiKey);

// ==================== Account Info ====================

// Get current account info
app.get("/me", (req, res) => {
  res.json({
    api_key_id: req.auth!.id,
    api_key_name: req.auth!.name,
    created_at: req.auth!.created_at,
  });
});

// List all snapshots (across all VMs)
app.get("/snapshots", (req, res) => {
  try {
    const snapshots = listAllSnapshots(req.auth!.id);
    res.json({ snapshots });
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// ==================== VM Management ====================

// List VMs
app.get("/vms", (req, res) => {
  try {
    const vms = listVMs(req.auth!.id);
    res.json({ vms });
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// Create a new VM
app.post("/vms", async (req, res) => {
  const { name } = req.body;

  if (!name) {
    res.status(400).json({ error: "name is required" });
    return;
  }

  try {
    console.log(`[API] Key ${req.auth!.id} creating VM: ${name}`);
    const vm = await spinUpVM(req.auth!.id, name);
    res.status(201).json(vm);
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// Get info about a specific VM
app.get("/vms/:name", async (req, res) => {
  try {
    const vm = await getVMInfo(req.auth!.id, req.params.name);
    res.json(vm);
  } catch (error: any) {
    res.status(404).json({ error: error.message });
  }
});

// Destroy a VM
app.delete("/vms/:name", async (req, res) => {
  try {
    console.log(`[API] Key ${req.auth!.id} destroying VM: ${req.params.name}`);
    await spinDownVM(req.auth!.id, req.params.name);
    res.json({ message: `VM "${req.params.name}" destroyed` });
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// ==================== Command Execution ====================

// Run a command on a VM
app.post("/vms/:name/run", async (req, res) => {
  const { command, timeout } = req.body;

  if (!command) {
    res.status(400).json({ error: "command is required" });
    return;
  }

  try {
    const result = await runOnVM(req.auth!.id, req.params.name, command, timeout);
    res.json(result);
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// Wipe workspace on a VM
app.post("/vms/:name/wipe", async (req, res) => {
  try {
    await wipeVM(req.auth!.id, req.params.name);
    res.json({ message: "Workspace wiped" });
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// ==================== Snapshots ====================

// List all snapshots for a VM
app.get("/vms/:name/snapshots", (req, res) => {
  try {
    const snapshots = listSnapshots(req.auth!.id, req.params.name);
    res.json({ snapshots });
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// Restore to a specific snapshot
app.post("/vms/:name/snapshots/:snapshot/restore", async (req, res) => {
  try {
    console.log(`[API] Key ${req.auth!.id} restoring ${req.params.name} to ${req.params.snapshot}`);
    await restoreSnapshot(req.auth!.id, req.params.name, req.params.snapshot);
    res.json({ message: `Restored to snapshot "${req.params.snapshot}"` });
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// ==================== Export / Import (S3) ====================

// Export current workspace to S3
app.post("/vms/:name/export", async (req, res) => {
  const { snapshot, key } = req.body;
  try {
    console.log(`[API] Key ${req.auth!.id} exporting from ${req.params.name} to S3`);
    const result = await exportToS3(req.auth!.id, req.params.name, snapshot, key);
    res.json(result);
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// Export a specific snapshot to S3
app.post("/vms/:name/snapshots/:snapshot/export", async (req, res) => {
  const { key } = req.body;
  try {
    console.log(`[API] Key ${req.auth!.id} exporting ${req.params.snapshot} from ${req.params.name} to S3`);
    const result = await exportToS3(req.auth!.id, req.params.name, req.params.snapshot, key);
    res.json(result);
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

// Import a snapshot from S3
app.post("/vms/:name/import", async (req, res) => {
  const { name, key } = req.body;
  if (!name || !key) {
    res.status(400).json({ error: "name (snapshot name) and key (S3 key) required" });
    return;
  }
  try {
    console.log(`[API] Key ${req.auth!.id} importing ${name} to ${req.params.name} from S3`);
    await importFromS3(req.auth!.id, req.params.name, name, key);
    res.json({ message: `Imported snapshot "${name}" from S3` });
  } catch (error: any) {
    res.status(500).json({ error: error.message });
  }
});

app.listen(PORT, () => {
  console.log(`🚀 Server listening on http://localhost:${PORT}`);
  console.log(`   Base VM: ${process.env.BASE_VM || "dev-1"}`);
  console.log(`   Get API key: POST /api-keys`);
});
