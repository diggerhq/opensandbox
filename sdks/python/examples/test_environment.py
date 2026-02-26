#!/usr/bin/env python3
"""
Environment & HOME Directory Test

Verifies that HOME=/workspace inside sandboxes so that tools like npm, pip,
git etc. use the NVMe-backed workspace drive for caches and config.

Tests:
  1. HOME is set to /workspace
  2. Tilde (~) expands to /workspace
  3. npm cache dir is under /workspace
  4. npm install writes cache to /workspace (not /root)
  5. Dotfiles and configs use /workspace
  6. /workspace is NVMe-backed

Usage:
  python examples/test_environment.py
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
    bold("║       Environment & HOME Directory Test          ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    sandbox = None

    try:
        sandbox = await Sandbox.create(template="node", timeout=120)
        green(f"Created sandbox: {sandbox.sandbox_id}")
        print()

        # ── Test 1: HOME is /workspace ──
        bold("━━━ Test 1: HOME environment variable ━━━\n")

        result = await sandbox.commands.run("echo $HOME")
        check("HOME is /workspace", result.stdout.strip() == "/workspace",
              f'got "{result.stdout.strip()}"')

        # ── Test 2: Tilde expansion ──
        bold("━━━ Test 2: Tilde (~) expansion ━━━\n")

        result = await sandbox.commands.run("echo ~")
        check("~ expands to /workspace", result.stdout.strip() == "/workspace",
              f'got "{result.stdout.strip()}"')

        result = await sandbox.commands.run("echo ~/test")
        check("~/test expands to /workspace/test",
              result.stdout.strip() == "/workspace/test",
              f'got "{result.stdout.strip()}"')
        print()

        # ── Test 3: npm cache directory ──
        bold("━━━ Test 3: npm cache directory ━━━\n")

        result = await sandbox.commands.run("npm config get cache")
        npm_cache = result.stdout.strip()
        check("npm cache is under /workspace", npm_cache.startswith("/workspace"),
              f'got "{npm_cache}"')
        check("npm cache is NOT under /root", not npm_cache.startswith("/root"),
              f'got "{npm_cache}"')
        dim(f"npm cache dir: {npm_cache}")
        print()

        # ── Test 4: npm install writes to workspace ──
        bold("━━━ Test 4: npm install uses workspace for cache ━━━\n")

        await sandbox.commands.run("mkdir -p /workspace/npm-test")
        await sandbox.files.write("/workspace/npm-test/package.json",
                                  '{"name":"test","version":"1.0.0","dependencies":{"is-odd":"3.0.1"}}')

        install_result = await sandbox.commands.run(
            "cd /workspace/npm-test && npm install --prefer-offline 2>&1",
            timeout=30)
        dim(f"npm install exit code: {install_result.exit_code}")

        root_check = await sandbox.commands.run(
            "du -sh /root/.npm 2>/dev/null || echo 'no /root/.npm'")
        check("No npm cache in /root/.npm",
              "no /root/.npm" in root_check.stdout,
              f'got "{root_check.stdout.strip()}"')

        workspace_cache = await sandbox.commands.run(
            "test -d /workspace/.npm && echo 'exists' || echo 'missing'")
        check("npm cache exists at /workspace/.npm",
              workspace_cache.stdout.strip() == "exists",
              f'got "{workspace_cache.stdout.strip()}"')
        print()

        # ── Test 5: Dotfiles go to workspace ──
        bold("━━━ Test 5: Dotfiles and configs use /workspace ━━━\n")

        result = await sandbox.commands.run("bash -c 'echo $HOME/.bashrc'")
        check(".bashrc path is /workspace/.bashrc",
              result.stdout.strip() == "/workspace/.bashrc",
              f'got "{result.stdout.strip()}"')

        await sandbox.commands.run("git config --global user.name 'Test User'")
        result = await sandbox.commands.run(
            "git config --global --list --show-origin 2>/dev/null | head -1")
        check("git config is under /workspace",
              "/workspace" in result.stdout,
              f'got "{result.stdout.strip()}"')
        print()

        # ── Test 6: /workspace is on the right filesystem ──
        bold("━━━ Test 6: /workspace is NVMe-backed ━━━\n")

        result = await sandbox.commands.run("df -h /workspace | tail -1")
        df_line = result.stdout.strip()
        dim(f"df output: {df_line}")
        check("/workspace is NOT on /dev/root", not df_line.startswith("/dev/root"),
              f'got "{df_line}"')
        print()

        # Clean up
        await sandbox.commands.run("rm -rf /workspace/npm-test")

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
