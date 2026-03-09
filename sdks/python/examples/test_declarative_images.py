#!/usr/bin/env python3
"""
Declarative Image Builder & Snapshots Test (Python SDK)

Tests both patterns for defining sandbox environments:

  Pattern 1 — On-demand images (cached by content hash):
    image = Image.base().apt_install(["curl"])
    sandbox = await Sandbox.create(image=image)

  Pattern 2 — Pre-built named snapshots:
    await snapshots.create(name="my-env", image=image)
    sandbox = await Sandbox.create(snapshot="my-env")

Usage:
  python examples/test_declarative_images.py
"""

import asyncio
import sys
import time

from opencomputer import Sandbox, Image, Snapshots

passed = 0
failed = 0


def green(msg: str) -> None:
    print(f"\033[32m\u2713 {msg}\033[0m")


def red(msg: str) -> None:
    print(f"\033[31m\u2717 {msg}\033[0m")


def bold(msg: str) -> None:
    print(f"\033[1m{msg}\033[0m")


def dim(msg: str) -> None:
    print(f"\033[2m  {msg}\033[0m")


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

    bold("\n╔══════════════════════════════════════════════════════╗")
    bold("║  Declarative Image Builder & Snapshots Test (Python) ║")
    bold("╚══════════════════════════════════════════════════════════╝\n")

    sandboxes: list[Sandbox] = []
    snapshots = Snapshots()

    try:
        # ── Test: Image builder basics ──────────────────────────────
        bold("━━━ Test: Image builder basics ━━━\n")

        base = Image.base()
        with_curl = base.apt_install(["curl"])
        with_python = with_curl.pip_install(["requests"])

        check("Image.base() uses default base", base.to_dict()["base"] == "base")
        check("Image is immutable — apt_install returns new instance", len(base.to_dict()["steps"]) == 0)
        check("Chained image has 1 step", len(with_curl.to_dict()["steps"]) == 1)
        check("Further chained image has 2 steps", len(with_python.to_dict()["steps"]) == 2)

        check("Different images have different cache keys", base.cache_key() != with_curl.cache_key())
        check("Same image produces same cache key", base.cache_key() == Image.base().cache_key())

        # File operations
        with_file = base.add_file("/workspace/config.json", '{"key": "value"}')
        check("add_file creates a step", len(with_file.to_dict()["steps"]) == 1)
        check("add_file step type is correct", with_file.to_dict()["steps"][0]["type"] == "add_file")
        print()

        # ── Pattern 1: On-demand image creation ────────────────────
        bold("━━━ Pattern 1: On-demand image (first build — cold) ━━━\n")

        image = (
            Image.base()
            .apt_install(["curl", "jq"])
            .run_commands("mkdir -p /workspace/project")
            .env({"MY_VAR": "hello-from-image", "PROJECT_ROOT": "/workspace/project"})
            .workdir("/workspace/project")
        )

        dim(f"Cache key: {image.cache_key()}")
        dim("Creating sandbox from image (this will build on first run)...")

        t1_start = time.time()
        sandbox1 = await Sandbox.create(image=image, timeout=300)
        t1_elapsed = int((time.time() - t1_start) * 1000)
        sandboxes.append(sandbox1)

        green(f"Sandbox created: {sandbox1.sandbox_id} ({t1_elapsed}ms)")
        check("Sandbox created successfully", sandbox1.status in ("running", "creating"))
        print()

        # ── Verify installed packages ──────────────────────────────
        bold("━━━ Verify: Packages installed ━━━\n")

        curl_check = await sandbox1.commands.run("which curl")
        check("curl is installed", curl_check.exit_code == 0, f"exit={curl_check.exit_code}")

        jq_check = await sandbox1.commands.run("which jq")
        check("jq is installed", jq_check.exit_code == 0, f"exit={jq_check.exit_code}")
        print()

        # ── Verify env vars ────────────────────────────────────────
        bold("━━━ Verify: Environment variables ━━━\n")

        env_check = await sandbox1.commands.run("bash -lc 'echo $MY_VAR'")
        check("MY_VAR is set", env_check.stdout.strip() == "hello-from-image", f'got: "{env_check.stdout.strip()}"')

        root_check = await sandbox1.commands.run("bash -lc 'echo $PROJECT_ROOT'")
        check("PROJECT_ROOT is set", root_check.stdout.strip() == "/workspace/project", f'got: "{root_check.stdout.strip()}"')
        print()

        # ── Cache hit: second sandbox from same image ──────────────
        bold("━━━ Pattern 1: Cache hit (second sandbox, same image) ━━━\n")

        dim("Creating second sandbox from same image (should be cached)...")
        t2_start = time.time()
        sandbox2 = await Sandbox.create(image=image, timeout=300)
        t2_elapsed = int((time.time() - t2_start) * 1000)
        sandboxes.append(sandbox2)

        green(f"Second sandbox created: {sandbox2.sandbox_id} ({t2_elapsed}ms)")

        if t1_elapsed > 5000:
            check(
                f"Cache hit is faster ({t2_elapsed}ms vs {t1_elapsed}ms cold build)",
                t2_elapsed < t1_elapsed,
                f"cold={t1_elapsed}ms, cached={t2_elapsed}ms",
            )
        else:
            dim(f"Cold build was fast ({t1_elapsed}ms) — skipping speedup check")

        curl_check2 = await sandbox2.commands.run("which curl")
        check("Cached sandbox also has curl", curl_check2.exit_code == 0)
        print()

        # Kill sandboxes before creating more
        for sb in sandboxes:
            try:
                await sb.kill()
            except Exception:
                pass
        sandboxes.clear()

        # ── Pattern 2: Pre-built named snapshot ────────────────────
        bold("━━━ Pattern 2: Create named snapshot ━━━\n")

        snapshot_image = (
            Image.base()
            .run_commands(
                "echo 'snapshot-marker' > /workspace/snapshot-test.txt",
                "mkdir -p /workspace/data",
            )
            .env({"SNAPSHOT_ENV": "from-snapshot"})
        )

        dim("Creating snapshot 'test-env-py'...")
        snapshot_info = await snapshots.create(name="test-env-py", image=snapshot_image)
        green(f"Snapshot created: {snapshot_info['name']} (status={snapshot_info['status']})")
        check("Snapshot status is ready", snapshot_info["status"] == "ready")
        print()

        # ── List snapshots ─────────────────────────────────────────
        bold("━━━ Verify: List snapshots ━━━\n")

        snapshot_list = await snapshots.list()
        found = next((s for s in snapshot_list if s["name"] == "test-env-py"), None)
        check("Snapshot appears in list", found is not None)
        print()

        # ── Get snapshot by name ───────────────────────────────────
        bold("━━━ Verify: Get snapshot by name ━━━\n")

        fetched = await snapshots.get("test-env-py")
        check("Fetched snapshot matches", fetched["name"] == "test-env-py")
        check("Fetched snapshot is ready", fetched["status"] == "ready")
        print()

        # ── Create sandbox from named snapshot ─────────────────────
        bold("━━━ Pattern 2: Create sandbox from named snapshot ━━━\n")

        dim("Creating sandbox from snapshot 'test-env-py'...")
        sandbox3 = await Sandbox.create(snapshot="test-env-py", timeout=300)
        sandboxes.append(sandbox3)

        check("Sandbox created successfully", sandbox3.status in ("running", "creating"))

        marker_check = await sandbox3.commands.run("cat /workspace/snapshot-test.txt")
        check("Snapshot marker file exists", marker_check.stdout.strip() == "snapshot-marker", f'got: "{marker_check.stdout.strip()}"')

        snapshot_env_check = await sandbox3.commands.run("bash -lc 'echo $SNAPSHOT_ENV'")
        check("Snapshot env var is set", snapshot_env_check.stdout.strip() == "from-snapshot", f'got: "{snapshot_env_check.stdout.strip()}"')
        print()

        # ── Delete snapshot ────────────────────────────────────────
        bold("━━━ Cleanup: Delete snapshot ━━━\n")

        await snapshots.delete("test-env-py")
        green("Snapshot 'test-env-py' deleted")

        list_after = await snapshots.list()
        deleted_found = next((s for s in list_after if s["name"] == "test-env-py"), None)
        check("Snapshot no longer in list", deleted_found is None)
        print()

        # Kill sandbox before next test
        for sb in sandboxes:
            try:
                await sb.kill()
            except Exception:
                pass
        sandboxes.clear()

        # ── Test: addFile step ─────────────────────────────────────
        bold("━━━ Test: add_file — bake files into the image ━━━\n")

        file_image = (
            Image.base()
            .add_file("/workspace/config.json", '{"env": "production", "debug": false}')
            .add_file("/workspace/setup.sh", "#!/bin/bash\necho 'Hello from setup!'")
            .run_commands("chmod +x /workspace/setup.sh")
        )

        dim("Creating sandbox with embedded files...")
        sandbox4 = await Sandbox.create(image=file_image, timeout=300)
        sandboxes.append(sandbox4)

        config_check = await sandbox4.commands.run("cat /workspace/config.json")
        check("add_file wrote config.json", '"env": "production"' in config_check.stdout, f'got: "{config_check.stdout.strip()}"')

        setup_check = await sandbox4.commands.run("/workspace/setup.sh")
        check("add_file wrote executable script", setup_check.stdout.strip() == "Hello from setup!", f'got: "{setup_check.stdout.strip()}"')
        print()

        # Kill sandbox before next test
        for sb in sandboxes:
            try:
                await sb.kill()
            except Exception:
                pass
        sandboxes.clear()

        # ── Test: Build log streaming ──────────────────────────────
        bold("━━━ Test: Build log streaming (on_build_log callback) ━━━\n")

        log_image = Image.base().run_commands("echo 'build step 1'", "echo 'build step 2'")

        build_logs: list[str] = []
        dim("Creating sandbox with build log streaming...")
        sandbox5 = await Sandbox.create(
            image=log_image,
            timeout=300,
            on_build_log=lambda log: (build_logs.append(log), dim(f"  build: {log}")),
        )
        sandboxes.append(sandbox5)

        check("Build log callback was called", len(build_logs) > 0, f"got {len(build_logs)} logs")
        check("Sandbox created via SSE stream", sandbox5.status in ("running", "creating"))
        print()

        # Kill sandbox before next test
        for sb in sandboxes:
            try:
                await sb.kill()
            except Exception:
                pass
        sandboxes.clear()

        # ── Test: Snapshot build log streaming ──────────────────────
        bold("━━━ Test: Snapshot build log streaming (on_build_logs) ━━━\n")

        snapshot_log_image = Image.base().run_commands("echo 'snapshot build'")

        snapshot_logs: list[str] = []
        dim("Creating snapshot with build log streaming...")
        streamed_snapshot = await snapshots.create(
            name="test-streamed-py",
            image=snapshot_log_image,
            on_build_logs=lambda log: (snapshot_logs.append(log), dim(f"  snapshot build: {log}")),
        )

        check("Snapshot build log callback was called", len(snapshot_logs) > 0, f"got {len(snapshot_logs)} logs")
        check("Streamed snapshot has name", streamed_snapshot["name"] == "test-streamed-py")
        check("Streamed snapshot is ready", streamed_snapshot["status"] == "ready")

        await snapshots.delete("test-streamed-py")
        green("Cleaned up test-streamed-py snapshot")
        print()

    except Exception as err:
        red(f"Fatal error: {err}")
        import traceback
        traceback.print_exc()
        failed += 1
    finally:
        bold("━━━ Cleanup ━━━\n")
        for sb in sandboxes:
            try:
                await sb.kill()
                dim(f"Killed {sb.sandbox_id}")
            except Exception:
                pass
        try:
            await snapshots.delete("test-env-py")
        except Exception:
            pass
        try:
            await snapshots.delete("test-streamed-py")
        except Exception:
            pass

    bold("========================================")
    bold(f" Results: {passed} passed, {failed} failed")
    bold("========================================\n")
    if failed > 0:
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
