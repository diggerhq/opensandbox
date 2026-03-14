#!/usr/bin/env python3
"""
Secret Stores & Secrets Test

Tests:
  1. Create a secret store
  2. List secret stores
  3. Get secret store by ID
  4. Update secret store
  5. Set secrets on a store
  6. List secrets (names only, values never returned)
  7. Create sandbox with secret store (inherits secrets)
  8. Verify secrets are injected as sealed env vars
  9. Delete secret
 10. Delete secret store

Usage:
  python examples/test_projects.py
"""

import asyncio
import sys
import time

from opencomputer import Sandbox, SecretStore

GREEN = "\033[32m"
RED = "\033[31m"
BOLD = "\033[1m"
DIM = "\033[2m"
RESET = "\033[0m"

passed = 0
failed = 0


def green(msg: str) -> None:
    print(f"{GREEN}\u2713 {msg}{RESET}")


def red(msg: str) -> None:
    print(f"{RED}\u2717 {msg}{RESET}")


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

    bold("\n\u2554\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2557")
    bold("\u2551       Secret Stores & Secrets Test               \u2551")
    bold("\u255a\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u2550\u255d\n")

    store_id = None
    sandbox = None
    store_name = f"test-store-{int(time.time())}"

    try:
        # -- Test 1: Create secret store --
        bold("--- Test 1: Create secret store ---\n")

        store = await SecretStore.create(
            name=store_name,
            egress_allowlist=["api.anthropic.com"],
        )

        store_id = store["id"]
        check("Store created", bool(store["id"]))
        check("Name matches", store["name"] == store_name)
        check("Egress allowlist set", len(store.get("egressAllowlist", [])) == 1)
        dim(f"Store ID: {store_id}")
        print()

        # -- Test 2: List secret stores --
        bold("--- Test 2: List secret stores ---\n")

        stores = await SecretStore.list()
        check("List returns list", isinstance(stores, list))
        found = any(s["id"] == store_id for s in stores)
        check("Created store in list", found)
        dim(f"Total stores: {len(stores)}")
        print()

        # -- Test 3: Get secret store --
        bold("--- Test 3: Get secret store by ID ---\n")

        fetched = await SecretStore.get(store_id)
        check("Get returns correct store", fetched["id"] == store_id)
        check("Get has correct name", fetched["name"] == store_name)
        print()

        # -- Test 4: Update secret store --
        bold("--- Test 4: Update secret store ---\n")

        updated_name = f"{store_name}-updated"
        updated = await SecretStore.update(
            store_id,
            name=updated_name,
            egress_allowlist=["api.anthropic.com", "*.openai.com"],
        )
        check("Name updated", updated["name"] == updated_name)
        check("Egress allowlist updated", len(updated.get("egressAllowlist", [])) == 2)
        print()

        # -- Test 5: Set secrets --
        bold("--- Test 5: Set secrets ---\n")

        await SecretStore.set_secret(store_id, "TEST_API_KEY", "sk-test-12345")
        green("Set TEST_API_KEY")

        await SecretStore.set_secret(store_id, "DATABASE_URL", "postgres://localhost/test")
        green("Set DATABASE_URL")

        await SecretStore.set_secret(store_id, "TEMP_SECRET", "will-be-deleted")
        green("Set TEMP_SECRET")
        print()

        # -- Test 6: List secrets --
        bold("--- Test 6: List secret entries ---\n")

        entries = await SecretStore.list_secrets(store_id)
        check("Returns list", isinstance(entries, list))
        names = [e["name"] for e in entries]
        check("Has TEST_API_KEY", "TEST_API_KEY" in names)
        check("Has DATABASE_URL", "DATABASE_URL" in names)
        check("Has TEMP_SECRET", "TEMP_SECRET" in names)
        check("3 secrets total", len(entries) == 3, f"got {len(entries)}")
        dim(f"Secret names: {', '.join(names)}")
        print()

        # -- Test 7: Create sandbox with secret store --
        bold("--- Test 7: Create sandbox with secret store ---\n")

        sandbox = await Sandbox.create(
            secret_store=updated_name,
            timeout=120,
        )
        check("Sandbox created", bool(sandbox.sandbox_id))
        dim(f"Sandbox ID: {sandbox.sandbox_id}")
        print()

        # -- Test 8: Verify secrets are sealed in sandbox --
        bold("--- Test 8: Verify secrets sealed in sandbox ---\n")

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

        # -- Test 9: Delete secret --
        bold("--- Test 9: Delete secret ---\n")

        await SecretStore.delete_secret(store_id, "TEMP_SECRET")
        green("Deleted TEMP_SECRET")

        after_delete = await SecretStore.list_secrets(store_id)
        after_names = [e["name"] for e in after_delete]
        check("TEMP_SECRET removed", "TEMP_SECRET" not in after_names)
        check("2 secrets remaining", len(after_delete) == 2, f"got {len(after_delete)}")
        print()

        # -- Test 10: Delete secret store --
        bold("--- Test 10: Delete secret store ---\n")

        # Kill sandbox first
        await sandbox.kill()
        green("Sandbox killed")
        sandbox = None

        await SecretStore.delete(store_id)
        green("Secret store deleted")

        # Verify it's gone
        try:
            await SecretStore.get(store_id)
            red("Store should not exist after delete")
            failed += 1
        except Exception:
            green("Store not found after delete (expected)")
            passed += 1
        store_id = None
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
        if store_id:
            try:
                await SecretStore.delete(store_id)
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
