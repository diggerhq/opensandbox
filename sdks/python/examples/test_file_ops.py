#!/usr/bin/env python3
"""
File Operations Edge Cases Test

Tests:
  1. Large file write/read (1MB)
  2. Special characters in content
  3. Deeply nested directories
  4. File deletion and overwrite
  5. Large directory listing
  6. Empty file handling
  7. File exists / not exists
  8. Write via commands + read via SDK

Usage:
  python examples/test_file_ops.py
"""

import asyncio
import json
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
    bold("║       File Operations Edge Cases Test            ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    sandbox = None

    try:
        sandbox = await Sandbox.create(template="base", timeout=120)
        green(f"Created sandbox: {sandbox.sandbox_id}")
        print()

        # ── Test 1: Large file ──
        bold("━━━ Test 1: Large file (1MB) ━━━\n")

        one_mb = "X" * (1024 * 1024)
        write_start = time.time()
        await sandbox.files.write("/tmp/large.txt", one_mb)
        write_ms = (time.time() - write_start) * 1000
        dim(f"Write: {write_ms:.0f}ms")

        read_start = time.time()
        large_content = await sandbox.files.read("/tmp/large.txt")
        read_ms = (time.time() - read_start) * 1000
        dim(f"Read: {read_ms:.0f}ms")

        check("1MB file size preserved", len(large_content) == len(one_mb),
              f"{len(large_content)} bytes")
        check("1MB file content intact", large_content == one_mb)
        print()

        # ── Test 2: Special characters ──
        bold("━━━ Test 2: Special characters ━━━\n")

        special_content = 'Hello "world" & <tag> \'quotes\' \\ newline\nTab\there 日本語 emoji🎉 nullish ?? chain?.'
        await sandbox.files.write("/tmp/special.txt", special_content)
        special_read = await sandbox.files.read("/tmp/special.txt")
        check("Special characters preserved", special_read == special_content,
              f"got: {special_read[:50]}...")

        json_content = json.dumps({"key": "value", "nested": {"arr": [1, 2, 3]}, "unicode": "日本語"}, indent=2)
        await sandbox.files.write("/tmp/data.json", json_content)
        json_read = await sandbox.files.read("/tmp/data.json")
        check("JSON content preserved", json_read == json_content)

        multiline = "\n".join(f"Line {i + 1}: Some content here" for i in range(100))
        await sandbox.files.write("/tmp/multiline.txt", multiline)
        multi_read = await sandbox.files.read("/tmp/multiline.txt")
        check("100-line file preserved", multi_read == multiline,
              f"lines: {len(multi_read.split(chr(10)))}")
        print()

        # ── Test 3: Deeply nested directories ──
        bold("━━━ Test 3: Deeply nested directories ━━━\n")

        deep_path = "/tmp/a/b/c/d/e/f/g/h"
        await sandbox.commands.run(f"mkdir -p {deep_path}")
        await sandbox.files.write(f"{deep_path}/deep.txt", "bottom-of-tree")
        deep_content = await sandbox.files.read(f"{deep_path}/deep.txt")
        check("8-level nested file created and read", deep_content == "bottom-of-tree")

        mid_entries = await sandbox.files.list("/tmp/a/b/c/d")
        check("Intermediate dir lists correctly",
              any(e.name == "e" and e.is_dir for e in mid_entries))
        print()

        # ── Test 4: File deletion and overwrite ──
        bold("━━━ Test 4: File deletion and overwrite ━━━\n")

        await sandbox.files.write("/tmp/overwrite.txt", "original")
        content = await sandbox.files.read("/tmp/overwrite.txt")
        check("Original content written", content == "original")

        await sandbox.files.write("/tmp/overwrite.txt", "overwritten")
        content = await sandbox.files.read("/tmp/overwrite.txt")
        check("Overwritten content correct", content == "overwritten")

        await sandbox.files.write("/tmp/overwrite.txt", "short")
        content = await sandbox.files.read("/tmp/overwrite.txt")
        check("Shorter overwrite correct (no trailing data)", content == "short")

        exists_before = await sandbox.files.exists("/tmp/overwrite.txt")
        check("File exists before delete", exists_before)

        await sandbox.files.remove("/tmp/overwrite.txt")
        exists_after = await sandbox.files.exists("/tmp/overwrite.txt")
        check("File gone after delete", not exists_after)

        await sandbox.files.remove("/tmp/a")
        dir_gone = await sandbox.files.exists(f"{deep_path}/deep.txt")
        check("Recursive directory deletion", not dir_gone)
        print()

        # ── Test 5: Large directory listing ──
        bold("━━━ Test 5: Large directory listing ━━━\n")

        await sandbox.commands.run(
            "for i in $(seq 1 50); do echo content-$i > /tmp/listtest-$i.txt; done")
        entries = await sandbox.files.list("/tmp")
        list_test_files = [e for e in entries if e.name.startswith("listtest-")]
        check("50 files visible in listing", len(list_test_files) == 50,
              f"found {len(list_test_files)}")

        entry = list_test_files[0]
        check("Entry has name", bool(entry.name))
        check("Entry has is_dir=False", entry.is_dir is False)
        check("Entry has size > 0", entry.size > 0, f"size={entry.size}")
        print()

        # ── Test 6: Empty file ──
        bold("━━━ Test 6: Empty file handling ━━━\n")

        await sandbox.files.write("/tmp/empty.txt", "")
        empty_content = await sandbox.files.read("/tmp/empty.txt")
        check("Empty file returns empty string", empty_content == "",
              f'got: "{empty_content}"')
        check("Empty file exists", await sandbox.files.exists("/tmp/empty.txt"))
        print()

        # ── Test 7: File exists checks ──
        bold("━━━ Test 7: File exists checks ━━━\n")

        check("Existing file → true", await sandbox.files.exists("/tmp/special.txt"))
        check("Non-existent file → false", not await sandbox.files.exists("/tmp/nope-no-way.txt"))
        check("Non-existent deep path → false",
              not await sandbox.files.exists("/tmp/no/such/path/file.txt"))
        print()

        # ── Test 8: Write via commands + read via SDK ──
        bold("━━━ Test 8: Write via commands + read via SDK ━━━\n")

        await sandbox.commands.run(
            "dd if=/dev/urandom bs=256 count=1 2>/dev/null | base64 > /tmp/random.b64")
        b64_content = await sandbox.files.read("/tmp/random.b64")
        check("Base64 random data readable", len(b64_content) > 100,
              f"{len(b64_content)} chars")

        await sandbox.commands.run('echo -n "command-written" > /tmp/cmd-file.txt')
        cmd_file_content = await sandbox.files.read("/tmp/cmd-file.txt")
        check("Command-written file readable via SDK",
              cmd_file_content == "command-written")
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
