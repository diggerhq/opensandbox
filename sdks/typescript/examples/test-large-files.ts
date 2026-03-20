/**
 * Large File Streaming Test
 *
 * Tests:
 *   1. Write a 100MB file via SDK, read back and verify size
 *   2. Download 100MB file via signed URL, verify content integrity (SHA-256)
 *   3. Upload 100MB file via signed URL, verify content integrity
 *   4. Write 50MB via writeStream, read back via readStream
 *
 * Usage:
 *   npx tsx examples/test-large-files.ts
 */

import { createHash } from "crypto";
import { Sandbox } from "../src/index";

function green(msg: string) { console.log(`\x1b[32m✓ ${msg}\x1b[0m`); }
function red(msg: string) { console.log(`\x1b[31m✗ ${msg}\x1b[0m`); }
function bold(msg: string) { console.log(`\x1b[1m${msg}\x1b[0m`); }
function dim(msg: string) { console.log(`\x1b[2m  ${msg}\x1b[0m`); }

let passed = 0;
let failed = 0;

function check(desc: string, condition: boolean, detail?: string) {
  if (condition) {
    green(desc);
    passed++;
  } else {
    red(`${desc}${detail ? ` (${detail})` : ""}`);
    failed++;
  }
}

/** Generate deterministic data: repeating pattern so we can verify integrity */
function generateData(sizeMB: number): Uint8Array {
  const size = sizeMB * 1024 * 1024;
  const buf = new Uint8Array(size);
  // Fill with repeating 256-byte pattern
  for (let i = 0; i < size; i++) {
    buf[i] = i % 256;
  }
  return buf;
}

function sha256(data: Uint8Array): string {
  return createHash("sha256").update(data).digest("hex");
}

async function streamToBuffer(stream: ReadableStream<Uint8Array>): Promise<Uint8Array> {
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let totalLen = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    totalLen += value.length;
  }
  const result = new Uint8Array(totalLen);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.length;
  }
  return result;
}

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║         Large File Streaming Test                ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let sandbox: Sandbox | null = null;

  try {
    sandbox = await Sandbox.create({ template: "base", timeout: 300 });
    green(`Created sandbox: ${sandbox.sandboxId}`);
    console.log();

    // ── Test 1: Write and read 100MB file via SDK ────────────────
    bold("━━━ Test 1: Write/read 100MB file via SDK ━━━\n");

    const data100 = generateData(100);
    const hash100 = sha256(data100);
    dim(`Generated 100MB data, SHA-256: ${hash100.slice(0, 16)}...`);

    const t0 = Date.now();
    await sandbox.files.write("/root/large100.bin", data100);
    const writeMs = Date.now() - t0;
    dim(`Write took ${writeMs}ms (${(100 * 1000 / writeMs).toFixed(1)} MB/s)`);
    green("100MB write completed");

    const t1 = Date.now();
    const readBack = await sandbox.files.readBytes("/root/large100.bin");
    const readMs = Date.now() - t1;
    dim(`Read took ${readMs}ms (${(100 * 1000 / readMs).toFixed(1)} MB/s)`);

    check("Read returns correct size", readBack.length === data100.length,
      `got ${readBack.length}, expected ${data100.length}`);
    const readHash = sha256(readBack);
    check("SHA-256 matches after read", readHash === hash100,
      `got ${readHash.slice(0, 16)}...`);
    console.log();

    // ── Test 2: Download 100MB via signed URL ────────────────────
    bold("━━━ Test 2: Download 100MB via signed URL ━━━\n");

    const dlUrl = await sandbox.downloadUrl("/root/large100.bin", { expiresIn: 300 });
    dim(`Download URL generated`);

    const t2 = Date.now();
    const dlRes = await fetch(dlUrl);
    check("Signed URL returns 200", dlRes.status === 200);

    const contentLength = dlRes.headers.get("content-length");
    dim(`Content-Length: ${contentLength}`);
    check("Content-Length is 100MB", contentLength === String(data100.length));

    const dlData = new Uint8Array(await dlRes.arrayBuffer());
    const dlMs = Date.now() - t2;
    dim(`Download took ${dlMs}ms (${(100 * 1000 / dlMs).toFixed(1)} MB/s)`);

    check("Downloaded size correct", dlData.length === data100.length);
    const dlHash = sha256(dlData);
    check("SHA-256 matches via signed URL download", dlHash === hash100);
    console.log();

    // ── Test 3: Upload 100MB via signed URL ──────────────────────
    bold("━━━ Test 3: Upload 100MB via signed URL ━━━\n");

    const upUrl = await sandbox.uploadUrl("/root/uploaded100.bin");
    dim(`Upload URL generated`);

    const t3 = Date.now();
    const upRes = await fetch(upUrl, {
      method: "PUT",
      body: data100,
    });
    const upMs = Date.now() - t3;
    dim(`Upload took ${upMs}ms (${(100 * 1000 / upMs).toFixed(1)} MB/s)`);
    check("Upload returns 204", upRes.status === 204);

    // Verify via SDK readBytes
    const upReadBack = await sandbox.files.readBytes("/root/uploaded100.bin");
    check("Uploaded file size correct", upReadBack.length === data100.length);
    const upHash = sha256(upReadBack);
    check("SHA-256 matches after signed URL upload", upHash === hash100);
    console.log();

    // ── Test 4: readStream / writeStream (50MB) ──────────────────
    bold("━━━ Test 4: readStream / writeStream 50MB ━━━\n");

    const data50 = generateData(50);
    const hash50 = sha256(data50);
    dim(`Generated 50MB data, SHA-256: ${hash50.slice(0, 16)}...`);

    // writeStream
    const t4 = Date.now();
    await sandbox.files.writeStream("/root/stream50.bin", data50);
    const wsMs = Date.now() - t4;
    dim(`writeStream took ${wsMs}ms`);
    green("50MB writeStream completed");

    // readStream
    const t5 = Date.now();
    const readStream = await sandbox.files.readStream("/root/stream50.bin");
    const streamData = await streamToBuffer(readStream);
    const rsMs = Date.now() - t5;
    dim(`readStream took ${rsMs}ms`);

    check("readStream returns correct size", streamData.length === data50.length,
      `got ${streamData.length}, expected ${data50.length}`);
    const streamHash = sha256(streamData);
    check("SHA-256 matches via readStream", streamHash === hash50);
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    if (sandbox) {
      await sandbox.kill();
      green("Sandbox killed");
    }
  }

  // --- Summary ---
  bold("========================================");
  bold(` Results: ${passed} passed, ${failed} failed`);
  bold("========================================\n");
  if (failed > 0) process.exit(1);
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
