#!/usr/bin/env python3
"""
Concurrent Sandbox Test

Tests:
  1. Create 5 sandboxes simultaneously
  2. Run commands on all in parallel
  3. Verify each sandbox is isolated
  4. Parallel file operations
  5. Verify all sandboxes visible via API
  6. Kill all in parallel

Usage:
  python examples/test_concurrent.py
"""

import asyncio
import sys
import time

from opensandbox import Sandbox

GREEN = "\033[32m"
RED = "\033[31m"
BOLD = "\033[1m"
DIM = "\033[2m"
RESET = "\033[0m"

passed = 0
failed = 0

SANDBOX_COUNT = 5


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
    bold("║       Concurrent Sandbox Test                    ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    sandboxes: list[Sandbox] = []

    try:
        # ── Test 1: Create N sandboxes simultaneously ──
        bold(f"━━━ Test 1: Create {SANDBOX_COUNT} sandboxes simultaneously ━━━\n")

        create_start = time.time()

        async def create_one(i: int) -> Sandbox:
            sb = await Sandbox.create(template="base", timeout=120)
            dim(f"Sandbox {i + 1}: {sb.sandbox_id} ({sb.domain})")
            return sb

        results = await asyncio.gather(
            *[create_one(i) for i in range(SANDBOX_COUNT)],
            return_exceptions=True,
        )
        create_ms = (time.time() - create_start) * 1000

        for i, r in enumerate(results):
            if isinstance(r, Exception):
                check(f"Sandbox {i + 1} created", False, str(r))
            else:
                sandboxes.append(r)
                check(f"Sandbox {i + 1} created", True)

        dim(f"Total create time: {create_ms:.0f}ms ({create_ms / SANDBOX_COUNT:.0f}ms avg)")
        print()

        # ── Test 2: Run commands on all in parallel ──
        bold("━━━ Test 2: Run commands on all sandboxes in parallel ━━━\n")

        cmd_start = time.time()

        async def run_echo(sb: Sandbox, i: int) -> dict:
            r = await sb.commands.run(f'echo "sandbox-{i}-{sb.sandbox_id}"')
            return {"index": i, "id": sb.sandbox_id, "result": r}

        cmd_results = await asyncio.gather(
            *[run_echo(sb, i) for i, sb in enumerate(sandboxes)],
            return_exceptions=True,
        )
        cmd_ms = (time.time() - cmd_start) * 1000

        for r in cmd_results:
            if isinstance(r, Exception):
                check("Command execution", False, str(r))
            else:
                expected = f"sandbox-{r['index']}-{r['id']}"
                check(f"Sandbox {r['index'] + 1} echo correct",
                      r["result"].stdout.strip() == expected,
                      r["result"].stdout.strip())

        dim(f"Total command time: {cmd_ms:.0f}ms")
        print()

        # ── Test 3: Verify isolation ──
        bold("━━━ Test 3: Verify sandbox isolation ━━━\n")

        await asyncio.gather(
            *[sb.files.write("/tmp/identity.txt", f"sandbox-{i}")
              for i, sb in enumerate(sandboxes)]
        )

        read_results = await asyncio.gather(
            *[sb.files.read("/tmp/identity.txt") for sb in sandboxes]
        )

        for i, content in enumerate(read_results):
            check(f"Sandbox {i + 1} sees only its own data",
                  content == f"sandbox-{i}", content)

        pid_results = await asyncio.gather(
            *[sb.commands.run("echo $$") for sb in sandboxes]
        )

        for i, r in enumerate(pid_results):
            dim(f"Sandbox {i + 1} shell PID: {r.stdout.strip()}")

        check("PID namespace isolation (low PIDs)",
              all(int(r.stdout.strip()) < 1000 for r in pid_results),
              f"PIDs: {', '.join(r.stdout.strip() for r in pid_results)}")
        print()

        # ── Test 4: Parallel file operations ──
        bold("━━━ Test 4: Parallel file operations across sandboxes ━━━\n")

        async def file_ops(sb: Sandbox, i: int) -> dict:
            # Write 10 files
            await asyncio.gather(
                *[sb.files.write(f"/tmp/file-{j}.txt", f"sb{i}-file{j}")
                  for j in range(10)]
            )
            # Read them back
            contents = await asyncio.gather(
                *[sb.files.read(f"/tmp/file-{j}.txt") for j in range(10)]
            )
            return {
                "index": i,
                "all_correct": all(c == f"sb{i}-file{j}" for j, c in enumerate(contents)),
                "count": len(contents),
            }

        file_results = await asyncio.gather(
            *[file_ops(sb, i) for i, sb in enumerate(sandboxes)]
        )

        for r in file_results:
            check(f"Sandbox {r['index'] + 1}: {r['count']} files written and verified",
                  r["all_correct"])
        print()

        # ── Test 5: Verify all sandboxes visible via API ──
        bold("━━━ Test 5: Verify all sandboxes visible via API ━━━\n")

        statuses = await asyncio.gather(
            *[sb.is_running() for sb in sandboxes]
        )

        for i, running in enumerate(statuses):
            check(f"Sandbox {i + 1} still running", running)
        print()

        # ── Test 6: Kill all in parallel ──
        bold(f"━━━ Test 6: Kill all {SANDBOX_COUNT} sandboxes simultaneously ━━━\n")

        kill_start = time.time()

        async def kill_one(sb: Sandbox, i: int) -> dict:
            try:
                await sb.kill()
                return {"index": i, "success": True}
            except Exception as e:
                return {"index": i, "success": False, "error": str(e)}

        kill_results = await asyncio.gather(
            *[kill_one(sb, i) for i, sb in enumerate(sandboxes)]
        )
        kill_ms = (time.time() - kill_start) * 1000

        for r in kill_results:
            check(f"Sandbox {r['index'] + 1} killed", r["success"],
                  r.get("error", ""))

        dim(f"Total kill time: {kill_ms:.0f}ms")

        await asyncio.sleep(1)

        post_kill = await asyncio.gather(
            *[sb.is_running() for sb in sandboxes]
        )

        for i, running in enumerate(post_kill):
            check(f"Sandbox {i + 1} confirmed stopped", not running)
        print()

    except Exception as e:
        red(f"Fatal error: {e}")
        failed += 1
        for sb in sandboxes:
            try:
                await sb.kill()
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
