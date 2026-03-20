/**
 * Signed Download/Upload URL Test
 *
 * Tests:
 *   1. Generate a signed download URL and fetch file content without API key
 *   2. Modify file after URL generation — verify URL serves live content
 *   3. Generate a signed upload URL and PUT new content without API key
 *   4. Verify uploaded content via SDK read
 *   5. Expired URL returns 403
 *   6. Tampered signature returns 403
 *
 * Usage:
 *   npx tsx examples/test-signed-urls.ts
 */

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

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       Signed Download/Upload URL Test            ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let sandbox: Sandbox | null = null;

  try {
    sandbox = await Sandbox.create({ template: "base", timeout: 120 });
    green(`Created sandbox: ${sandbox.sandboxId}`);
    console.log();

    // ── Test 1: Signed download URL ────────────────────────────────
    bold("━━━ Test 1: Signed download URL ━━━\n");

    const originalContent = "ORIGINAL CONTENT - written before signed URL generation";
    await sandbox.files.write("/tmp/hello.txt", originalContent);
    dim("Wrote /tmp/hello.txt with original content");

    const downloadUrl = await sandbox.downloadUrl("/tmp/hello.txt");
    dim(`Download URL: ${downloadUrl}`);

    // Inspect URL structure
    const urlObj = new URL(downloadUrl);
    dim(`Host: ${urlObj.host}`);
    dim(`Path: ${urlObj.pathname}`);
    check("URL contains sandbox ID", downloadUrl.includes(sandbox.sandboxId));
    check("URL contains signature param", urlObj.searchParams.has("signature"));
    check("URL contains expires param", urlObj.searchParams.has("expires"));

    // Fetch without API key — just a plain GET
    const res1 = await fetch(downloadUrl);
    const body1 = await res1.text();
    dim(`Status: ${res1.status}`);
    dim(`Body: "${body1.trim()}"`);
    check("Download URL returns 200", res1.status === 200);
    check("Content matches original", body1 === originalContent);
    console.log();

    // ── Test 2: URL serves live content (not a snapshot) ───────────
    bold("━━━ Test 2: Live content (not snapshot) ━━━\n");

    const modifiedContent = "MODIFIED CONTENT - written AFTER signed URL generation";
    await sandbox.files.write("/tmp/hello.txt", modifiedContent);
    dim("Overwrote /tmp/hello.txt with modified content");

    // Fetch the SAME URL again
    const res2 = await fetch(downloadUrl);
    const body2 = await res2.text();
    dim(`Same URL body: "${body2.trim()}"`);

    check("Same URL returns modified content (live, not snapshot)", body2 === modifiedContent);

    // Generate a new URL — should also work
    const downloadUrl2 = await sandbox.downloadUrl("/tmp/hello.txt");
    const res3 = await fetch(downloadUrl2);
    const body3 = await res3.text();
    check("New URL also returns modified content", body3 === modifiedContent);
    console.log();

    // ── Test 3: Signed upload URL ──────────────────────────────────
    bold("━━━ Test 3: Signed upload URL ━━━\n");

    const uploadUrl = await sandbox.uploadUrl("/tmp/uploaded.txt");
    dim(`Upload URL: ${uploadUrl}`);

    const uploadContent = "UPLOADED VIA SIGNED URL - no API key needed";
    const uploadRes = await fetch(uploadUrl, {
      method: "PUT",
      body: uploadContent,
    });
    dim(`Upload status: ${uploadRes.status}`);
    check("Upload returns 204", uploadRes.status === 204);

    // Verify via SDK read
    const readBack = await sandbox.files.read("/tmp/uploaded.txt");
    dim(`Readback: "${readBack}"`);
    check("Uploaded content readable via SDK", readBack === uploadContent);
    console.log();

    // ── Test 4: Upload overwrite via signed URL ────────────────────
    bold("━━━ Test 4: Upload overwrite ━━━\n");

    const overwriteContent = "OVERWRITTEN VIA SIGNED URL";
    const uploadUrl2 = await sandbox.uploadUrl("/tmp/uploaded.txt");
    const overwriteRes = await fetch(uploadUrl2, {
      method: "PUT",
      body: overwriteContent,
    });
    check("Overwrite upload returns 204", overwriteRes.status === 204);

    const readBack2 = await sandbox.files.read("/tmp/uploaded.txt");
    check("Overwritten content correct", readBack2 === overwriteContent);
    console.log();

    // ── Test 5: Download uploaded file via signed URL ───────────────
    bold("━━━ Test 5: Round-trip: upload then download via signed URLs ━━━\n");

    const roundtripContent = "round-trip test content 🎉";
    const upUrl = await sandbox.uploadUrl("/tmp/roundtrip.txt");
    await fetch(upUrl, { method: "PUT", body: roundtripContent });

    const dlUrl = await sandbox.downloadUrl("/tmp/roundtrip.txt");
    const dlRes = await fetch(dlUrl);
    const dlBody = await dlRes.text();
    check("Round-trip content intact", dlBody === roundtripContent);
    console.log();

    // ── Test 6: Expired URL ────────────────────────────────────────
    bold("━━━ Test 6: Expired URL ━━━\n");

    const shortUrl = await sandbox.downloadUrl("/tmp/hello.txt", { expiresIn: 1 });
    dim("Generated URL with 1s expiry, waiting 2s...");
    await new Promise((r) => setTimeout(r, 2000));

    const expiredRes = await fetch(shortUrl);
    dim(`Expired URL status: ${expiredRes.status}`);
    check("Expired URL returns 403", expiredRes.status === 403);

    if (expiredRes.status === 403) {
      const errBody = await expiredRes.json();
      dim(`Error: ${JSON.stringify(errBody)}`);
      check("Error message mentions expiry", errBody.error?.includes("expired"));
    }
    console.log();

    // ── Test 7: Tampered signature ─────────────────────────────────
    bold("━━━ Test 7: Tampered signature ━━━\n");

    const validUrl = await sandbox.downloadUrl("/tmp/hello.txt");
    const tampered = validUrl.replace(/signature=[^&]+/, "signature=deadbeef0000");
    const tamperedRes = await fetch(tampered);
    dim(`Tampered URL status: ${tamperedRes.status}`);
    check("Tampered signature returns 403", tamperedRes.status === 403);
    console.log();

    // ── Test 8: Tampered path ──────────────────────────────────────
    bold("━━━ Test 8: Tampered path ━━━\n");

    // Write a secret file, then try to access it by tampering the path param
    await sandbox.files.write("/tmp/secret.txt", "TOP SECRET");
    const legitUrl = await sandbox.downloadUrl("/tmp/hello.txt");
    const pathTampered = legitUrl.replace(
      /path=[^&]+/,
      `path=${encodeURIComponent("/tmp/secret.txt")}`,
    );
    const pathTamperedRes = await fetch(pathTampered);
    dim(`Path-tampered URL status: ${pathTamperedRes.status}`);
    check("Path-tampered URL returns 403 (signature mismatch)", pathTamperedRes.status === 403);
    console.log();

    // ── Test 9: Custom expiry ──────────────────────────────────────
    bold("━━━ Test 9: Custom expiry (5 min) ━━━\n");

    const customUrl = await sandbox.downloadUrl("/tmp/hello.txt", { expiresIn: 300 });
    const customUrlObj = new URL(customUrl);
    const expires = parseInt(customUrlObj.searchParams.get("expires") || "0");
    const now = Math.floor(Date.now() / 1000);
    const diff = expires - now;
    dim(`Expires in ${diff}s (expected ~300s)`);
    check("Expiry is approximately 5 minutes", diff >= 295 && diff <= 305, `${diff}s`);
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
