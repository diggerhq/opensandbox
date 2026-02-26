#!/usr/bin/env python3
"""
Python SDK Production Test

Validates that the Python template works end-to-end by running a Python
test script inside a sandbox that exercises stdlib, file ops, env vars, etc.

Usage:
  python examples/test_python_sdk.py
"""

import asyncio
import json
import sys

from opensandbox import Sandbox

GREEN = "\033[32m"
RED = "\033[31m"
BOLD = "\033[1m"
DIM = "\033[2m"
RESET = "\033[0m"

passed = 0
failed = 0


def green(msg: str) -> None:
    print(f"{GREEN}✓ {msg}{RESET}")


def red(msg: str) -> None:
    print(f"{RED}✗ {msg}{RESET}")


def bold(msg: str) -> None:
    print(f"{BOLD}{msg}{RESET}")


def dim(msg: str) -> None:
    print(f"{DIM}  {msg}{RESET}")


def check(desc: str, condition: bool, detail: str = "") -> None:
    global passed, failed
    if condition:
        green(desc)
        passed += 1
    else:
        red(f"{desc} ({detail})" if detail else desc)
        failed += 1


PYTHON_TEST_SCRIPT = '''
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
'''


async def main() -> None:
    global passed, failed

    bold("\n╔══════════════════════════════════════════════════╗")
    bold("║       Python SDK Production Test                 ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    sandbox = None

    try:
        # --- Create sandbox with Python template ---
        bold("[1/4] Creating Python sandbox...")
        sandbox = await Sandbox.create(template="python", timeout=120)
        green(f"Created: {sandbox.sandbox_id}")
        dim(f"Domain: {sandbox.domain}")
        print()

        # --- Write and run Python test script ---
        bold("[2/4] Writing Python test script...")
        await sandbox.files.write("/tmp/test_sdk.py", PYTHON_TEST_SCRIPT)
        green("Script written to /tmp/test_sdk.py")
        print()

        bold("[3/4] Running Python tests inside sandbox...")
        result = await sandbox.commands.run("python3 /tmp/test_sdk.py", timeout=30)
        check("Python script exited cleanly", result.exit_code == 0,
              f"exit code: {result.exit_code}")

        if result.exit_code != 0:
            dim(f"stderr: {result.stderr}")
            dim(f"stdout: {result.stdout}")
        else:
            data = json.loads(result.stdout.strip())
            check("Echo command works", data["echo"] == "hello-from-python", data["echo"])
            check("File write/read works", data["file_content"] == "python-sdk-data", data["file_content"])
            check("HOME env var present", data["home"] != "unknown", data["home"])
            check("PATH env var exists", data["path_exists"] is True)
            check("Nested directory file works", data["nested"] == "nested-content", data["nested"])
            check("Python version detected", data["python_version"].startswith("3."), data["python_version"])
            check("Platform is Linux", data["platform"] == "linux", data["platform"])
            check("Math.pi correct", data["pi"] == "3.14159", data["pi"])
            check("JSON roundtrip works", data["json_roundtrip"] is True)
            dim(f"Python {data['python_version']} on {data['platform']}")
        print()

        # --- Verify file ops from SDK side ---
        bold("[4/4] Verifying files from Python SDK...")
        content = await sandbox.files.read("/tmp/py-test.txt")
        check("SDK can read Python-written file", content == "python-sdk-data", content)

        entries = await sandbox.files.list("/tmp/py-nested/deep/dir")
        check("SDK can list Python-created directory",
              any(e.name == "file.txt" for e in entries))
        print()

    except Exception as e:
        red(f"Fatal error: {e}")
        failed += 1
    finally:
        if sandbox:
            await sandbox.kill()
            green("Sandbox killed")

    # --- Summary ---
    bold("========================================")
    bold(f" Results: {passed} passed, {failed} failed")
    bold("========================================\n")
    if failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
