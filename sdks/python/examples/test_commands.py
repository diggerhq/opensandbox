#!/usr/bin/env python3
"""
Command Edge Cases Test

Tests:
  1. Basic commands
  2. stderr handling
  3. Non-zero exit codes
  4. Large stdout output
  5. Environment variable passing
  6. Working directory
  7. Shell features (pipes, redirects, subshells)
  8. Concurrent commands on same sandbox
  9. Command timeout

Usage:
  python examples/test_commands.py
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
    bold("║       Command Edge Cases Test                    ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    sandbox = None

    try:
        sandbox = await Sandbox.create(template="base", timeout=120)
        green(f"Created sandbox: {sandbox.sandbox_id}")
        print()

        # ── Test 1: Basic commands ──
        bold("━━━ Test 1: Basic commands ━━━\n")

        echo = await sandbox.commands.run("echo hello-world")
        check("Echo returns correct output", echo.stdout.strip() == "hello-world")
        check("Echo exit code is 0", echo.exit_code == 0)

        multi = await sandbox.commands.run("echo line1 && echo line2 && echo line3")
        lines = multi.stdout.strip().split("\n")
        check("Multi-command outputs 3 lines", len(lines) == 3)
        check("Multi-command content correct", lines[0] == "line1" and lines[2] == "line3")
        print()

        # ── Test 2: stderr handling ──
        bold("━━━ Test 2: stderr handling ━━━\n")

        stderr_cmd = await sandbox.commands.run("echo error-msg >&2")
        check("stderr captured", stderr_cmd.stderr.strip() == "error-msg")
        check("stdout empty when writing to stderr", stderr_cmd.stdout.strip() == "")
        check("Exit code 0 even with stderr", stderr_cmd.exit_code == 0)

        mixed = await sandbox.commands.run("echo stdout-data && echo stderr-data >&2")
        check("Mixed: stdout captured", "stdout-data" in mixed.stdout)
        check("Mixed: stderr captured", "stderr-data" in mixed.stderr)
        print()

        # ── Test 3: Non-zero exit codes ──
        bold("━━━ Test 3: Non-zero exit codes ━━━\n")

        exit1 = await sandbox.commands.run("exit 1")
        check("Exit code 1 captured", exit1.exit_code == 1, f"got {exit1.exit_code}")

        exit42 = await sandbox.commands.run("exit 42")
        check("Exit code 42 captured", exit42.exit_code == 42, f"got {exit42.exit_code}")

        false_cmd = await sandbox.commands.run("false")
        check("'false' returns exit code 1", false_cmd.exit_code == 1, f"got {false_cmd.exit_code}")

        not_found = await sandbox.commands.run("nonexistent-command-xyz 2>&1 || true")
        check("Non-existent command handled", not_found.exit_code == 0)
        print()

        # ── Test 4: Large stdout ──
        bold("━━━ Test 4: Large stdout output ━━━\n")

        large_out = await sandbox.commands.run("seq 1 10000")
        line_count = len(large_out.stdout.strip().split("\n"))
        check("10000 lines of output captured", line_count == 10000, f"got {line_count} lines")
        dim(f"Output size: {len(large_out.stdout)} chars")

        large_lines = large_out.stdout.strip().split("\n")
        check("First line is 1", large_lines[0] == "1")
        check("Last line is 10000", large_lines[-1] == "10000")
        print()

        # ── Test 5: Environment variables ──
        bold("━━━ Test 5: Environment variable passing ━━━\n")

        env_result = await sandbox.commands.run("echo $MY_VAR",
                                                env={"MY_VAR": "secret-value-123"})
        check("Env var passed correctly", env_result.stdout.strip() == "secret-value-123")

        multi_env = await sandbox.commands.run('echo "$A:$B:$C"',
                                               env={"A": "alpha", "B": "beta", "C": "gamma"})
        check("Multiple env vars", multi_env.stdout.strip() == "alpha:beta:gamma")

        special_env = await sandbox.commands.run("echo $SPECIAL",
                                                 env={"SPECIAL": "hello world with spaces & stuff"})
        check("Env var with special chars",
              special_env.stdout.strip() == "hello world with spaces & stuff")
        print()

        # ── Test 6: Working directory ──
        bold("━━━ Test 6: Working directory ━━━\n")

        await sandbox.commands.run("mkdir -p /tmp/workdir/sub")
        await sandbox.files.write("/tmp/workdir/sub/data.txt", "found-it")

        cwd_result = await sandbox.commands.run("cat data.txt", cwd="/tmp/workdir/sub")
        check("Working directory respected", cwd_result.stdout.strip() == "found-it")

        pwd_result = await sandbox.commands.run("pwd", cwd="/tmp/workdir")
        check("pwd reflects cwd", pwd_result.stdout.strip() == "/tmp/workdir")
        print()

        # ── Test 7: Shell features ──
        bold("━━━ Test 7: Shell features (pipes, redirects, subshells) ━━━\n")

        pipe_result = await sandbox.commands.run("echo 'hello world' | tr ' ' '-'")
        check("Pipe works", pipe_result.stdout.strip() == "hello-world")

        subshell = await sandbox.commands.run("echo $(hostname)")
        check("Command substitution works", len(subshell.stdout.strip()) > 0, subshell.stdout.strip())

        await sandbox.commands.run("echo redirect-test > /tmp/redirect.txt")
        redirect_content = await sandbox.files.read("/tmp/redirect.txt")
        check("Redirect to file works", redirect_content.strip() == "redirect-test")

        await sandbox.commands.run("touch /tmp/wc-a.txt /tmp/wc-b.txt /tmp/wc-c.txt")
        wc_result = await sandbox.commands.run("ls /tmp/wc-*.txt | wc -l")
        check("Wildcard expansion works", wc_result.stdout.strip() == "3")

        arith = await sandbox.commands.run("echo $((42 * 7))")
        check("Arithmetic expansion works", arith.stdout.strip() == "294")

        here_str = await sandbox.commands.run("bash -c \"cat <<< 'here-string-data'\"")
        check("Here string works", here_str.stdout.strip() == "here-string-data")
        print()

        # ── Test 8: Concurrent commands ──
        bold("━━━ Test 8: Concurrent commands on same sandbox ━━━\n")

        concurrent_start = time.time()

        async def run_concurrent(i: int) -> dict:
            r = await sandbox.commands.run(f"echo concurrent-{i}")
            return {"index": i, "output": r.stdout.strip(), "exit_code": r.exit_code}

        tasks = [run_concurrent(i) for i in range(10)]
        concurrent_results = await asyncio.gather(*tasks)
        concurrent_ms = (time.time() - concurrent_start) * 1000

        all_correct = True
        for r in concurrent_results:
            if r["output"] != f"concurrent-{r['index']}" or r["exit_code"] != 0:
                all_correct = False
                dim(f"Command {r['index']}: expected \"concurrent-{r['index']}\", "
                    f"got \"{r['output']}\" (exit {r['exit_code']})")

        check("10 concurrent commands all returned correctly", all_correct)
        dim(f"Total concurrent time: {concurrent_ms:.0f}ms")
        print()

        # ── Test 9: Command timeout ──
        bold("━━━ Test 9: Command timeout ━━━\n")

        timeout_start = time.time()
        try:
            result = await sandbox.commands.run("sleep 30", timeout=3)
            timeout_ms = (time.time() - timeout_start) * 1000
            check("Command timed out within ~3s", timeout_ms < 10000, f"took {timeout_ms:.0f}ms")
        except Exception as e:
            timeout_ms = (time.time() - timeout_start) * 1000
            check("Command timed out within ~3s", timeout_ms < 10000,
                  f"took {timeout_ms:.0f}ms, error: {e}")
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
