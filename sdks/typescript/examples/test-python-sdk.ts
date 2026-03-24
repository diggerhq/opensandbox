/**
 * Python SDK Production Test
 *
 * Validates the Python SDK works end-to-end against production by running
 * the Python test script inside a sandbox (meta: using TS SDK to test Python SDK).
 *
 * Usage:
 *   npx tsx examples/test-python-sdk.ts
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

// Python test script that exercises all SDK operations and prints JSON results
const PYTHON_TEST_SCRIPT = `
import json
import os

results = {}

# Test 1: Basic echo
import subprocess
r = subprocess.run(["echo", "hello-from-python"], capture_output=True, text=True)
results["echo"] = r.stdout.strip()

# Test 2: File write + read
with open("/tmp/py-test.txt", "w") as f:
    f.write("python-sdk-data")
with open("/tmp/py-test.txt", "r") as f:
    results["file_content"] = f.read()

# Test 3: Environment variables
results["home"] = os.environ.get("HOME", "unknown")
results["path_exists"] = "PATH" in os.environ

# Test 4: Nested directory
os.makedirs("/tmp/py-nested/deep/dir", exist_ok=True)
with open("/tmp/py-nested/deep/dir/file.txt", "w") as f:
    f.write("nested-content")
with open("/tmp/py-nested/deep/dir/file.txt", "r") as f:
    results["nested"] = f.read()

# Test 5: Python-specific features
import sys
results["python_version"] = sys.version.split()[0]
results["platform"] = sys.platform

# Test 6: Math/stdlib
import math
results["pi"] = str(round(math.pi, 5))

# Test 7: JSON handling
data = {"key": "value", "number": 42, "nested": {"a": True}}
results["json_roundtrip"] = json.loads(json.dumps(data)) == data

print(json.dumps(results))
`;

async function main() {
  bold("\n╔══════════════════════════════════════════════════╗");
  bold("║       Python SDK Production Test                 ║");
  bold("╚══════════════════════════════════════════════════╝\n");

  let sandbox: Sandbox | null = null;

  try {
    // --- Create sandbox with Python template ---
    bold("[1/4] Creating Python sandbox...");
    sandbox = await Sandbox.create({
      template: "base",
      timeout: 120,
    });
    green(`Created: ${sandbox.sandboxId}`);
    dim(`Domain: ${sandbox.domain}`);
    console.log();

    // --- Write and run Python test script ---
    bold("[2/4] Writing Python test script...");
    await sandbox.files.write("/tmp/test_sdk.py", PYTHON_TEST_SCRIPT);
    green("Script written to /tmp/test_sdk.py");
    console.log();

    bold("[3/4] Running Python tests inside sandbox...");
    const result = await sandbox.commands.run("python3 /tmp/test_sdk.py", { timeout: 30 });
    check("Python script exited cleanly", result.exitCode === 0, `exit code: ${result.exitCode}`);

    if (result.exitCode !== 0) {
      dim(`stderr: ${result.stderr}`);
      dim(`stdout: ${result.stdout}`);
    } else {
      const data = JSON.parse(result.stdout.trim());
      check("Echo command works", data.echo === "hello-from-python", data.echo);
      check("File write/read works", data.file_content === "python-sdk-data", data.file_content);
      check("HOME env var present", data.home !== "unknown", data.home);
      check("PATH env var exists", data.path_exists === true);
      check("Nested directory file works", data.nested === "nested-content", data.nested);
      check("Python version detected", data.python_version.startsWith("3."), data.python_version);
      check("Platform is Linux", data.platform === "linux", data.platform);
      check("Math.pi correct", data.pi === "3.14159", data.pi);
      check("JSON roundtrip works", data.json_roundtrip === true);
      dim(`Python ${data.python_version} on ${data.platform}`);
    }
    console.log();

    // --- Verify file ops from SDK side ---
    bold("[4/4] Verifying files from TypeScript SDK...");
    const content = await sandbox.files.read("/tmp/py-test.txt");
    check("TS SDK can read Python-written file", content === "python-sdk-data", content);

    const entries = await sandbox.files.list("/tmp/py-nested/deep/dir");
    check("TS SDK can list Python-created directory", entries.some(e => e.name === "file.txt"));
    console.log();

  } catch (err: any) {
    red(`Fatal error: ${err.message}`);
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
