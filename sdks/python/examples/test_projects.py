#!/usr/bin/env python3
"""
Projects & Secrets Test

Tests:
  1. Create a project
  2. List projects
  3. Get project by ID
  4. Update project
  5. Set secrets on a project
  6. List secrets (names only, values never returned)
  7. Create sandbox with project (inherits config + secrets)
  8. Verify secrets are injected as env vars
  9. Delete secret
 10. Delete project

Usage:
  python examples/test_projects.py
"""

import asyncio
import sys
import time

from opencomputer import Sandbox, Project

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
    bold("║       Projects & Secrets Test                    ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    project_id = None
    sandbox = None
    project_name = f"test-project-{int(time.time())}"

    try:
        # ── Test 1: Create project ──
        bold("━━━ Test 1: Create project ━━━\n")

        project = await Project.create(
            name=project_name,
            template="base",
            cpu_count=1,
            memory_mb=512,
            timeout_sec=120,
        )

        project_id = project["id"]
        check("Project created", bool(project["id"]))
        check("Name matches", project["name"] == project_name)
        check("Template set", project["template"] == "base")
        check("CPU set", project["cpuCount"] == 1)
        check("Memory set", project["memoryMB"] == 512)
        check("Timeout set", project["timeoutSec"] == 120)
        dim(f"Project ID: {project_id}")
        print()

        # ── Test 2: List projects ──
        bold("━━━ Test 2: List projects ━━━\n")

        projects = await Project.list()
        check("List returns list", isinstance(projects, list))
        found = any(p["id"] == project_id for p in projects)
        check("Created project in list", found)
        dim(f"Total projects: {len(projects)}")
        print()

        # ── Test 3: Get project ──
        bold("━━━ Test 3: Get project by ID ━━━\n")

        fetched = await Project.get(project_id)
        check("Get returns correct project", fetched["id"] == project_id)
        check("Get has correct name", fetched["name"] == project_name)
        print()

        # ── Test 4: Update project ──
        bold("━━━ Test 4: Update project ━━━\n")

        updated = await Project.update(
            project_id,
            memory_mb=1024,
            timeout_sec=300,
        )
        check("Memory updated to 1024", updated["memoryMB"] == 1024)
        check("Timeout updated to 300", updated["timeoutSec"] == 300)
        check("Name unchanged", updated["name"] == project_name)
        print()

        # ── Test 5: Set secrets ──
        bold("━━━ Test 5: Set project secrets ━━━\n")

        await Project.set_secret(project_id, "TEST_API_KEY", "sk-test-12345")
        green("Set TEST_API_KEY")

        await Project.set_secret(project_id, "DATABASE_URL", "postgres://localhost/test")
        green("Set DATABASE_URL")

        await Project.set_secret(project_id, "TEMP_SECRET", "will-be-deleted")
        green("Set TEMP_SECRET")
        print()

        # ── Test 6: List secrets ──
        bold("━━━ Test 6: List secret names ━━━\n")

        secret_names = await Project.list_secrets(project_id)
        check("Returns list", isinstance(secret_names, list))
        check("Has TEST_API_KEY", "TEST_API_KEY" in secret_names)
        check("Has DATABASE_URL", "DATABASE_URL" in secret_names)
        check("Has TEMP_SECRET", "TEMP_SECRET" in secret_names)
        check("3 secrets total", len(secret_names) == 3, f"got {len(secret_names)}")
        dim(f"Secret names: {', '.join(secret_names)}")
        print()

        # ── Test 7: Create sandbox with project ──
        bold("━━━ Test 7: Create sandbox with project ━━━\n")

        sandbox = await Sandbox.create(
            project=project_name,
            timeout=120,
        )
        check("Sandbox created", bool(sandbox.sandbox_id))
        dim(f"Sandbox ID: {sandbox.sandbox_id}")
        print()

        # ── Test 8: Verify secrets are sealed in sandbox ──
        bold("━━━ Test 8: Verify secrets sealed in sandbox ━━━\n")

        # Secrets should be sealed tokens (osb_sealed_*) inside the VM.
        # The MITM proxy replaces sealed tokens with real values on outbound HTTPS requests,
        # so the real secret never exists in VM memory.
        api_key_result = await sandbox.commands.run("echo $TEST_API_KEY")
        api_key_val = api_key_result.stdout.strip()
        check("TEST_API_KEY is sealed", api_key_val.startswith("osb_sealed_"),
              f'got "{api_key_val}"')

        db_url_result = await sandbox.commands.run("echo $DATABASE_URL")
        db_url_val = db_url_result.stdout.strip()
        check("DATABASE_URL is sealed", db_url_val.startswith("osb_sealed_"),
              f'got "{db_url_val}"')

        temp_result = await sandbox.commands.run("echo $TEMP_SECRET")
        temp_val = temp_result.stdout.strip()
        check("TEMP_SECRET is sealed", temp_val.startswith("osb_sealed_"),
              f'got "{temp_val}"')
        print()

        # ── Test 9: Delete secret ──
        bold("━━━ Test 9: Delete secret ━━━\n")

        await Project.delete_secret(project_id, "TEMP_SECRET")
        green("Deleted TEMP_SECRET")

        after_delete = await Project.list_secrets(project_id)
        check("TEMP_SECRET removed", "TEMP_SECRET" not in after_delete)
        check("2 secrets remaining", len(after_delete) == 2, f"got {len(after_delete)}")
        print()

        # ── Test 10: Delete project ──
        bold("━━━ Test 10: Delete project ━━━\n")

        # Kill sandbox first
        await sandbox.kill()
        green("Sandbox killed")
        sandbox = None

        await Project.delete(project_id)
        green("Project deleted")

        # Verify it's gone
        try:
            await Project.get(project_id)
            red("Project should not exist after delete")
            failed += 1
        except Exception:
            green("Project not found after delete (expected)")
            passed += 1
        project_id = None
        print()

    except Exception as e:
        red(f"Fatal error: {e}")
        import traceback
        traceback.print_exc()
        failed += 1
    finally:
        # Cleanup
        if sandbox:
            try:
                await sandbox.kill()
            except Exception:
                pass
        if project_id:
            try:
                await Project.delete(project_id)
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
