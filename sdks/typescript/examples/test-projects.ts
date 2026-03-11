/**
 * Projects & Secrets Test
 *
 * Tests:
 *   1. Create a project
 *   2. List projects
 *   3. Get project by ID
 *   4. Update project
 *   5. Set secrets on a project
 *   6. List secrets (names only, values never returned)
 *   7. Create sandbox with project (inherits config + secrets)
 *   8. Verify secrets are injected as env vars
 *   9. Delete secret
 *  10. Delete project
 *
 * Usage:
 *   npx tsx examples/test-projects.ts
 */

import { Sandbox, Project } from "../src/index";

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
  bold("║       Projects & Secrets Test                    ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let projectId: string | null = null;
  let sandbox: Sandbox | null = null;
  const projectName = `test-project-${Date.now()}`;

  try {
    // ── Test 1: Create project ────────────────────────────────────
    bold("━━━ Test 1: Create project ━━━\n");

    const project = await Project.create({
      name: projectName,
      template: "base",
      cpuCount: 1,
      memoryMB: 512,
      timeoutSec: 120,
    });

    projectId = project.id;
    check("Project created", !!project.id);
    check("Name matches", project.name === projectName);
    check("Template set", project.template === "base");
    check("CPU set", project.cpuCount === 1);
    check("Memory set", project.memoryMB === 512);
    check("Timeout set", project.timeoutSec === 120);
    dim(`Project ID: ${project.id}`);
    console.log();

    // ── Test 2: List projects ─────────────────────────────────────
    bold("━━━ Test 2: List projects ━━━\n");

    const projects = await Project.list();
    check("List returns array", Array.isArray(projects));
    const found = projects.find((p) => p.id === projectId);
    check("Created project in list", !!found);
    dim(`Total projects: ${projects.length}`);
    console.log();

    // ── Test 3: Get project ───────────────────────────────────────
    bold("━━━ Test 3: Get project by ID ━━━\n");

    const fetched = await Project.get(projectId!);
    check("Get returns correct project", fetched.id === projectId);
    check("Get has correct name", fetched.name === projectName);
    console.log();

    // ── Test 4: Update project ────────────────────────────────────
    bold("━━━ Test 4: Update project ━━━\n");

    const updated = await Project.update(projectId!, {
      memoryMB: 1024,
      timeoutSec: 300,
    });
    check("Memory updated to 1024", updated.memoryMB === 1024);
    check("Timeout updated to 300", updated.timeoutSec === 300);
    check("Name unchanged", updated.name === projectName);
    console.log();

    // ── Test 5: Set secrets ───────────────────────────────────────
    bold("━━━ Test 5: Set project secrets ━━━\n");

    await Project.setSecret(projectId!, "TEST_API_KEY", "sk-test-12345");
    green("Set TEST_API_KEY");

    await Project.setSecret(projectId!, "DATABASE_URL", "postgres://localhost/test");
    green("Set DATABASE_URL");

    await Project.setSecret(projectId!, "TEMP_SECRET", "will-be-deleted");
    green("Set TEMP_SECRET");
    console.log();

    // ── Test 6: List secrets ──────────────────────────────────────
    bold("━━━ Test 6: List secret names ━━━\n");

    const secretNames = await Project.listSecrets(projectId!);
    check("Returns array", Array.isArray(secretNames));
    check("Has TEST_API_KEY", secretNames.includes("TEST_API_KEY"));
    check("Has DATABASE_URL", secretNames.includes("DATABASE_URL"));
    check("Has TEMP_SECRET", secretNames.includes("TEMP_SECRET"));
    check("3 secrets total", secretNames.length === 3, `got ${secretNames.length}`);
    dim(`Secret names: ${secretNames.join(", ")}`);
    console.log();

    // ── Test 7: Create sandbox with project ───────────────────────
    bold("━━━ Test 7: Create sandbox with project ━━━\n");

    sandbox = await Sandbox.create({
      project: projectName,
      timeout: 120,
    });
    check("Sandbox created", !!sandbox.sandboxId);
    dim(`Sandbox ID: ${sandbox.sandboxId}`);
    console.log();

    // ── Test 8: Verify secrets are sealed in sandbox ──────────────
    bold("━━━ Test 8: Verify secrets sealed in sandbox ━━━\n");

    // Secrets should be sealed tokens (osb_sealed_*) inside the VM.
    // The MITM proxy replaces sealed tokens with real values on outbound HTTPS requests,
    // so the real secret never exists in VM memory.
    const apiKeyResult = await sandbox.commands.run("echo $TEST_API_KEY");
    const apiKeyVal = apiKeyResult.stdout.trim();
    check("TEST_API_KEY is sealed", apiKeyVal.startsWith("osb_sealed_"),
      `got "${apiKeyVal}"`);

    const dbUrlResult = await sandbox.commands.run("echo $DATABASE_URL");
    const dbUrlVal = dbUrlResult.stdout.trim();
    check("DATABASE_URL is sealed", dbUrlVal.startsWith("osb_sealed_"),
      `got "${dbUrlVal}"`);

    const tempResult = await sandbox.commands.run("echo $TEMP_SECRET");
    const tempVal = tempResult.stdout.trim();
    check("TEMP_SECRET is sealed", tempVal.startsWith("osb_sealed_"),
      `got "${tempVal}"`);
    console.log();

    // ── Test 9: Delete secret ─────────────────────────────────────
    bold("━━━ Test 9: Delete secret ━━━\n");

    await Project.deleteSecret(projectId!, "TEMP_SECRET");
    green("Deleted TEMP_SECRET");

    const afterDelete = await Project.listSecrets(projectId!);
    check("TEMP_SECRET removed", !afterDelete.includes("TEMP_SECRET"));
    check("2 secrets remaining", afterDelete.length === 2, `got ${afterDelete.length}`);
    console.log();

    // ── Test 10: Delete project ───────────────────────────────────
    bold("━━━ Test 10: Delete project ━━━\n");

    // Kill sandbox first
    await sandbox.kill();
    green("Sandbox killed");
    sandbox = null;

    await Project.delete(projectId!);
    green("Project deleted");

    // Verify it's gone
    try {
      await Project.get(projectId!);
      red("Project should not exist after delete");
      failed++;
    } catch {
      green("Project not found after delete (expected)");
      passed++;
    }
    projectId = null;
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
    if (err.stack) dim(err.stack);
    failed++;
  } finally {
    // Cleanup
    if (sandbox) {
      try { await sandbox.kill(); } catch {}
    }
    if (projectId) {
      try { await Project.delete(projectId); } catch {}
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
