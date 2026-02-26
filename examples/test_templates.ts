/**
 * Test script for the OpenSandbox Templates API.
 *
 * Templates are standalone — they're not accessed through a Sandbox instance.
 * They let you build custom container images from Dockerfiles, which can then
 * be used as the `template` parameter when creating sandboxes.
 *
 * Usage:
 *     npx tsx test_templates.ts [API_URL] [API_KEY]
 *
 * Defaults to http://localhost:8080 with no API key.
 */

import { Sandbox } from "../sdks/typescript/src/index";
import { Templates, type TemplateInfo } from "../sdks/typescript/src/template";

function resolveApiUrl(url: string): string {
  const base = url.replace(/\/+$/, "");
  return base.endsWith("/api") ? base : `${base}/api`;
}

async function main() {
  const apiUrl = resolveApiUrl(
    process.argv[2] ?? process.env.OPENSANDBOX_API_URL ?? "http://localhost:8080"
  );
  const apiKey = process.argv[3] ?? process.env.OPENSANDBOX_API_KEY ?? "";
  const rawUrl = process.argv[2] ?? process.env.OPENSANDBOX_API_URL ?? "http://localhost:8080";

  const templates = new Templates(apiUrl, apiKey);

  // ── 1. List default templates ────────────────────────────────
  console.log("=== 1. List default templates ===");
  let allTemplates = await templates.list();
  console.log(`Found ${allTemplates.length} templates:`);
  for (const t of allTemplates) {
    console.log(`  - ${t.name} (id=${t.templateID}, tag=${t.tag}, status=${t.status})`);
  }

  // ── 2. Get a specific template ───────────────────────────────
  console.log("\n=== 2. Get 'base' template ===");
  const base = await templates.get("base");
  console.log(`  name=${base.name}, id=${base.templateID}, tag=${base.tag}, status=${base.status}`);

  // ── 3. Build a custom template ───────────────────────────────
  console.log("\n=== 3. Build custom template 'test-custom' ===");
  console.log("  (This runs `podman build` synchronously — may take a minute...)");
  const custom = await templates.build(
    "test-custom",
    `FROM ubuntu:22.04
RUN apt-get update -qq && apt-get install -y -qq curl > /dev/null 2>&1
RUN echo "custom template ready" > /etc/motd
`
  );
  console.log(`  Built! id=${custom.templateID}, name=${custom.name}, status=${custom.status}`);

  // ── 4. Verify it shows up in the list ────────────────────────
  console.log("\n=== 4. Verify template appears in list ===");
  allTemplates = await templates.list();
  const names = allTemplates.map((t) => t.name);
  if (!names.includes("test-custom")) {
    throw new Error(`Expected 'test-custom' in ${JSON.stringify(names)}`);
  }
  console.log(`  OK — found ${allTemplates.length} templates: ${JSON.stringify(names)}`);

  // ── 5. Get the custom template by name ───────────────────────
  console.log("\n=== 5. Get custom template ===");
  const fetched = await templates.get("test-custom");
  console.log(`  name=${fetched.name}, status=${fetched.status}`);

  // ── 6. Create a sandbox using the custom template ────────────
  console.log("\n=== 6. Create sandbox with custom template ===");
  const sandbox = await Sandbox.create({
    template: "test-custom",
    timeout: 60,
    apiUrl: rawUrl,
    apiKey,
  });
  console.log(`  Sandbox created: ${sandbox.sandboxId}`);

  // Verify the custom image works
  const motd = await sandbox.commands.run("cat /etc/motd");
  console.log(`  /etc/motd says: ${motd.stdout.trim()}`);
  if (!motd.stdout.includes("custom template ready")) {
    throw new Error(`Unexpected motd: ${motd.stdout}`);
  }

  const curl = await sandbox.commands.run("curl --version | head -1");
  console.log(`  curl: ${curl.stdout.trim()}`);

  await sandbox.kill();
  console.log("  Sandbox killed.");

  // ── 7. Delete the custom template ────────────────────────────
  console.log("\n=== 7. Delete custom template ===");
  await templates.delete("test-custom");
  console.log("  Deleted 'test-custom'.");

  // ── 8. Verify deletion ───────────────────────────────────────
  console.log("\n=== 8. Verify deletion ===");
  try {
    await templates.get("test-custom");
    console.log("  ERROR: template still exists after deletion!");
  } catch (e: any) {
    if (e.message.includes("404")) {
      console.log("  OK — template not found (404), as expected.");
    } else {
      console.log(`  Unexpected error: ${e.message}`);
    }
  }

  // Final list
  allTemplates = await templates.list();
  const remaining = allTemplates.map((t) => t.name);
  if (remaining.includes("test-custom")) {
    throw new Error(`'test-custom' still in ${JSON.stringify(remaining)}`);
  }
  console.log(`  Remaining templates: ${JSON.stringify(remaining)}`);

  console.log("\n✅ All template tests passed!");
}

main().catch((err) => {
  console.error(`\n❌ Test failed: ${err.message}`);
  process.exit(1);
});
