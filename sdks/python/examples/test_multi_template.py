#!/usr/bin/env python3
"""
Multi-Template Production Test

Verifies all 3 templates (base, python, node) create, run commands,
and have the expected runtimes available.

Usage:
  python examples/test_multi_template.py
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


TEMPLATES = [
    {
        "template": "base",
        "expected_binary": "bash",
        "version_cmd": "bash --version | head -1",
        "version_prefix": "GNU bash",
        "test_cmd": "echo 'hello from base'",
        "test_expected": "hello from base",
    },
    {
        "template": "python",
        "expected_binary": "python3",
        "version_cmd": "python3 --version",
        "version_prefix": "Python 3",
        "test_cmd": 'python3 -c "print(2 + 2)"',
        "test_expected": "4",
    },
    {
        "template": "node",
        "expected_binary": "node",
        "version_cmd": "node --version",
        "version_prefix": "v",
        "test_cmd": 'node -e "console.log(JSON.stringify({ok:true}))"',
        "test_expected": '{"ok":true}',
    },
]


async def test_template(t: dict, index: int) -> tuple[int, int]:
    p = 0
    f = 0

    def local_check(desc: str, condition: bool, detail: str = "") -> None:
        nonlocal p, f
        if condition:
            green(desc)
            p += 1
        else:
            red(f"{desc} ({detail})" if detail else desc)
            f += 1

    bold(f'\n━━━ Template {index + 1}/3: "{t["template"]}" ━━━\n')
    sandbox = None

    try:
        start = time.time()
        sandbox = await Sandbox.create(template=t["template"], timeout=120)
        create_ms = (time.time() - start) * 1000
        local_check(f"Created {t['template']} sandbox ({create_ms:.0f}ms)", True)
        dim(f"ID: {sandbox.sandbox_id}")
        dim(f"Domain: {sandbox.domain}")

        local_check("Domain assigned", bool(sandbox.domain))

        which = await sandbox.commands.run(f"which {t['expected_binary']}")
        local_check(f"{t['expected_binary']} binary exists",
                     which.exit_code == 0, which.stderr.strip())

        version = await sandbox.commands.run(t["version_cmd"])
        local_check(f'Version starts with "{t["version_prefix"]}"',
                     version.stdout.strip().startswith(t["version_prefix"]),
                     version.stdout.strip())
        dim(f"Version: {version.stdout.strip()}")

        test = await sandbox.commands.run(t["test_cmd"])
        local_check("Test command output correct",
                     test.stdout.strip() == t["test_expected"],
                     f'expected "{t["test_expected"]}", got "{test.stdout.strip()}"')

        await sandbox.files.write("/tmp/template-test.txt", f"from-{t['template']}")
        content = await sandbox.files.read("/tmp/template-test.txt")
        local_check("File ops work", content == f"from-{t['template']}")

        uname = await sandbox.commands.run("uname -s")
        local_check("Running on Linux", uname.stdout.strip() == "Linux")

    except Exception as e:
        red(f"  Error testing {t['template']}: {e}")
        f += 1
    finally:
        if sandbox:
            await sandbox.kill()
            dim("Sandbox killed")

    return p, f


async def main() -> None:
    global passed, failed

    bold("\n╔══════════════════════════════════════════════════╗")
    bold("║       Multi-Template Production Test             ║")
    bold("╚══════════════════════════════════════════════════╝")

    for i, t in enumerate(TEMPLATES):
        p, f = await test_template(t, i)
        passed += p
        failed += f

    print()
    bold("========================================")
    bold(f" Results: {passed} passed, {failed} failed")
    bold("========================================\n")
    if failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
