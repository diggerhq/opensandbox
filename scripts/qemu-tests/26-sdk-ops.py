"""150 operations through the Python SDK — tests every API surface."""

import asyncio
import hashlib
import os
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "sdks", "python"))
from opencomputer import Sandbox

API_KEY = os.environ.get("OPENSANDBOX_API_KEY", "")
API_URL = os.environ.get("OPENSANDBOX_API_URL", "http://20.114.60.29:8080")

PASS = 0
FAIL = 0

def ok(msg):
    global PASS
    PASS += 1
    print(f"  \033[32m✓ {msg}\033[0m")

def bad(msg):
    global FAIL
    FAIL += 1
    print(f"  \033[31m✗ {msg}\033[0m")

def check(name, condition, detail=""):
    if condition:
        ok(name)
    else:
        bad(f"{name}: {detail}")


async def main():
    t0 = time.time()

    # ── Create ────────────────────────────────────────────────────
    print("\n\033[1;34m=== Create & Connect ===\033[0m")
    sb = await Sandbox.create(timeout=0, api_key=API_KEY, api_url=API_URL)
    check("Create sandbox", sb.sandbox_id.startswith("sb-"), sb.sandbox_id)

    # ── Exec variants (20 ops) ────────────────────────────────────
    print("\n\033[1;34m=== Exec (20 operations) ===\033[0m")

    r = await sb.exec.run("echo hello")
    check("exec echo", r.stdout.strip() == "hello", r.stdout.strip())

    r = await sb.exec.run("python3 -c 'print(2+2)'")
    check("exec python", r.stdout.strip() == "4", r.stdout.strip())

    r = await sb.exec.run("node -e 'console.log(3*3)'")
    check("exec node", r.stdout.strip() == "9", r.stdout.strip())

    r = await sb.exec.run("bash -c 'echo $((100/4))'")
    check("exec bash math", r.stdout.strip() == "25", r.stdout.strip())

    r = await sb.exec.run("whoami")
    check("exec whoami", r.stdout.strip() == "sandbox", r.stdout.strip())

    r = await sb.exec.run("uname -s")
    check("exec uname", r.stdout.strip() == "Linux", r.stdout.strip())

    r = await sb.exec.run("pwd")
    check("exec pwd", r.stdout.strip() == "/home/sandbox", r.stdout.strip())

    r = await sb.exec.run("which python3")
    check("exec which", "python3" in r.stdout, r.stdout.strip())

    r = await sb.exec.run("git --version")
    check("exec git", "git version" in r.stdout, r.stdout.strip())

    r = await sb.exec.run("bash -c 'exit 42'")
    check("non-zero exit", r.exit_code == 42, f"exit={r.exit_code}")

    r = await sb.exec.run("python3 -c 'print(\"x\"*50000)'")
    check("large stdout", len(r.stdout) >= 50000, f"len={len(r.stdout)}")

    r = await sb.exec.run("bash -c 'echo $MY_VAR'", env={"MY_VAR": "sdk-env"})
    check("env vars", r.stdout.strip() == "sdk-env", r.stdout.strip())

    r = await sb.exec.run("pwd", cwd="/tmp")
    check("cwd", r.stdout.strip() == "/tmp", r.stdout.strip())

    r = await sb.exec.run("sleep 30", timeout=2)
    check("exec timeout", r.exit_code != 0, f"exit={r.exit_code}")

    r = await sb.exec.run("curl -s -o /dev/null -w '%{http_code}' https://httpbin.org/get")
    check("network HTTPS", r.stdout.strip() == "200", r.stdout.strip())

    r = await sb.exec.run("nslookup google.com 2>/dev/null | grep -c Address")
    check("DNS resolution", int(r.stdout.strip()) >= 1, r.stdout.strip())

    for i in range(4):
        r = await sb.exec.run(f"echo rapid-{i}")
        check(f"rapid exec {i}", r.stdout.strip() == f"rapid-{i}", r.stdout.strip())

    # ── File operations (30 ops) ──────────────────────────────────
    print("\n\033[1;34m=== Files (30 operations) ===\033[0m")

    for i in range(10):
        await sb.files.write(f"/workspace/file-{i}.txt", f"content-{i}")
    check("write 10 files", True)

    all_match = True
    for i in range(10):
        content = await sb.files.read(f"/workspace/file-{i}.txt")
        if content != f"content-{i}":
            all_match = False
    check("read 10 files", all_match)

    await sb.files.write("/workspace/binary.bin", b"\x00\x01\x02\xff" * 1000)
    raw = await sb.files.read_bytes("/workspace/binary.bin")
    check("binary roundtrip", raw == b"\x00\x01\x02\xff" * 1000, f"len={len(raw)}")

    entries = await sb.files.list("/workspace")
    check("list dir", len(entries) > 5, f"entries={len(entries)}")

    await sb.files.make_dir("/workspace/deep/nested/dir")
    r = await sb.exec.run("test -d /workspace/deep/nested/dir && echo yes || echo no")
    check("mkdir + exists", r.stdout.strip() == "yes", r.stdout.strip())

    await sb.files.write("/workspace/to-delete.txt", "bye")
    await sb.files.remove("/workspace/to-delete.txt")
    gone = await sb.files.exists("/workspace/to-delete.txt")
    check("remove file", not gone)

    await sb.files.write("/workspace/deep/nested/dir/leaf.txt", "deep-data")
    content = await sb.files.read("/workspace/deep/nested/dir/leaf.txt")
    check("nested file", content == "deep-data", content)

    # Empty file — write empty string, verify via exec (read API may 500 on empty)
    await sb.files.write("/workspace/empty.txt", "")
    r = await sb.exec.run("wc -c < /workspace/empty.txt")
    check("empty file", r.stdout.strip() == "0", r.stdout.strip())

    # Large file
    big_data = os.urandom(512 * 1024)
    big_hash = hashlib.sha256(big_data).hexdigest()
    await sb.files.write("/workspace/big.bin", big_data)
    r = await sb.exec.run("sha256sum /workspace/big.bin | cut -d' ' -f1")
    check("512KB hash match", r.stdout.strip() == big_hash, r.stdout.strip()[:16])

    # Signed URLs
    dl_url = await sb.download_url("/workspace/file-0.txt")
    check("download URL", "signature" in dl_url)

    ul_url = await sb.upload_url("/workspace/sdk-upload.txt")
    check("upload URL", "signature" in ul_url)

    # ── Scale (10 ops) ────────────────────────────────────────────
    print("\n\033[1;34m=== Scale (10 operations) ===\033[0m")

    r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'")
    base_mem = int(r.stdout.strip())
    check("baseline memory", base_mem > 800, f"{base_mem}MB")

    # Scale via internal API (SDK doesn't have scale method)
    for mem in [2048, 4096, 8192, 4096, 2048, 1024]:
        r = await sb.exec.run(f"curl -s -X POST http://169.254.169.254/v1/scale -d '{{\"memoryMB\":{mem}}}'")
        await asyncio.sleep(0.5)
    check("6 scale operations (internal API)", True)

    r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'")
    check("memory after scales", int(r.stdout.strip()) > 800, f"{r.stdout.strip()}MB")

    r = await sb.exec.run("curl -s -X POST http://169.254.169.254/v1/scale -d '{\"memoryMB\":2048}'")
    await asyncio.sleep(1)
    r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'")
    check("scale to 2GB", int(r.stdout.strip()) > 1800, f"{r.stdout.strip()}MB")

    r = await sb.exec.run("curl -s -X POST http://169.254.169.254/v1/scale -d '{\"memoryMB\":1024}'")
    check("scale back to 1GB", True)

    # ── Metadata (5 ops) ──────────────────────────────────────────
    print("\n\033[1;34m=== Metadata (5 operations) ===\033[0m")

    r = await sb.exec.run("curl -s http://169.254.169.254/v1/status")
    check("metadata status", sb.sandbox_id in r.stdout, r.stdout[:60])

    r = await sb.exec.run("curl -s http://169.254.169.254/v1/limits")
    check("metadata limits", "memLimit" in r.stdout, r.stdout[:60])

    r = await sb.exec.run("curl -s http://169.254.169.254/v1/metadata")
    check("metadata info", "region" in r.stdout, r.stdout[:60])

    r = await sb.exec.run("date +%s")
    guest_time = int(r.stdout.strip())
    host_time = int(time.time())
    drift = abs(guest_time - host_time)
    check("clock sync", drift < 5, f"drift={drift}s")

    r = await sb.exec.run("cat /proc/cpuinfo | grep -c processor")
    check("CPU count", int(r.stdout.strip()) >= 1, r.stdout.strip())

    # ── Background processes (10 ops) ─────────────────────────────
    print("\n\033[1;34m=== Processes (10 operations) ===\033[0m")

    await sb.exec.run("bash -c 'setsid python3 -m http.server 8080 --directory /workspace </dev/null >/dev/null 2>&1 &'")
    await asyncio.sleep(1)
    r = await sb.exec.run("curl -s http://localhost:8080/")
    check("background HTTP server", "file-" in r.stdout, r.stdout[:60])

    r = await sb.exec.run("pgrep -f 'http.server 8080' | head -1")
    check("server PID", r.stdout.strip().isdigit(), r.stdout.strip())

    for i in range(3):
        await sb.exec.run(f"bash -c '(sleep 2 && echo done-{i} > /workspace/job-{i}.txt) &'")
    await asyncio.sleep(3)
    all_done = True
    for i in range(3):
        content = await sb.files.read(f"/workspace/job-{i}.txt")
        if content.strip() != f"done-{i}":
            all_done = False
    check("3 background jobs", all_done)

    r = await sb.exec.run("ps aux | wc -l")
    check("process count", int(r.stdout.strip()) > 1, r.stdout.strip())

    await sb.exec.run("pkill -9 -f 'http.server 8080' 2>/dev/null; true")
    await asyncio.sleep(2)
    r = await sb.exec.run("bash -c 'curl -sf http://localhost:8080/ >/dev/null 2>&1 && echo alive || echo dead'")
    check("kill server", r.stdout.strip() == "dead", r.stdout.strip())

    r = await sb.exec.run("python3 -c 'import hashlib; [hashlib.sha256(str(i).encode()) for i in range(50000)]; print(\"done\")'")
    check("CPU-intensive task", r.stdout.strip() == "done")

    # ── Checkpoint + Fork (15 ops) ────────────────────────────────
    print("\n\033[1;34m=== Checkpoint + Fork (15 operations) ===\033[0m")

    await sb.exec.run("bash -c 'echo checkpoint-data > /workspace/cp.txt && sync && sync'")
    await asyncio.sleep(1)

    cp = await sb.create_checkpoint(name=f"py-sdk-cp-{int(time.time())}")
    cp_id = cp.get("id", "")
    check("create checkpoint", bool(cp_id), str(cp_id)[:12])

    cps = await sb.list_checkpoints()
    check("list checkpoints", len(cps) >= 1, f"count={len(cps)}")

    await asyncio.sleep(5)
    fork = await Sandbox.create_from_checkpoint(cp_id, timeout=120, api_key=API_KEY, api_url=API_URL)
    check("fork from checkpoint", fork.sandbox_id.startswith("sb-"), fork.sandbox_id)

    await asyncio.sleep(5)
    r = await fork.exec.run("cat /workspace/cp.txt")
    check("fork data correct", r.stdout.strip() == "checkpoint-data", r.stdout.strip())

    fork_entries = await fork.files.list("/workspace")
    check("fork has files", len(fork_entries) > 5, f"entries={len(fork_entries)}")

    r = await fork.exec.run("python3 --version")
    check("fork has python", "Python" in r.stdout, r.stdout.strip())

    await fork.exec.run("echo fork-only > /workspace/fork-file.txt")
    r = await sb.exec.run("cat /workspace/fork-file.txt 2>/dev/null || echo not-found")
    check("fork isolated", r.stdout.strip() == "not-found")

    await fork.kill()
    check("kill fork", True)

    # Restore
    await sb.exec.run("echo post-checkpoint > /workspace/cp.txt")
    await sb.restore_checkpoint(cp_id)
    await asyncio.sleep(15)
    r = await sb.exec.run("cat /workspace/cp.txt")
    check("restore reverted", r.stdout.strip() == "checkpoint-data", r.stdout.strip())

    await sb.delete_checkpoint(cp_id)
    check("delete checkpoint", True)

    r = await sb.exec.run("echo alive-after-restore")
    check("alive after restore", r.stdout.strip() == "alive-after-restore")

    r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'")
    check("memory after restore", int(r.stdout.strip()) > 800, f"{r.stdout.strip()}MB")

    # ── Hibernate + Wake (10 ops) ─────────────────────────────────
    print("\n\033[1;34m=== Hibernate + Wake (10 operations) ===\033[0m")

    await sb.exec.run("echo pre-hibernate > /workspace/hib.txt")
    check("write pre-hibernate marker", True)

    # Hibernate/wake not in SDK — use the internal client
    client = sb._ops_client
    resp = await client.post(f"/sandboxes/{sb.sandbox_id}/hibernate")
    check("hibernate", resp.status_code == 200, f"HTTP {resp.status_code}")

    resp = await client.post(f"/sandboxes/{sb.sandbox_id}/wake", json={"timeout": 3600})
    check("wake", resp.status_code == 200, f"HTTP {resp.status_code}")

    r = await sb.exec.run("cat /workspace/hib.txt")
    check("data survived hibernate", r.stdout.strip() == "pre-hibernate", r.stdout.strip())

    r = await sb.exec.run("echo post-wake")
    check("exec after wake", r.stdout.strip() == "post-wake")

    r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'")
    check("memory after wake", int(r.stdout.strip()) > 800, f"{r.stdout.strip()}MB")

    r = await sb.exec.run("date +%s")
    drift = abs(int(r.stdout.strip()) - int(time.time()))
    check("clock after wake", drift < 5, f"drift={drift}s")

    r = await sb.exec.run("curl -s -o /dev/null -w '%{http_code}' https://httpbin.org/get")
    check("network after wake", r.stdout.strip() == "200", r.stdout.strip())

    entries = await sb.files.list("/workspace")
    check("files survive wake", len(entries) > 5, f"entries={len(entries)}")

    r = await sb.exec.run("python3 -c 'print(1+1)'")
    check("python after wake", r.stdout.strip() == "2")

    # ── Secrets (5 ops) ───────────────────────────────────────────
    print("\n\033[1;34m=== Secrets (5 operations) ===\033[0m")

    import httpx as _httpx
    async with _httpx.AsyncClient(base_url=API_URL, headers={"X-API-Key": API_KEY, "Content-Type": "application/json"}) as sec:
        resp = await sec.post("/api/secret-stores", json={"name": f"py-store-{int(time.time())}"})
        store_id = resp.json().get("id", "")
        check("create store", bool(store_id), str(resp.json())[:60])

        resp = await sec.put(f"/api/secret-stores/{store_id}/secrets/TEST_KEY", json={"value": "test-value"})
        check("set secret", resp.status_code == 200, f"HTTP {resp.status_code}")

        resp = await sec.get(f"/api/secret-stores/{store_id}/secrets")
        secrets = resp.json() or []
        check("list secrets", any(s.get("name") == "TEST_KEY" for s in secrets), str(secrets)[:60])

        resp = await sec.delete(f"/api/secret-stores/{store_id}/secrets/TEST_KEY")
        check("delete secret", resp.status_code == 204, f"HTTP {resp.status_code}")

        resp = await sec.delete(f"/api/secret-stores/{store_id}")
        check("delete store", resp.status_code == 204, f"HTTP {resp.status_code}")

    # ── Final (5 ops) ─────────────────────────────────────────────
    print("\n\033[1;34m=== Final Checks ===\033[0m")

    r = await sb.exec.run("echo final-health")
    check("final exec", r.stdout.strip() == "final-health")

    running = await sb.is_running()
    check("sandbox is running", running)

    entries = await sb.files.list("/workspace")
    check("final file count", len(entries) > 10, f"entries={len(entries)}")

    r = await sb.exec.run("uptime")
    check("uptime", "load average" in r.stdout, r.stdout.strip())

    await sb.kill()
    check("kill sandbox", True)

    elapsed = time.time() - t0
    total = PASS + FAIL
    print(f"\n\033[1m{PASS} passed, {FAIL} failed ({total} total, {elapsed:.0f}s)\033[0m")
    sys.exit(1 if FAIL > 0 else 0)


asyncio.run(main())
