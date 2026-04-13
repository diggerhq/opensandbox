#!/usr/bin/env python3
"""
Test secret store inheritance on fork / create_from_checkpoint.

Creates two secret stores, a sandbox with store A, checkpoints it,
then forks with store B attached. Also tests snapshot + secretStore path.
Verifies:
  1. Secret env vars from both stores are present (layered merge)
  2. Secrets are sealed in-guest (osb_sealed_* tokens, not plaintext)
  3. Actual secret VALUES resolve correctly through the proxy (httpbin echo)
  4. Store B's value wins on collision (SHARED_KEY override)
  5. Snapshot + secretStore path works end-to-end

Usage:
  OPENCOMPUTER_API_URL=... OPENCOMPUTER_API_KEY=... python examples/test_secret_store_fork.py
"""

import asyncio
import os
import sys
import time

from opencomputer import Sandbox, SecretStore, Snapshots, Image

GREEN = "\033[32m"
RED = "\033[31m"
BOLD = "\033[1m"
DIM = "\033[2m"
RESET = "\033[0m"

passed = 0
failed = 0


def green(msg: str) -> None:
    print(f"{GREEN}\u2713 {msg}{RESET}")


def red(msg: str, detail: str = "") -> None:
    suffix = f" \u2014 {detail}" if detail else ""
    print(f"{RED}\u2717 {msg}{suffix}{RESET}")


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
        red(desc, detail)
        failed += 1


async def main() -> None:
    global passed, failed

    suffix = int(time.time() * 1000)
    store_a_name = f"fork-test-a-{suffix}"
    store_b_name = f"fork-test-b-{suffix}"

    # Random-ish values for httpbin verification
    git_token_val = f"git_{os.urandom(12).hex()}"
    shared_key_a_val = f"shared_a_{os.urandom(12).hex()}"
    shared_key_b_val = f"shared_b_{os.urandom(12).hex()}"
    api_key_val = f"api_{os.urandom(12).hex()}"

    store_a_id: str | None = None
    store_b_id: str | None = None
    base_sandbox: Sandbox | None = None
    forked_sandbox: Sandbox | None = None
    snapshot_sandbox: Sandbox | None = None
    snapshot_name: str | None = None

    try:
        # -- Setup: two secret stores -----------------------------------------
        bold("\n=== 1. Setup: create two secret stores ===\n")

        store_a = await SecretStore.create(
            name=store_a_name,
            egress_allowlist=["github.com", "httpbin.org"],
        )
        store_a_id = store_a["id"]
        print(f"  Store A: {store_a_id} ({store_a_name})")

        await SecretStore.set_secret(store_a_id, "GIT_TOKEN", git_token_val,
                                     allowed_hosts=["github.com", "httpbin.org"])
        await SecretStore.set_secret(store_a_id, "SHARED_KEY", shared_key_a_val,
                                     allowed_hosts=["httpbin.org"])
        dim(f"GIT_TOKEN = {git_token_val}")
        dim(f"SHARED_KEY(A) = {shared_key_a_val}")
        green("Store A created: GIT_TOKEN + SHARED_KEY, egress=[github.com, httpbin.org]")

        store_b = await SecretStore.create(
            name=store_b_name,
            egress_allowlist=["api.anthropic.com", "httpbin.org"],
        )
        store_b_id = store_b["id"]
        print(f"  Store B: {store_b_id} ({store_b_name})")

        await SecretStore.set_secret(store_b_id, "API_KEY", api_key_val,
                                     allowed_hosts=["api.anthropic.com", "httpbin.org"])
        await SecretStore.set_secret(store_b_id, "SHARED_KEY", shared_key_b_val,
                                     allowed_hosts=["httpbin.org"])
        dim(f"API_KEY = {api_key_val}")
        dim(f"SHARED_KEY(B) = {shared_key_b_val} (should override A)")
        green("Store B created: API_KEY + SHARED_KEY(override), egress=[api.anthropic.com, httpbin.org]")

        # -- Layer 1: sandbox with store A -------------------------------------
        bold("\n=== 2. Create base sandbox with store A ===\n")

        base_sandbox = await Sandbox.create(secret_store=store_a_name, timeout=120)
        print(f"  Base sandbox: {base_sandbox.sandbox_id}")
        await asyncio.sleep(5)

        env_base = (await base_sandbox.exec.run("env")).stdout
        check("Base has GIT_TOKEN", "GIT_TOKEN" in env_base)
        check("Base has SHARED_KEY", "SHARED_KEY" in env_base)
        check("Base does NOT have API_KEY", "API_KEY" not in env_base)

        # Verify sealed
        base_git_echo = (await base_sandbox.exec.run('printf %s "$GIT_TOKEN"')).stdout.strip()
        check(
            "Base: GIT_TOKEN is sealed (not plaintext)",
            base_git_echo.startswith("osb_sealed_") and git_token_val not in base_git_echo,
            f'got "{base_git_echo[:40]}..."',
        )

        # Verify values via httpbin
        base_httpbin = (await base_sandbox.exec.run(
            'curl -sS -m 10 https://httpbin.org/headers '
            '-H "X-Git: $GIT_TOKEN" -H "X-Shared: $SHARED_KEY"'
        )).stdout
        check("Base: GIT_TOKEN value resolves via httpbin",
              git_token_val in base_httpbin,
              f"httpbin did not echo back {git_token_val}")
        check("Base: SHARED_KEY(A) value resolves via httpbin",
              shared_key_a_val in base_httpbin,
              f"httpbin did not echo back {shared_key_a_val}")

        # -- Checkpoint --------------------------------------------------------
        bold("\n=== 3. Create checkpoint from base ===\n")

        cp = await base_sandbox.create_checkpoint("fork-test-cp")
        print(f"  Checkpoint: {cp['id']}")

        for _ in range(30):
            cps = await base_sandbox.list_checkpoints()
            found = next((c for c in cps if c["id"] == cp["id"]), None)
            if found and found.get("status") == "ready":
                break
            await asyncio.sleep(2)
        green("Checkpoint ready")

        # -- Layer 2: fork with store B ----------------------------------------
        bold("\n=== 4. Fork from checkpoint with store B (secretStore inheritance) ===\n")

        forked_sandbox = await Sandbox.create_from_checkpoint(
            cp["id"],
            secret_store=store_b_name,
            timeout=120,
        )
        print(f"  Forked sandbox: {forked_sandbox.sandbox_id}")
        await asyncio.sleep(5)

        # -- 4a. Check env vars exist ------------------------------------------
        bold("\n=== 4a. Verify secret env vars present ===\n")

        env_fork = (await forked_sandbox.exec.run("env")).stdout
        check("Fork has GIT_TOKEN (inherited from store A)", "GIT_TOKEN" in env_fork)
        check("Fork has API_KEY (from store B)", "API_KEY" in env_fork)
        check("Fork has SHARED_KEY (merged)", "SHARED_KEY" in env_fork)
        check("Fork has HTTP_PROXY (secrets proxy active)", "HTTP_PROXY" in env_fork)

        sealed_count = env_fork.count("osb_sealed_")
        check(
            f"Fork has 3 sealed secrets (got {sealed_count})",
            sealed_count == 3,
            "expected GIT_TOKEN(A) + SHARED_KEY(B override) + API_KEY(B)",
        )

        # -- 4b. Verify secrets are sealed -------------------------------------
        bold("\n=== 4b. Verify secrets are sealed in-guest ===\n")

        fork_git_echo = (await forked_sandbox.exec.run('printf %s "$GIT_TOKEN"')).stdout.strip()
        check("Fork: GIT_TOKEN is sealed",
              fork_git_echo.startswith("osb_sealed_") and git_token_val not in fork_git_echo,
              f'got "{fork_git_echo[:40]}..."')

        fork_api_echo = (await forked_sandbox.exec.run('printf %s "$API_KEY"')).stdout.strip()
        check("Fork: API_KEY is sealed",
              fork_api_echo.startswith("osb_sealed_") and api_key_val not in fork_api_echo,
              f'got "{fork_api_echo[:40]}..."')

        fork_shared_echo = (await forked_sandbox.exec.run('printf %s "$SHARED_KEY"')).stdout.strip()
        check("Fork: SHARED_KEY is sealed",
              fork_shared_echo.startswith("osb_sealed_") and shared_key_b_val not in fork_shared_echo,
              f'got "{fork_shared_echo[:40]}..."')

        # -- 4c. Verify actual VALUES via httpbin ------------------------------
        bold("\n=== 4c. Verify actual secret values via httpbin (proxy substitution) ===\n")

        fork_httpbin = (await forked_sandbox.exec.run(
            'curl -sS -m 10 https://httpbin.org/headers '
            '-H "X-Git: $GIT_TOKEN" '
            '-H "X-Api: $API_KEY" '
            '-H "X-Shared: $SHARED_KEY"'
        )).stdout
        dim(f"httpbin response (first 500 chars):")
        dim(fork_httpbin.replace("\n", " ")[:500])

        check("Fork: GIT_TOKEN value correct (inherited from A)",
              git_token_val in fork_httpbin,
              f"httpbin did not echo back {git_token_val}")
        check("Fork: API_KEY value correct (from B)",
              api_key_val in fork_httpbin,
              f"httpbin did not echo back {api_key_val}")
        check("Fork: SHARED_KEY has B's value (B override wins on collision)",
              shared_key_b_val in fork_httpbin,
              f"httpbin did not echo back B's value {shared_key_b_val}")
        check("Fork: SHARED_KEY does NOT have A's value (overridden by B)",
              shared_key_a_val not in fork_httpbin,
              f"httpbin echoed A's value {shared_key_a_val} -- override did not work")

        # -- 5. Snapshot/template path -----------------------------------------
        bold("\n=== 5. Create snapshot template, fork with secretStore ===\n")

        snapshots = Snapshots()
        snapshot_name = f"fork-secret-test-{suffix}"
        dim(f'Creating snapshot "{snapshot_name}" from base image...')
        await snapshots.create(
            name=snapshot_name,
            image=Image.base().run_commands("echo 'template-ready' > /home/sandbox/marker.txt"),
        )

        for _ in range(30):
            info = await snapshots.get(snapshot_name)
            if info.get("status") == "ready":
                break
            await asyncio.sleep(2)
        green(f'Snapshot "{snapshot_name}" ready')

        snapshot_sandbox = await Sandbox.create(
            snapshot=snapshot_name,
            secret_store=store_b_name,
            timeout=120,
        )
        print(f"  Snapshot-forked sandbox: {snapshot_sandbox.sandbox_id}")
        await asyncio.sleep(5)

        # Verify template content
        marker = (await snapshot_sandbox.exec.run("cat /home/sandbox/marker.txt")).stdout.strip()
        check("Snapshot fork: template content present (/home/sandbox/marker.txt)",
              marker == "template-ready",
              f'got "{marker}"')

        # Verify secrets
        env_snap = (await snapshot_sandbox.exec.run("env")).stdout
        check("Snapshot fork: has API_KEY (from store B)", "API_KEY" in env_snap)
        check("Snapshot fork: has SHARED_KEY (from store B)", "SHARED_KEY" in env_snap)
        check("Snapshot fork: has HTTP_PROXY (secrets proxy active)", "HTTP_PROXY" in env_snap)

        snap_api_echo = (await snapshot_sandbox.exec.run('printf %s "$API_KEY"')).stdout.strip()
        check("Snapshot fork: API_KEY is sealed",
              snap_api_echo.startswith("osb_sealed_") and api_key_val not in snap_api_echo,
              f'got "{snap_api_echo[:40]}..."')

        # Verify values via httpbin
        snap_httpbin = (await snapshot_sandbox.exec.run(
            'curl -sS -m 10 https://httpbin.org/headers '
            '-H "X-Api: $API_KEY" '
            '-H "X-Shared: $SHARED_KEY"'
        )).stdout
        dim(f"httpbin response (first 500 chars):")
        dim(snap_httpbin.replace("\n", " ")[:500])

        check("Snapshot fork: API_KEY value correct via httpbin",
              api_key_val in snap_httpbin,
              f"httpbin did not echo back {api_key_val}")
        check("Snapshot fork: SHARED_KEY has B's value via httpbin",
              shared_key_b_val in snap_httpbin,
              f"httpbin did not echo back {shared_key_b_val}")

        # -- Summary -----------------------------------------------------------
        bold(f"\n=== Results: {passed} passed, {failed} failed ===\n")

    finally:
        bold("=== Cleanup ===")
        if snapshot_sandbox:
            try:
                await snapshot_sandbox.kill()
            except Exception:
                pass
        if forked_sandbox:
            try:
                await forked_sandbox.kill()
            except Exception:
                pass
        if base_sandbox:
            try:
                await base_sandbox.kill()
            except Exception:
                pass
        if snapshot_name:
            try:
                s = Snapshots()
                await s.delete(snapshot_name)
            except Exception:
                pass
        if store_b_id:
            try:
                await SecretStore.delete(store_b_id)
            except Exception:
                pass
        if store_a_id:
            try:
                await SecretStore.delete(store_a_id)
            except Exception:
                pass
        green("Cleanup done")

    sys.exit(1 if failed > 0 else 0)


if __name__ == "__main__":
    asyncio.run(main())
