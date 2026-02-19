/**
 * OpenSandbox Production Test Suite Runner
 *
 * Runs all production tests in sequence, with a summary at the end.
 * Tests are ordered from fast/simple to slow/complex.
 *
 * Usage:
 *   npx tsx examples/run-all-tests.ts              # Run all tests
 *   npx tsx examples/run-all-tests.ts --skip-slow  # Skip timeout test (waits ~2min)
 *
 * Individual tests:
 *   npx tsx examples/test-python-sdk.ts
 *   npx tsx examples/test-multi-template.ts
 *   npx tsx examples/test-commands.ts
 *   npx tsx examples/test-file-ops.ts
 *   npx tsx examples/test-reconnect.ts
 *   npx tsx examples/test-domain-tls.ts
 *   npx tsx examples/test-concurrent.ts
 *   npx tsx examples/test-hibernation-stress.ts
 *   npx tsx examples/test-timeout.ts
 *   npx tsx examples/webhook-demo.ts
 */

import { execSync } from "child_process";
import * as path from "path";
import { fileURLToPath } from "url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const BOLD = "\x1b[1m";
const GREEN = "\x1b[32m";
const RED = "\x1b[31m";
const DIM = "\x1b[2m";
const RESET = "\x1b[0m";
const CYAN = "\x1b[36m";

const skipSlow = process.argv.includes("--skip-slow");

interface TestSuite {
  name: string;
  file: string;
  slow?: boolean;
  description: string;
}

const SUITES: TestSuite[] = [
  { name: "Commands", file: "test-commands.ts", description: "Shell commands, stderr, exit codes, pipes, concurrency" },
  { name: "File Ops", file: "test-file-ops.ts", description: "Large files, special chars, nested dirs, deletion" },
  { name: "Python SDK", file: "test-python-sdk.ts", description: "Python template, stdlib, file ops from Python" },
  { name: "Multi-Template", file: "test-multi-template.ts", description: "base, python, node templates" },
  { name: "Reconnect", file: "test-reconnect.ts", description: "Sandbox.connect(), state persistence, multi-conn" },
  { name: "Domain/TLS", file: "test-domain-tls.ts", description: "Subdomains, Let's Encrypt certs, routing isolation" },
  { name: "Concurrent", file: "test-concurrent.ts", description: "5 sandboxes in parallel, isolation, parallel ops" },
  { name: "Hibernation", file: "test-hibernation-stress.ts", description: "Multi-cycle hibernate/wake, large state, auto-wake" },
  { name: "Timeout", file: "test-timeout.ts", slow: true, description: "30s timeout, setTimeout(), rolling timeout (takes ~2min)" },
];

async function main() {
  console.log(`${BOLD}\n╔════════════════════════════════════════════════════════╗`);
  console.log(`║       OpenSandbox Production Test Suite                ║`);
  console.log(`╚════════════════════════════════════════════════════════╝${RESET}\n`);

  const filteredSuites = skipSlow ? SUITES.filter(s => !s.slow) : SUITES;

  console.log(`${DIM}Running ${filteredSuites.length} test suites${skipSlow ? " (slow tests skipped)" : ""}${RESET}`);
  console.log(`${DIM}${"─".repeat(60)}${RESET}\n`);

  const results: { name: string; passed: boolean; durationMs: number; error?: string }[] = [];
  const totalStart = Date.now();

  for (let i = 0; i < filteredSuites.length; i++) {
    const suite = filteredSuites[i];
    const filePath = path.join(__dirname, suite.file);

    console.log(`${BOLD}[${i + 1}/${filteredSuites.length}] ${suite.name}${RESET}`);
    console.log(`${DIM}    ${suite.description}${RESET}`);
    console.log(`${DIM}    Running: npx tsx ${suite.file}${RESET}\n`);

    const start = Date.now();
    try {
      execSync(`npx tsx "${filePath}"`, {
        stdio: "inherit",
        env: { ...process.env },
        timeout: 300000, // 5 min max per suite
      });
      const durationMs = Date.now() - start;
      results.push({ name: suite.name, passed: true, durationMs });
      console.log(`${GREEN}── ${suite.name}: PASSED (${(durationMs / 1000).toFixed(1)}s) ──${RESET}\n`);
    } catch (err: any) {
      const durationMs = Date.now() - start;
      results.push({ name: suite.name, passed: false, durationMs, error: err.message });
      console.log(`${RED}── ${suite.name}: FAILED (${(durationMs / 1000).toFixed(1)}s) ──${RESET}\n`);
    }
  }

  const totalMs = Date.now() - totalStart;

  // ── Summary ─────────────────────────────────────────────────────────
  console.log(`\n${BOLD}╔════════════════════════════════════════════════════════╗`);
  console.log(`║                    Test Results                        ║`);
  console.log(`╠════════════════════════════════════════════════════════╣${RESET}`);

  const maxNameLen = Math.max(...results.map(r => r.name.length));
  for (const r of results) {
    const icon = r.passed ? `${GREEN}✓${RESET}` : `${RED}✗${RESET}`;
    const name = r.name.padEnd(maxNameLen);
    const duration = `${(r.durationMs / 1000).toFixed(1)}s`.padStart(7);
    console.log(`  ${icon} ${name}  ${duration}`);
  }

  const passedCount = results.filter(r => r.passed).length;
  const failedCount = results.filter(r => !r.passed).length;

  console.log(`${BOLD}\n╠════════════════════════════════════════════════════════╣`);
  console.log(`║  ${passedCount} passed, ${failedCount} failed | Total: ${(totalMs / 1000).toFixed(1)}s${" ".repeat(Math.max(0, 32 - String(passedCount).length - String(failedCount).length - String((totalMs / 1000).toFixed(1)).length))}║`);
  console.log(`╚════════════════════════════════════════════════════════╝${RESET}\n`);

  if (failedCount > 0) {
    console.log(`${RED}Failed suites:${RESET}`);
    for (const r of results.filter(r => !r.passed)) {
      console.log(`  ${RED}✗ ${r.name}${RESET}`);
    }
    console.log();
    process.exit(1);
  }
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
