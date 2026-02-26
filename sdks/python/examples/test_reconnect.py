#!/usr/bin/env python3
"""
SDK Connect/Reconnect Test

Tests:
  1. Create sandbox, disconnect, reconnect via Sandbox.connect()
  2. Verify state persists across connections
  3. Multiple connect() calls to same sandbox
  4. Operations work on reconnected instance

Usage:
  python examples/test_reconnect.py
"""

import asyncio
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


async def main() -> None:
    global passed, failed

    bold("\n╔══════════════════════════════════════════════════╗")
    bold("║       SDK Connect/Reconnect Test                 ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    sandbox_id = ""

    try:
        # ── Test 1: Create and store ID ──
        bold("━━━ Test 1: Create sandbox and store ID ━━━\n")

        original = await Sandbox.create(template="base", timeout=120)
        sandbox_id = original.sandbox_id
        green(f"Created: {sandbox_id}")
        dim(f"Domain: {original.domain}")

        await original.files.write("/tmp/reconnect-test.txt", "original-data")
        await original.commands.run("echo 'state-marker' > /tmp/marker.txt")
        green("State written to sandbox")
        print()

        # ── Test 2: Reconnect via Sandbox.connect() ──
        bold("━━━ Test 2: Reconnect via Sandbox.connect() ━━━\n")

        reconnected = await Sandbox.connect(sandbox_id)
        check("Reconnect succeeded", reconnected is not None)
        check("Same sandbox ID", reconnected.sandbox_id == sandbox_id)
        check("Domain matches", reconnected.domain == original.domain, reconnected.domain)
        check("Status is running", reconnected.status == "running", reconnected.status)
        print()

        # ── Test 3: Read state from reconnected instance ──
        bold("━━━ Test 3: State persists across connections ━━━\n")

        file_content = await reconnected.files.read("/tmp/reconnect-test.txt")
        check("File from original instance readable", file_content == "original-data", file_content)

        marker_content = await reconnected.files.read("/tmp/marker.txt")
        check("Command output file persisted", marker_content.strip() == "state-marker")

        await reconnected.files.write("/tmp/reconnect-new.txt", "from-reconnected")
        new_content = await reconnected.files.read("/tmp/reconnect-new.txt")
        check("Write from reconnected instance works", new_content == "from-reconnected")
        print()

        # ── Test 4: Commands work on reconnected instance ──
        bold("━━━ Test 4: Commands work on reconnected instance ━━━\n")

        echo = await reconnected.commands.run("echo reconnected-echo")
        check("Echo command works", echo.stdout.strip() == "reconnected-echo")
        check("Exit code is 0", echo.exit_code == 0)

        uname = await reconnected.commands.run("uname -s")
        check("Uname returns Linux", uname.stdout.strip() == "Linux")

        env_result = await reconnected.commands.run("echo $TEST_VAR",
                                                     env={"TEST_VAR": "reconnected-env"})
        check("Env vars work on reconnected instance",
              env_result.stdout.strip() == "reconnected-env")

        entries = await reconnected.files.list("/tmp")
        check("File listing works",
              any(e.name == "reconnect-test.txt" for e in entries))
        check("New file visible in listing",
              any(e.name == "reconnect-new.txt" for e in entries))
        print()

        # ── Test 5: Multiple simultaneous connections ──
        bold("━━━ Test 5: Multiple simultaneous connections ━━━\n")

        conn1 = await Sandbox.connect(sandbox_id)
        conn2 = await Sandbox.connect(sandbox_id)
        conn3 = await Sandbox.connect(sandbox_id)

        check("3 connections to same sandbox succeeded",
              conn1 is not None and conn2 is not None and conn3 is not None)

        read1, read2, read3 = await asyncio.gather(
            conn1.files.read("/tmp/reconnect-test.txt"),
            conn2.files.read("/tmp/reconnect-test.txt"),
            conn3.files.read("/tmp/reconnect-test.txt"),
        )

        check("All connections read same data",
              read1 == "original-data" and read2 == "original-data" and read3 == "original-data")

        await conn1.files.write("/tmp/cross-conn.txt", "from-conn1")
        cross_read = await conn2.files.read("/tmp/cross-conn.txt")
        check("Write from conn1 visible to conn2", cross_read == "from-conn1")
        print()

        # ── Test 6: is_running on reconnected instance ──
        bold("━━━ Test 6: is_running on reconnected instance ━━━\n")

        running = await reconnected.is_running()
        check("is_running returns True", running)
        print()

        # ── Test 7: Kill from reconnected instance ──
        bold("━━━ Test 7: Kill from reconnected instance ━━━\n")

        await reconnected.kill()
        green("Killed from reconnected instance")

        await asyncio.sleep(0.5)

        still_running = await original.is_running()
        check("Original ref sees sandbox as stopped", not still_running)

        try:
            dead_conn = await Sandbox.connect(sandbox_id)
            dead_running = await dead_conn.is_running()
            check("Connect to killed sandbox: is_running=False", not dead_running)
        except Exception as e:
            check("Connect to killed sandbox: throws or returns not running", True)
            dim(f"Error: {e}")
        print()

        # Clear sandbox_id so cleanup doesn't try to kill again
        sandbox_id = ""

    except Exception as e:
        red(f"Fatal error: {e}")
        failed += 1
    finally:
        if sandbox_id:
            try:
                cleanup = await Sandbox.connect(sandbox_id)
                await cleanup.kill()
                green("Sandbox killed in cleanup")
            except Exception:
                pass

    # --- Summary ---
    bold("========================================")
    bold(f" Results: {passed} passed, {failed} failed")
    bold("========================================\n")
    if failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
