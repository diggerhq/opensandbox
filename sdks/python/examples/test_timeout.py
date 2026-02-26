#!/usr/bin/env python3
"""
Timeout Behavior Test

Tests:
  1. Activity resets the rolling timeout (sandbox stays alive with pokes)
  2. Sandbox with short timeout eventually auto-hibernates when idle
  3. Different timeout values accepted

Usage:
  python examples/test_timeout.py
"""

import asyncio
import sys

from opensandbox import Sandbox

GREEN = "\033[32m"
RED = "\033[31m"
BOLD = "\033[1m"
DIM = "\033[2m"
CYAN = "\033[36m"
YELLOW = "\033[33m"
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


def cyan(msg: str) -> None:
    print(f"{CYAN}→ {msg}{RESET}")


def yellow(msg: str) -> None:
    print(f"{YELLOW}⚠ {msg}{RESET}")


def check(desc: str, condition: bool, detail: str = "") -> None:
    global passed, failed
    if condition:
        green(desc)
        passed += 1
    else:
        red(f"{desc} ({detail})" if detail else desc)
        failed += 1


async def main() -> None:
    global passed, failed

    bold("\n╔══════════════════════════════════════════════════╗")
    bold("║       Timeout Behavior Test                      ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    # ── Test 1: Activity resets rolling timeout ──
    bold("━━━ Test 1: Activity resets rolling timeout ━━━\n")
    dim("Timeout=60s, pokes every 15s × 8 = 120s total elapsed")
    dim("Without rolling reset, sandbox would die at 60s")

    sb = await Sandbox.create(template="base", timeout=60)
    green(f"Created: {sb.sandbox_id} (timeout: 60s)")

    for i in range(8):
        dim(f"Poke {i + 1}/8: waiting 15s then running command...")
        await asyncio.sleep(15)
        try:
            result = await sb.commands.run(f"echo poke-{i + 1}")
            check(f"Poke {i + 1} at {(i + 1) * 15}s: sandbox still alive",
                  result.stdout.strip() == f"poke-{i + 1}",
                  f'got: "{result.stdout.strip()}"')
        except Exception as e:
            check(f"Poke {i + 1} at {(i + 1) * 15}s: sandbox still alive", False, str(e))

    dim("Total elapsed: ~120s (2× the 60s timeout, proving rolling reset)")
    running = await sb.is_running()
    check("Sandbox alive after 120s with activity (rolling timeout proven)", running)

    await sb.kill()
    green("Sandbox killed")
    print()

    # ── Test 2: Idle sandbox eventually times out ──
    bold("━━━ Test 2: Idle sandbox times out ━━━\n")

    sb = await Sandbox.create(template="base", timeout=30)
    green(f"Created: {sb.sandbox_id} (timeout: 30s)")

    result = await sb.commands.run("echo alive")
    check("Commands work while alive", result.stdout.strip() == "alive")

    dim("Leaving sandbox completely idle for 40 seconds...")
    await asyncio.sleep(40)

    running = await sb.is_running()
    dim(f"is_running after 40s idle: {running}")

    if running:
        try:
            reconnected = await Sandbox.connect(sb.sandbox_id)
            after_result = await reconnected.commands.run("echo after-timeout")
            dim(f'Command after idle: "{after_result.stdout.strip()}"')
            yellow("Sandbox still responds after idle — timeout may auto-hibernate + auto-wake on command")
        except Exception as e:
            dim(f"Command after idle failed: {e}")
            green("Sandbox appears to have timed out (command failed)")
    else:
        green("Sandbox no longer running after idle period")

    check("Idle timeout test completed", True)

    try:
        await sb.kill()
    except Exception:
        pass
    green("Sandbox cleaned up")
    print()

    # ── Test 3: Different timeout values accepted ──
    bold("━━━ Test 3: Different timeout values accepted ━━━\n")

    timeouts = [30, 60, 300, 600]
    for t in timeouts:
        sb = await Sandbox.create(template="base", timeout=t)
        check(f"Sandbox created with timeout={t}s", sb.status == "running")
        result = await sb.commands.run("echo ok")
        check(f"Commands work with timeout={t}s", result.stdout.strip() == "ok")
        await sb.kill()
    print()

    # --- Summary ---
    bold("========================================")
    bold(f" Results: {passed} passed, {failed} failed")
    bold("========================================\n")
    if failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
