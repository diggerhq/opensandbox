#!/usr/bin/env python3
"""
Default Template Verification Test

Verifies that the "default" template image contains all expected packages:
  1. Python 3 + pip + venv
  2. Node.js 20 + npm
  3. Build tools (gcc, g++, make, cmake)
  4. Git + git-lfs
  5. Common utilities (curl, wget, jq, tar, zip, unzip, etc.)
  6. System libraries (libssl, libffi, zlib, sqlite3)
  7. Locale (en_US.UTF-8 available)
  8. Workspace directory

Usage:
  python examples/test_default_template.py
"""

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from opencomputer import Sandbox

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


async def expect_command(
    sandbox: Sandbox,
    desc: str,
    cmd: str,
    contains: str | None = None,
) -> str:
    """Run a command and check it exits 0 and stdout contains expected substring."""
    result = await sandbox.commands.run(cmd)
    out = (result.stdout.strip() or result.stderr.strip())
    dim(f"$ {cmd}")
    dim(f"  → {out}")

    if result.exit_code != 0:
        check(desc, False, f"exit code {result.exit_code}")
        return out

    if contains is not None:
        check(desc, contains.lower() in out.lower(),
              f'expected "{contains}" in output')
    else:
        check(desc, True)
    return out


async def main() -> None:
    global passed, failed

    bold("\n╔══════════════════════════════════════════════════╗")
    bold("║     Default Template Verification Test           ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    sandbox = None

    try:
        sandbox = await Sandbox.create(template="default", timeout=120)
        green(f"Created sandbox: {sandbox.sandbox_id}")
        print()

        # ── 1. Python ──
        bold("━━━ 1. Python 3 ━━━\n")

        await expect_command(sandbox, "python3 is installed",
                             "python3 --version", contains="Python 3")
        await expect_command(sandbox, "python symlink works",
                             "python --version", contains="Python 3")
        await expect_command(sandbox, "pip3 is installed",
                             "pip3 --version", contains="pip")
        await expect_command(sandbox, "python3-venv is available",
                             "python3 -m venv --help 2>&1 | head -1", contains="usage")

        pip_install = await sandbox.commands.run(
            "pip3 install --no-cache-dir cowsay 2>&1", timeout=30)
        check("pip install works", pip_install.exit_code == 0,
              f"exit {pip_install.exit_code}")
        cowsay = await sandbox.commands.run(
            "python3 -c 'import cowsay; print(\"pip-ok\")'")
        check("installed Python package importable",
              "pip-ok" in cowsay.stdout.strip())
        print()

        # ── 2. Node.js ──
        bold("━━━ 2. Node.js 20 + npm ━━━\n")

        node_version = await expect_command(sandbox, "node is installed",
                                            "node --version")
        check("Node.js is v20.x", node_version.startswith("v20"),
              f"got {node_version}")
        await expect_command(sandbox, "npm is installed", "npm --version")

        await sandbox.commands.run(
            "mkdir -p /tmp/npm-test && cd /tmp/npm-test && npm init -y 2>&1",
            timeout=10)
        npm_install = await sandbox.commands.run(
            "cd /tmp/npm-test && npm install is-odd 2>&1", timeout=30)
        check("npm install works", npm_install.exit_code == 0,
              f"exit {npm_install.exit_code}")
        node_run = await sandbox.commands.run(
            """node -e "console.log(require('is-odd')(3))" """,
            cwd="/tmp/npm-test")
        check("installed npm package works",
              node_run.stdout.strip() == "true",
              f'got "{node_run.stdout.strip()}"')
        print()

        # ── 3. Build tools ──
        bold("━━━ 3. Build tools ━━━\n")

        await expect_command(sandbox, "gcc is installed",
                             "gcc --version 2>&1 | head -1", contains="gcc")
        await expect_command(sandbox, "g++ is installed",
                             "g++ --version 2>&1 | head -1", contains="g++")
        await expect_command(sandbox, "make is installed",
                             "make --version 2>&1 | head -1", contains="make")
        await expect_command(sandbox, "cmake is installed",
                             "cmake --version 2>&1 | head -1", contains="cmake")
        await expect_command(sandbox, "pkg-config is installed",
                             "pkg-config --version")

        await sandbox.files.write("/tmp/hello.c",
            '#include <stdio.h>\nint main() { printf("compiled-ok\\n"); return 0; }')
        compile_result = await sandbox.commands.run(
            "gcc -o /tmp/hello /tmp/hello.c && /tmp/hello")
        check("C compilation + execution works",
              compile_result.stdout.strip() == "compiled-ok",
              f'got "{compile_result.stdout.strip()}"')
        print()

        # ── 4. Git ──
        bold("━━━ 4. Git ━━━\n")

        await expect_command(sandbox, "git is installed",
                             "git --version", contains="git version")
        await expect_command(sandbox, "git-lfs is installed",
                             "git lfs version 2>&1 | head -1", contains="git-lfs")
        print()

        # ── 5. Networking & utilities ──
        bold("━━━ 5. Networking & common utilities ━━━\n")

        await expect_command(sandbox, "curl is installed",
                             "curl --version 2>&1 | head -1", contains="curl")
        await expect_command(sandbox, "wget is installed",
                             "wget --version 2>&1 | head -1", contains="wget")
        await expect_command(sandbox, "ssh client is installed",
                             "ssh -V 2>&1", contains="OpenSSH")
        await expect_command(sandbox, "jq is installed",
                             "jq --version", contains="jq")
        await expect_command(sandbox, "tar is installed",
                             "tar --version 2>&1 | head -1", contains="tar")
        await expect_command(sandbox, "zip is installed",
                             "zip --version 2>&1 | head -2 | tail -1", contains="zip")
        await expect_command(sandbox, "unzip is installed",
                             "unzip -v 2>&1 | head -1", contains="unzip")
        await expect_command(sandbox, "rsync is installed",
                             "rsync --version 2>&1 | head -1", contains="rsync")
        await expect_command(sandbox, "htop is installed",
                             "htop --version 2>&1 | head -1", contains="htop")
        await expect_command(sandbox, "tree is installed",
                             "tree --version 2>&1", contains="tree")
        print()

        # ── 6. System libraries ──
        bold("━━━ 6. System libraries ━━━\n")

        await expect_command(sandbox, "sqlite3 is installed",
                             "sqlite3 --version 2>&1 | head -1")
        await expect_command(sandbox, "libssl headers present",
                             "test -f /usr/include/openssl/ssl.h && echo ok",
                             contains="ok")
        await expect_command(
            sandbox, "libffi headers present",
            "test -f /usr/include/ffi.h && echo ok "
            "|| (dpkg -L libffi-dev 2>/dev/null | grep ffi.h | head -1)",
            contains="ffi")
        await expect_command(sandbox, "zlib headers present",
                             "test -f /usr/include/zlib.h && echo ok",
                             contains="ok")
        print()

        # ── 7. Locale ──
        bold("━━━ 7. Locale ━━━\n")

        await expect_command(sandbox, "en_US.UTF-8 locale is available",
                             "locale -a 2>&1 | grep -i en_US.utf8",
                             contains="en_US")
        print()

        # ── 8. Workspace ──
        bold("━━━ 8. Workspace directory ━━━\n")

        await expect_command(sandbox, "/workspace exists",
                             "test -d /workspace && echo ok", contains="ok")
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
