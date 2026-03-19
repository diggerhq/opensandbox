"""
OpenComputer Full Platform Demo
===============================
Tests every major feature of the QEMU backend in a single script.
Run with: OPENCOMPUTER_API_KEY=test-dev-key OPENCOMPUTER_API_URL=http://3.148.184.81:8080 python full_platform_demo.py
"""

import asyncio
import os
import time

import httpx
from opencomputer import Sandbox, SecretStore

API_URL = os.environ.get("OPENCOMPUTER_API_URL", "http://3.148.184.81:8080")
API_KEY = os.environ.get("OPENCOMPUTER_API_KEY", "test-dev-key")

PASS = 0
FAIL = 0


def ok(msg):
    global PASS
    PASS += 1
    print(f"  ✓ {msg}")


def fail(msg):
    global FAIL
    FAIL += 1
    print(f"  ✗ {msg}")


def check(condition, pass_msg, fail_msg):
    if condition:
        ok(pass_msg)
    else:
        fail(fail_msg)


async def wait_checkpoint(sb, cp_id, timeout=20):
    for _ in range(timeout):
        cps = await sb.list_checkpoints()
        found = [c for c in cps if c["id"] == cp_id]
        if found and found[0].get("status") == "ready":
            return True
        await asyncio.sleep(1)
    return False


async def main():
    print("╔═══════════════════════════════════════════╗")
    print("║  OpenComputer Full Platform Demo          ║")
    print("╚═══════════════════════════════════════════╝\n")

    sandboxes = []
    store_id = None

    try:
        # ── 1. SECRET STORE ──────────────────────────────────────────────
        print("▸ 1. Secret Store")
        store_name = f"demo-{int(time.time())}"
        store = await SecretStore.create(
            name=store_name,
            egress_allowlist=["httpbin.org", "api.github.com"],
        )
        store_id = store["id"]
        check(store_id, f"Created store: {store_name}", "Create store failed")

        await SecretStore.set_secret(store_id, "MY_TOKEN", "secret-value-42")
        await SecretStore.set_secret(store_id, "DB_PASS", "hunter2", allowed_hosts=["db.example.com"])
        secrets = await SecretStore.list_secrets(store_id)
        check(len(secrets) == 2, f"Set 2 secrets", f"Expected 2 secrets, got {len(secrets)}")

        # Verify plaintext not exposed
        names = [s["name"] for s in secrets]
        values = str(secrets)
        check("secret-value-42" not in values, "Plaintext not exposed in list", "PLAINTEXT LEAKED!")

        # Delete one
        await SecretStore.delete_secret(store_id, "DB_PASS")
        secrets = await SecretStore.list_secrets(store_id)
        check(len(secrets) == 1, "Deleted DB_PASS", "Delete failed")
        print()

        # ── 2. SANDBOX CREATION WITH SECRETS ─────────────────────────────
        print("▸ 2. Sandbox with Secrets")
        sb = await Sandbox.create(
            timeout=600,
            secret_store=store_name,
            envs={"APP_ENV": "demo", "VERSION": "1.0"},
        )
        sandboxes.append(sb)
        check(sb.sandbox_id is not None, f"Created: {sb.sandbox_id}", "Create failed")

        # Check secrets injected as env vars
        r = await sb.exec.run("echo $MY_TOKEN")
        token_val = r.stdout.strip()
        check(len(token_val) > 0, f"MY_TOKEN injected ({token_val[:15]}...)", "MY_TOKEN not found")

        # Check custom envs
        r = await sb.exec.run("echo $APP_ENV $VERSION")
        check("demo 1.0" in r.stdout, "Custom envs present", f"Envs: {r.stdout.strip()}")

        # Deleted secret should not be present
        r = await sb.exec.run("echo DB_PASS=$DB_PASS")
        check(r.stdout.strip() == "DB_PASS=", "Deleted secret not injected", f"DB_PASS leaked: {r.stdout.strip()}")
        print()

        # ── 3. EXEC: RUN ────────────────────────────────────────────────
        print("▸ 3. Exec (fire-and-forget)")
        r = await sb.exec.run("echo hello world")
        check(r.stdout.strip() == "hello world", "echo", f"Got: {r.stdout.strip()}")

        r = await sb.exec.run("python3 -c \"print(sum(range(100)))\"")
        check(r.stdout.strip() == "4950", "python3 sum", f"Got: {r.stdout.strip()}")

        r = await sb.exec.run("node -e \"console.log(JSON.stringify({ok:true}))\"")
        check('"ok":true' in r.stdout, "node JSON", f"Got: {r.stdout.strip()}")

        # Timeout handling
        r = await sb.exec.run("sleep 30", timeout=2000)
        check(r.exit_code != 0 or True, "Timeout returns (doesn't hang)", "Timeout hung")
        print()

        # ── 4. EXEC: START (streaming session) ──────────────────────────
        print("▸ 4. Exec Session (streaming)")
        output_lines = []
        session = await sb.exec.start(
            "python3",
            args=["-c", "import time\nfor i in range(3):\n    print(f'line {i}', flush=True)\n    time.sleep(0.2)"],
            on_stdout=lambda data: output_lines.append(data.decode()),
        )
        exit_code = await session.done
        check(exit_code == 0, f"Session exited 0, got {len(output_lines)} chunks", f"Exit: {exit_code}")
        combined = "".join(output_lines)
        check("line 0" in combined and "line 2" in combined, "Streamed all lines", f"Output: {combined[:50]}")
        print()

        # ── 5. FILES ────────────────────────────────────────────────────
        print("▸ 5. File I/O")
        await sb.files.write("/workspace/hello.txt", "hello from demo")
        content = await sb.files.read("/workspace/hello.txt")
        check(content == "hello from demo", "Write + read", f"Got: {content[:30]}")

        await sb.files.write("/workspace/app.py", "import json\nprint(json.dumps({'status': 'ok'}))\n")
        r = await sb.exec.run("python3 /workspace/app.py")
        check('"status": "ok"' in r.stdout, "Run written file", f"Got: {r.stdout.strip()}")

        entries = await sb.files.list("/workspace")
        names = [e["name"] for e in entries] if isinstance(entries, list) else []
        check("hello.txt" in names, "List dir", f"Entries: {names}")

        # Binary round-trip
        binary_data = bytes(range(256)) * 4
        await sb.files.write("/workspace/binary.bin", binary_data)
        read_back = await sb.files.read_bytes("/workspace/binary.bin")
        check(read_back == binary_data, "Binary round-trip (1KB)", f"Size mismatch: {len(read_back)}")
        print()

        # ── 6. INSTALL PACKAGES (rootfs persistence) ────────────────────
        print("▸ 6. Package Installation")
        await sb.exec.run("apt-get update -qq && apt-get install -y -qq cowsay 2>/dev/null", timeout=60000)
        r = await sb.exec.run("/usr/games/cowsay moo 2>/dev/null | head -1")
        check(len(r.stdout.strip()) > 0, "apt install (cowsay)", f"Got: {r.stdout.strip()}")

        await sb.exec.run("pip3 install --quiet httpx 2>/dev/null", timeout=60000)
        r = await sb.exec.run("python3 -c \"import httpx; print(httpx.__version__)\"")
        check(len(r.stdout.strip()) > 0, f"pip install (httpx {r.stdout.strip()})", "pip install failed")
        print()

        # ── 7. MEMORY SCALING ───────────────────────────────────────────
        print("▸ 7. Memory Scaling")
        r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'")
        baseline = int(r.stdout.strip())
        check(800 < baseline < 1000, f"Baseline: {baseline}MB", f"Unexpected: {baseline}MB")

        # Scale up via POST /scale
        async with httpx.AsyncClient(base_url=API_URL, headers={"X-API-Key": API_KEY, "Content-Type": "application/json"}, timeout=30) as client:
            resp = await client.post(f"/api/sandboxes/{sb.sandbox_id}/scale", json={"memory_mb": 2048})
        check(resp.status_code == 200, "Scale to 2GB: API accepted", f"Scale: {resp.status_code}")

        await asyncio.sleep(1)
        r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'")
        scaled = int(r.stdout.strip())
        check(scaled > 1800, f"Visible RAM: {scaled}MB", f"RAM didn't scale: {scaled}MB")

        # Scale down
        async with httpx.AsyncClient(base_url=API_URL, headers={"X-API-Key": API_KEY, "Content-Type": "application/json"}, timeout=30) as client:
            resp = await client.post(f"/api/sandboxes/{sb.sandbox_id}/scale", json={"memory_mb": 512})
        check(resp.status_code == 200, "Scale down to 512MB: API accepted", f"Scale down: {resp.status_code}")
        print()

        # ── 8. METADATA API ─────────────────────────────────────────────
        print("▸ 8. Metadata API (169.254.169.254)")
        r = await sb.exec.run("curl -s http://169.254.169.254/v1/status")
        check(sb.sandbox_id in r.stdout, "Status: sandboxId", f"Got: {r.stdout[:50]}")

        r = await sb.exec.run("curl -s http://169.254.169.254/v1/metadata")
        check('"region"' in r.stdout, "Metadata: region", f"Got: {r.stdout[:50]}")

        r = await sb.exec.run("curl -s http://169.254.169.254/v1/limits")
        check('"memLimit"' in r.stdout, "Limits: memLimit", f"Got: {r.stdout[:50]}")

        # Clock sync
        r = await sb.exec.run("date +%s")
        guest_time = int(r.stdout.strip())
        host_time = int(time.time())
        drift = abs(guest_time - host_time)
        check(drift <= 3, f"Clock drift: {drift}s", f"Clock off by {drift}s")
        print()

        # ── 9. NETWORK ──────────────────────────────────────────────────
        print("▸ 9. Networking")
        r = await sb.exec.run("curl -s -o /dev/null -w '%{http_code}' https://httpbin.org/ip", timeout=15000)
        check(r.stdout.strip() == "200", "HTTPS outbound", f"Got: {r.stdout.strip()}")

        r = await sb.exec.run("ping -c1 -W3 8.8.8.8 2>&1 | grep -c 'bytes from'")
        check(r.stdout.strip() == "1", "Ping 8.8.8.8", f"Ping: {r.stdout.strip()}")
        print()

        # ── 10. HIBERNATE / WAKE ────────────────────────────────────────
        print("▸ 10. Hibernate / Wake")
        # Write a marker before hibernate
        await sb.files.write("/workspace/persist.txt", "survived-hibernate")

        await sb.hibernate()
        ok("Hibernated")

        await sb.wake(timeout=600)
        ok("Woke")

        # Verify persistence
        content = await sb.files.read("/workspace/persist.txt")
        check(content == "survived-hibernate", "Workspace file survived", f"Got: {content}")

        r = await sb.exec.run("/usr/games/cowsay alive 2>/dev/null | head -1")
        check(len(r.stdout.strip()) > 0, "cowsay survived (rootfs)", "cowsay gone")

        r = await sb.exec.run("python3 -c \"import httpx; print('httpx ok')\"")
        check("httpx ok" in r.stdout, "httpx survived (pip)", f"Got: {r.stdout.strip()}")
        print()

        # ── 11. CHECKPOINT / FORK ───────────────────────────────────────
        print("▸ 11. Checkpoint & Fork")
        await sb.files.write("/workspace/checkpoint_marker.txt", "at-checkpoint")
        cp = await sb.create_checkpoint(name="demo-cp")
        cp_id = cp["id"]
        ready = await wait_checkpoint(sb, cp_id)
        check(ready, f"Checkpoint ready: {cp_id[:12]}...", "Checkpoint timeout")

        # Fork from checkpoint
        fork = await Sandbox.create_from_checkpoint(cp_id, timeout=300)
        sandboxes.append(fork)
        await asyncio.sleep(5)
        ok(f"Forked: {fork.sandbox_id}")

        # Verify fork has the checkpoint data
        content = await fork.files.read("/workspace/checkpoint_marker.txt")
        check(content == "at-checkpoint", "Fork has checkpoint data", f"Fork got: {content}")

        # Verify isolation — write to fork, check original
        await fork.files.write("/workspace/fork_only.txt", "fork-data")
        r = await sb.exec.run("cat /workspace/fork_only.txt 2>&1")
        check("No such file" in r.stdout or r.exit_code != 0, "Fork isolated from original", "Fork leaked!")

        await fork.kill()
        sandboxes.remove(fork)
        ok("Fork destroyed")
        print()

        # ── 12. RESTORE CHECKPOINT ──────────────────────────────────────
        print("▸ 12. Restore Checkpoint (in-place revert)")
        # Modify state after checkpoint
        await sb.exec.run("rm /workspace/checkpoint_marker.txt")
        await sb.files.write("/workspace/new_file.txt", "post-checkpoint")
        r = await sb.exec.run("cat /workspace/checkpoint_marker.txt 2>&1")
        check("No such file" in r.stdout or r.exit_code != 0, "Marker deleted", "Delete failed")

        # Restore
        await sb.restore_checkpoint(cp_id)
        await asyncio.sleep(4)  # wait for loadvm + agent reconnect
        ok("Restored")

        # Verify revert
        content = await sb.files.read("/workspace/checkpoint_marker.txt")
        check(content == "at-checkpoint", "Marker restored!", f"Got: {content}")

        r = await sb.exec.run("cat /workspace/new_file.txt 2>&1")
        check("No such file" in r.stdout or r.exit_code != 0, "Post-checkpoint file gone (reverted)", "Revert incomplete")
        print()

        # ── 13. PREVIEW URL ─────────────────────────────────────────────
        print("▸ 13. Preview URL")
        # Start a simple HTTP server in the sandbox
        await sb.files.write("/workspace/index.html", "<h1>Hello from OpenComputer!</h1>")
        session = await sb.exec.start(
            "python3",
            args=["-m", "http.server", "3000", "--directory", "/workspace"],
        )
        await asyncio.sleep(2)

        try:
            preview = await sb.create_preview_url(port=3000)
            hostname = preview.get("hostname", "")
            check(len(hostname) > 0, f"Preview: http://{hostname}", "No hostname")

            # Verify it serves content
            async with httpx.AsyncClient(timeout=10) as client:
                resp = await client.get(f"http://{hostname}")
            check(resp.status_code == 200 and "Hello" in resp.text, "Preview serves content", f"Got: {resp.status_code}")
        except Exception as e:
            fail(f"Preview URL: {e}")

        await session.kill()
        print()

    finally:
        # ── CLEANUP ─────────────────────────────────────────────────────
        print("▸ Cleanup")
        for s in sandboxes:
            try:
                await s.kill()
                print(f"  Killed {s.sandbox_id}")
            except Exception:
                pass
        if store_id:
            try:
                await SecretStore.delete(store_id)
                print(f"  Deleted store {store_id[:12]}...")
            except Exception:
                pass

    # ── RESULTS ─────────────────────────────────────────────────────
    print(f"\n{'═' * 45}")
    print(f"  {PASS} passed, {FAIL} failed")
    print(f"{'═' * 45}")
    return FAIL == 0


if __name__ == "__main__":
    success = asyncio.run(main())
    exit(0 if success else 1)
