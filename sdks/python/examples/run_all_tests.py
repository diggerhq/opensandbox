#!/usr/bin/env python3
"""
OpenSandbox Production Test Suite Runner (Python)

Runs all production tests in sequence, with a summary at the end.
Tests are ordered from fast/simple to slow/complex.

Usage:
  python examples/run_all_tests.py              # Run all tests
  python examples/run_all_tests.py --skip-slow  # Skip timeout test (waits ~2min)

Individual tests:
  python examples/test_environment.py
  python examples/test_commands.py
  python examples/test_file_ops.py
  python examples/test_python_sdk.py
  python examples/test_multi_template.py
  python examples/test_reconnect.py
  python examples/test_domain_tls.py
  python examples/test_concurrent.py
  python examples/test_timeout.py
"""

import os
import subprocess
import sys
import time

BOLD = "\033[1m"
GREEN = "\033[32m"
RED = "\033[31m"
DIM = "\033[2m"
CYAN = "\033[36m"
RESET = "\033[0m"

SUITES = [
    {
        "name": "Environment",
        "file": "test_environment.py",
        "description": "HOME=/workspace, npm/pip cache dirs, dotfiles",
    },
    {
        "name": "Commands",
        "file": "test_commands.py",
        "description": "Shell commands, stderr, exit codes, pipes, concurrency",
    },
    {
        "name": "File Ops",
        "file": "test_file_ops.py",
        "description": "Large files, special chars, nested dirs, deletion",
    },
    {
        "name": "Python SDK",
        "file": "test_python_sdk.py",
        "description": "Python template, stdlib, file ops from Python",
    },
    {
        "name": "Multi-Template",
        "file": "test_multi_template.py",
        "description": "base, python, node templates",
    },
    {
        "name": "Reconnect",
        "file": "test_reconnect.py",
        "description": "Sandbox.connect(), state persistence, multi-conn",
    },
    {
        "name": "Domain/TLS",
        "file": "test_domain_tls.py",
        "description": "Subdomains, HTTPS requests, routing isolation",
    },
    {
        "name": "Concurrent",
        "file": "test_concurrent.py",
        "description": "5 sandboxes in parallel, isolation, parallel ops",
    },
    {
        "name": "Timeout",
        "file": "test_timeout.py",
        "slow": True,
        "description": "30s timeout, rolling timeout (takes ~2min)",
    },
]


def main() -> None:
    print(f"{BOLD}")
    print("╔════════════════════════════════════════════════════════╗")
    print("║       OpenSandbox Production Test Suite (Python)      ║")
    print(f"╚════════════════════════════════════════════════════════╝{RESET}\n")

    skip_slow = "--skip-slow" in sys.argv
    filtered = [s for s in SUITES if not (skip_slow and s.get("slow"))]

    print(f'{DIM}Running {len(filtered)} test suites{"(slow tests skipped)" if skip_slow else ""}{RESET}')
    print(f"{DIM}{'─' * 60}{RESET}\n")

    results: list[dict] = []
    total_start = time.time()
    script_dir = os.path.dirname(os.path.abspath(__file__))

    for i, suite in enumerate(filtered):
        file_path = os.path.join(script_dir, suite["file"])

        print(f"{BOLD}[{i + 1}/{len(filtered)}] {suite['name']}{RESET}")
        print(f"{DIM}    {suite['description']}{RESET}")
        print(f"{DIM}    Running: python {suite['file']}{RESET}\n")

        start = time.time()
        try:
            subprocess.run(
                [sys.executable, file_path],
                check=True,
                env={**os.environ},
                timeout=300,  # 5 min max per suite
            )
            duration_ms = (time.time() - start) * 1000
            results.append({"name": suite["name"], "passed": True, "duration_ms": duration_ms})
            print(f"{GREEN}── {suite['name']}: PASSED ({duration_ms / 1000:.1f}s) ──{RESET}\n")
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as e:
            duration_ms = (time.time() - start) * 1000
            results.append({"name": suite["name"], "passed": False, "duration_ms": duration_ms, "error": str(e)})
            print(f"{RED}── {suite['name']}: FAILED ({duration_ms / 1000:.1f}s) ──{RESET}\n")

    total_ms = (time.time() - total_start) * 1000

    # ── Summary ──
    print(f"\n{BOLD}╔════════════════════════════════════════════════════════╗")
    print("║                    Test Results                        ║")
    print(f"╠════════════════════════════════════════════════════════╣{RESET}")

    max_name = max(len(r["name"]) for r in results) if results else 0
    for r in results:
        icon = f"{GREEN}✓{RESET}" if r["passed"] else f"{RED}✗{RESET}"
        name = r["name"].ljust(max_name)
        duration = f"{r['duration_ms'] / 1000:.1f}s".rjust(7)
        print(f"  {icon} {name}  {duration}")

    passed_count = sum(1 for r in results if r["passed"])
    failed_count = sum(1 for r in results if not r["passed"])

    print(f"\n{BOLD}╠════════════════════════════════════════════════════════╣")
    print(f"║  {passed_count} passed, {failed_count} failed | Total: {total_ms / 1000:.1f}s")
    print(f"╚════════════════════════════════════════════════════════╝{RESET}\n")

    if failed_count > 0:
        print(f"{RED}Failed suites:{RESET}")
        for r in results:
            if not r["passed"]:
                print(f"  {RED}✗ {r['name']}{RESET}")
        print()
        sys.exit(1)


if __name__ == "__main__":
    main()
