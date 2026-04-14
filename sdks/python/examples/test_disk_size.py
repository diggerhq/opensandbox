#!/usr/bin/env python3
"""
Disk Size Test

Verifies the `disk_mb` sandbox creation parameter:
  1. Default (no arg) → 20GB workspace
  2. Explicit 20GB    → 20GB workspace
  3. Explicit 30GB    → 30GB workspace (requires org `max_disk_mb` >= 30720)
  4. Below minimum    → rejected with 400

Larger disk sizes are in closed beta — contact us to raise your org's
max_disk_mb ceiling: https://cal.com/team/digger/opencomputer-founder-chat

Usage:
  python examples/test_disk_size.py

Environment:
  OPENCOMPUTER_API_URL  (default: http://localhost:8080)
  OPENCOMPUTER_API_KEY  (default: test-key)
"""

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from opencomputer import Sandbox

API_URL = os.environ.get("OPENCOMPUTER_API_URL", "http://localhost:8080")
API_KEY = os.environ.get("OPENCOMPUTER_API_KEY", "test-key")

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


async def run_case(label: str, disk_mb: int | None, expect_size: str | None, expect_reject: bool = False) -> None:
    global passed, failed
    bold(f"\n{label}")
    kwargs = dict(api_url=API_URL, api_key=API_KEY, timeout=120)
    if disk_mb is not None:
        kwargs["disk_mb"] = disk_mb
    try:
        async with await Sandbox.create(**kwargs) as sb:
            dim(f"created {sb.sandbox_id}")
            r = await sb.exec.run("df -h /home/sandbox | tail -1")
            dim(f"df: {r.stdout.strip()}")
            if expect_reject:
                red("expected rejection but sandbox was created")
                failed += 1
            elif expect_size and expect_size in r.stdout:
                green(f"workspace reports {expect_size}")
                passed += 1
            else:
                red(f"expected {expect_size} in df output")
                failed += 1
    except Exception as e:
        if expect_reject:
            green(f"rejected as expected ({str(e).splitlines()[0]})")
            passed += 1
        else:
            red(f"unexpected error: {e}")
            failed += 1


async def main() -> None:
    bold("OpenComputer SDK — disk size test")
    dim(f"API: {API_URL}")

    await run_case("default (no disk_mb)", None, "20G")
    await run_case("explicit 20GB", 20480, "20G")
    await run_case("30GB", 30720, "30G")
    await run_case("below minimum (8GB)", 8192, None, expect_reject=True)

    print()
    bold(f"Results: {passed} passed, {failed} failed")
    sys.exit(1 if failed > 0 else 0)


if __name__ == "__main__":
    asyncio.run(main())
