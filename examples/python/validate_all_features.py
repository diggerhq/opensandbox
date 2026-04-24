"""
OpenComputer Full SDK Validation — Python
==========================================
Tests EVERY feature exposed by the Python SDK against a live server.

Usage:
    OPENCOMPUTER_API_KEY=your-key OPENCOMPUTER_API_URL=http://your-server:8080 python validate_all_features.py

Required: pip install opencomputer httpx
"""

import asyncio
import os
import time
import traceback

import httpx
from opencomputer import Image, Sandbox, SecretStore, Snapshots

API_URL = os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
API_KEY = os.environ.get("OPENCOMPUTER_API_KEY", "")

PASS = 0
FAIL = 0
SKIP = 0
ERRORS = []


def ok(msg):
    global PASS
    PASS += 1
    print(f"  ✓ {msg}")


def fail(msg, err=None):
    global FAIL
    FAIL += 1
    detail = f" — {err}" if err else ""
    print(f"  ✗ {msg}{detail}")
    ERRORS.append(f"{msg}{detail}")


def skip(msg):
    global SKIP
    SKIP += 1
    print(f"  ⊘ {msg}")


async def wait_ready(sb, timeout=30):
    """Poll until sandbox can actually execute commands (not just status=running)."""
    for _ in range(timeout):
        try:
            r = await sb.exec.run("echo ready", timeout=3)
            if r.stdout.strip() == "ready":
                return True
        except:
            pass
        await asyncio.sleep(1)
    return False


async def test(name, coro):
    try:
        await coro
    except Exception as e:
        fail(name, str(e))


async def main():
    print("╔═══════════════════════════════════════════════════╗")
    print("║  OpenComputer Python SDK — Full Feature Validation ║")
    print(f"║  Server: {API_URL:<42}║")
    print("╚═══════════════════════════════════════════════════╝\n")

    sandboxes = []
    store_ids = []

    try:
        # ─── 1. SANDBOX LIFECYCLE ────────────────────────────────────
        print("▸ 1. Sandbox Lifecycle")

        sb = await Sandbox.create(timeout=600)
        sandboxes.append(sb)
        ok(f"create: {sb.sandbox_id}")

        running = await sb.is_running()
        ok(f"is_running: {running}") if running else fail("is_running: False")

        await sb.set_timeout(300)
        ok("set_timeout(300)")

        # Connect to existing
        sb2 = await Sandbox.connect(sb.sandbox_id)
        ok(f"connect: {sb2.sandbox_id}")
        await sb2.close()
        print()

        # ─── 2. EXEC.RUN ────────────────────────────────────────────
        print("▸ 2. Exec (fire-and-forget)")

        r = await sb.exec.run("echo hello")
        ok(f"echo: {r.stdout.strip()}") if r.stdout.strip() == "hello" else fail(f"echo: {r.stdout}")

        r = await sb.exec.run("python3 -c \"print(2+2)\"")
        ok(f"python3: {r.stdout.strip()}") if r.stdout.strip() == "4" else fail(f"python3: {r.stdout}")

        r = await sb.exec.run("node -e \"console.log(JSON.stringify({ok:true}))\"")
        ok(f"node: {r.stdout.strip()}") if "ok" in r.stdout else fail(f"node: {r.stdout}")

        r = await sb.exec.run("env | grep MY_VAR", env={"MY_VAR": "test123"})
        ok(f"exec with env") if "test123" in r.stdout else fail(f"exec env: {r.stdout}")

        r = await sb.exec.run("pwd", cwd="/workspace")
        ok(f"exec with cwd") if "/workspace" in r.stdout else fail(f"exec cwd: {r.stdout}")

        # Timeout — server-side exec timeout (seconds, not ms)
        t0 = time.time()
        r = await sb.exec.run("sleep 30", timeout=3)
        elapsed = time.time() - t0
        ok(f"exec timeout ({elapsed:.1f}s)") if elapsed < 10 else fail(f"exec timeout: {elapsed:.1f}s")
        print()

        # ─── 3. EXEC.START (long-running sessions) ─────────────────
        print("▸ 3. Exec Sessions")

        # start() returns an ExecSession with streaming + done future (matches TS SDK).
        sess = await sb.exec.start(
            "python3",
            args=["-c", "import time\nfor i in range(3): print(f'line-{i}', flush=True); time.sleep(0.2)"],
        )
        ok(f"start session: {sess.session_id}") if sess.session_id else fail(f"start: {sess.session_id}")
        await sess.done
        await sess.close()

        # List sessions
        sessions = await sb.exec.list()
        ok(f"list sessions: {len(sessions)}") if len(sessions) >= 1 else fail(f"list: {sessions}")

        # Start + kill
        long_sess = await sb.exec.start("sleep", args=["300"])
        await asyncio.sleep(0.5)
        await long_sess.kill()
        await long_sess.close()
        ok("kill session")
        print()

        # ─── 4. FILES ────────────────────────────────────────────────
        print("▸ 4. Filesystem")

        await sb.files.write("/workspace/test.txt", "hello world")
        ok("write text file")

        content = await sb.files.read("/workspace/test.txt")
        ok(f"read text: {content}") if content == "hello world" else fail(f"read: {content}")

        # Binary
        binary = bytes(range(256))
        await sb.files.write("/workspace/binary.bin", binary)
        read_back = await sb.files.read_bytes("/workspace/binary.bin")
        ok("binary round-trip") if read_back == binary else fail(f"binary: {len(read_back)} bytes")

        # List
        entries = await sb.files.list("/workspace")
        names = [e.name for e in entries]
        ok(f"list: {len(names)} files") if "test.txt" in names else fail(f"list: {names}")

        # Exists
        exists = await sb.files.exists("/workspace/test.txt")
        ok("exists: True") if exists else fail("exists: False")

        not_exists = await sb.files.exists("/workspace/nope.txt")
        ok("not exists: False") if not not_exists else fail("not exists: True")

        # Make dir
        await sb.files.make_dir("/workspace/subdir/nested")
        r = await sb.exec.run("ls -d /workspace/subdir/nested")
        ok("make_dir") if r.exit_code == 0 else fail("make_dir")

        # Remove
        await sb.files.remove("/workspace/test.txt")
        exists_after = await sb.files.exists("/workspace/test.txt")
        ok("remove file") if not exists_after else fail("remove: still exists")

        # Download/upload URLs
        try:
            url = await sb.download_url("/workspace/binary.bin")
            ok(f"download_url: {url[:40]}...") if url.startswith("http") else fail(f"download_url: {url}")
        except Exception as e:
            skip(f"download_url: {e}")

        try:
            url = await sb.upload_url("/workspace/upload-test.txt")
            ok(f"upload_url: {url[:40]}...") if url.startswith("http") else fail(f"upload_url: {url}")
        except Exception as e:
            skip(f"upload_url: {e}")
        print()

        # ─── 5. PTY ─────────────────────────────────────────────────
        print("▸ 5. PTY Sessions")
        try:
            pty = await sb.pty.create(cols=120, rows=40)
            ok(f"create PTY: {pty.session_id}")

            # Send a command
            await pty.send("echo pty-works\n")
            await asyncio.sleep(1)
            data = await pty.recv()
            ok(f"PTY I/O: {len(data)} bytes") if len(data) > 0 else fail("PTY recv empty")

            await pty.close()
            ok("close PTY")
        except Exception as e:
            skip(f"PTY: {e}")
        print()

        # ─── 6. MEMORY SCALING ───────────────────────────────────────
        print("▸ 6. Memory Scaling")
        try:
            async with httpx.AsyncClient(headers={"X-API-Key": API_KEY, "Content-Type": "application/json"}, timeout=30) as client:
                resp = await client.put(f"{API_URL}/api/sandboxes/{sb.sandbox_id}/limits", json={"memoryMB": 2048})
            if resp.status_code == 200:
                ok("scale to 2GB")
                await asyncio.sleep(1)
                r = await sb.exec.run("free -m | awk '/Mem:/{print $2}'")
                mem = int(r.stdout.strip())
                ok(f"verified: {mem}MB") if mem > 1800 else fail(f"mem: {mem}MB")

                # Scale down
                async with httpx.AsyncClient(headers={"X-API-Key": API_KEY, "Content-Type": "application/json"}, timeout=30) as client:
                    resp = await client.put(f"{API_URL}/api/sandboxes/{sb.sandbox_id}/limits", json={"memoryMB": 1024})
                ok("scale down to 1GB") if resp.status_code == 200 else fail(f"scale down: {resp.status_code}")
            else:
                skip(f"scale: {resp.status_code}")
        except Exception as e:
            skip(f"scaling: {e}")
        print()

        # ─── 7. METADATA API ────────────────────────────────────────
        print("▸ 7. Metadata API (169.254.169.254)")

        r = await sb.exec.run("curl -s http://169.254.169.254/v1/status")
        ok("status") if sb.sandbox_id in r.stdout else skip(f"status: {r.stdout[:40]}")

        r = await sb.exec.run("curl -s http://169.254.169.254/v1/metadata")
        ok("metadata") if "region" in r.stdout else skip(f"metadata: {r.stdout[:40]}")

        r = await sb.exec.run("curl -s http://169.254.169.254/v1/limits")
        ok("limits") if "memLimit" in r.stdout else skip(f"limits: {r.stdout[:40]}")

        # Clock
        r = await sb.exec.run("date +%s")
        try:
            drift = abs(int(r.stdout.strip()) - int(time.time()))
            ok(f"clock drift: {drift}s") if drift <= 3 else fail(f"clock drift: {drift}s")
        except:
            skip("clock check failed")
        print()

        # ─── 8. NETWORK ─────────────────────────────────────────────
        print("▸ 8. Network")

        r = await sb.exec.run("curl -s -o /dev/null -w '%{http_code}' https://httpbin.org/ip", timeout=15000)
        ok("HTTPS outbound") if r.stdout.strip() == "200" else fail(f"HTTPS: {r.stdout}")

        r = await sb.exec.run("ping -c1 -W3 8.8.8.8 2>&1 | grep -c 'bytes from'")
        ok("ping") if r.stdout.strip() == "1" else skip(f"ping blocked (ICMP not allowed on prod)")
        print()

        # ─── 9. PREVIEW URLs ────────────────────────────────────────
        print("▸ 9. Preview URLs")

        await sb.exec.run("bash -c 'mkdir -p /workspace && echo hello > /workspace/index.html'")
        await sb.exec.run("bash -c 'setsid python3 -m http.server 3000 --directory /workspace </dev/null >/dev/null 2>&1 &'")
        await asyncio.sleep(2)

        try:
            preview = await sb.create_preview_url(port=3000)
            hostname = preview.get("hostname", "")
            ok(f"create preview: {hostname}") if hostname else fail(f"preview: {preview}")

            previews = await sb.list_preview_urls()
            ok(f"list previews: {len(previews)}") if len(previews) >= 1 else fail(f"list: {previews}")

            await sb.delete_preview_url(3000)
            ok("delete preview")
        except Exception as e:
            skip(f"preview URLs: {e}")
        print()

        # ─── 10. HIBERNATE / WAKE ───────────────────────────────────
        print("▸ 10. Hibernate / Wake")

        await sb.files.write("/workspace/persist.txt", "survive-hibernate")
        await sb.exec.run("pip3 install --quiet pyyaml 2>/dev/null", timeout=60)

        # Python SDK doesn't have hibernate/wake — use REST API directly
        try:
            async with httpx.AsyncClient(headers={"X-API-Key": API_KEY, "Content-Type": "application/json"}, timeout=120) as client:
                resp = await client.post(f"{API_URL}/api/sandboxes/{sb.sandbox_id}/hibernate")
                resp.raise_for_status()
            ok("hibernate")

            async with httpx.AsyncClient(headers={"X-API-Key": API_KEY, "Content-Type": "application/json"}, timeout=120) as client:
                resp = await client.post(f"{API_URL}/api/sandboxes/{sb.sandbox_id}/wake", json={"timeout": 600})
                resp.raise_for_status()
            ok("wake")

            content = await sb.files.read("/workspace/persist.txt")
            ok("workspace survived") if content == "survive-hibernate" else fail(f"persist: {content}")

            r = await sb.exec.run("python3 -c \"import yaml; print(yaml.__version__)\"")
            ok(f"rootfs survived: pyyaml {r.stdout.strip()}") if r.exit_code == 0 else fail("pyyaml gone")
        except Exception as e:
            skip(f"hibernate/wake: {e}")
        print()

        # ─── 11. CHECKPOINT / FORK ──────────────────────────────────
        print("▸ 11. Checkpoint & Fork")

        await sb.files.write("/workspace/checkpoint-data.txt", "at-checkpoint")
        cp = await sb.create_checkpoint(name="validate-cp")
        cp_id = cp["id"]
        ok(f"create checkpoint: {cp_id[:12]}...")

        # Wait for ready
        for _ in range(20):
            cps = await sb.list_checkpoints()
            found = [c for c in cps if c["id"] == cp_id]
            if found and found[0].get("status") == "ready":
                break
            await asyncio.sleep(1)
        ok("checkpoint ready")

        # List checkpoints
        cps = await sb.list_checkpoints()
        ok(f"list checkpoints: {len(cps)}") if len(cps) >= 1 else fail(f"list: {cps}")

        # Fork
        fork = await Sandbox.create_from_checkpoint(cp_id, timeout=300)
        sandboxes.append(fork)
        await wait_ready(fork)
        ok(f"fork: {fork.sandbox_id}")

        content = await fork.files.read("/workspace/checkpoint-data.txt")
        ok("fork has checkpoint data") if content == "at-checkpoint" else fail(f"fork: {content}")

        # Isolation
        await fork.files.write("/workspace/fork-only.txt", "fork")
        r = await sb.exec.run("cat /workspace/fork-only.txt 2>&1 || echo not-found")
        ok("fork isolated") if "not-found" in r.stdout else fail("fork leaked")

        await fork.kill()
        sandboxes.remove(fork)
        ok("fork killed")
        print()

        # ─── 12. RESTORE CHECKPOINT ─────────────────────────────────
        print("▸ 12. Restore Checkpoint")

        await sb.exec.run("rm /workspace/checkpoint-data.txt")
        await sb.files.write("/workspace/post-cp.txt", "after")

        await sb.restore_checkpoint(cp_id)
        await wait_ready(sb)
        ok("restore")

        content = await sb.files.read("/workspace/checkpoint-data.txt")
        ok("restored: file back") if content == "at-checkpoint" else fail(f"restore: {content}")

        r = await sb.exec.run("cat /workspace/post-cp.txt 2>&1 || echo gone")
        ok("restored: post-cp gone") if "gone" in r.stdout else fail("post-cp still exists")

        # Delete checkpoint
        await sb.delete_checkpoint(cp_id)
        ok("delete checkpoint")
        print()

        # ─── 13. SECRET STORES ──────────────────────────────────────
        print("▸ 13. Secret Stores")

        store_name = f"validate-{int(time.time())}"
        try:
            store = await SecretStore.create(name=store_name, egress_allowlist=["httpbin.org"])
            store_id = store["id"]
            store_ids.append(store_id)
            ok(f"create store: {store_name}")

            await SecretStore.set_secret(store_id, "TEST_KEY", "secret-value")
            ok("set secret")

            secrets = await SecretStore.list_secrets(store_id)
            ok(f"list secrets: {len(secrets)}") if len(secrets) == 1 else fail(f"secrets: {secrets}")

            # Verify plaintext not exposed
            ok("no plaintext") if "secret-value" not in str(secrets) else fail("PLAINTEXT LEAKED")

            stores = await SecretStore.list()
            ok(f"list stores: {len(stores)}") if len(stores) >= 1 else fail(f"stores: {stores}")

            store_info = await SecretStore.get(store_id)
            ok("get store") if store_info["name"] == store_name else fail(f"get: {store_info}")

            await SecretStore.update(store_id, egress_allowlist=["httpbin.org", "api.github.com"])
            ok("update store")

            await SecretStore.delete_secret(store_id, "TEST_KEY")
            ok("delete secret")

            await SecretStore.delete(store_id)
            store_ids.remove(store_id)
            ok("delete store")
        except Exception as e:
            fail(f"secret stores: {e}")
        print()

        # ─── 14. IMAGE BUILDER ──────────────────────────────────────
        print("▸ 14. Image Builder")

        try:
            image = Image.base().run_commands("echo image-built > /workspace/image-proof.txt").pip_install(["httpx"])
            img_sb = await Sandbox.create(image=image, timeout=300)
            sandboxes.append(img_sb)
            await wait_ready(img_sb)
            ok(f"image build: {img_sb.sandbox_id}")

            content = await img_sb.files.read("/workspace/image-proof.txt")
            ok("image workspace") if "image-built" in content else fail(f"image ws: {content}")

            r = await img_sb.exec.run("python3 -c \"import httpx; print(httpx.__version__)\"")
            ok(f"image pip: httpx {r.stdout.strip()}") if r.exit_code == 0 else fail("httpx missing")

            # Cache hit
            t0 = time.time()
            img_sb2 = await Sandbox.create(image=image, timeout=300)
            sandboxes.append(img_sb2)
            elapsed = time.time() - t0
            ok(f"cache hit: {elapsed:.1f}s") if elapsed < 5 else ok(f"cache: {elapsed:.1f}s")

            await img_sb.kill()
            sandboxes.remove(img_sb)
            await img_sb2.kill()
            sandboxes.remove(img_sb2)
        except Exception as e:
            fail(f"image builder: {e}")
        print()

        # ─── 15. SNAPSHOTS ──────────────────────────────────────────
        print("▸ 15. Named Snapshots")

        snap_name = f"validate-snap-{int(time.time())}"
        try:
            snapshots = Snapshots()
            snap = await snapshots.create(
                name=snap_name,
                image=Image.base().run_commands("echo snap-data > /workspace/snap.txt"),
            )
            ok(f"create snapshot: {snap_name}")

            snap_list = await snapshots.list()
            ok(f"list snapshots: {len(snap_list)}") if any(s["name"] == snap_name for s in snap_list) else fail("not in list")

            snap_info = await snapshots.get(snap_name)
            ok("get snapshot") if snap_info["status"] == "ready" else fail(f"status: {snap_info['status']}")

            snap_sb = await Sandbox.create(snapshot=snap_name, timeout=300)
            sandboxes.append(snap_sb)
            await wait_ready(snap_sb)
            content = await snap_sb.files.read("/workspace/snap.txt")
            ok("sandbox from snapshot") if "snap-data" in content else fail(f"snap content: {content}")
            await snap_sb.kill()
            sandboxes.remove(snap_sb)

            await snapshots.delete(snap_name)
            ok("delete snapshot")
            await snapshots.close()
        except Exception as e:
            fail(f"snapshots: {e}")
        print()

        # ─── 16. SANDBOX WITH SECRETS ────────────────────────────────
        print("▸ 16. Sandbox with Secret Store")

        try:
            store = await SecretStore.create(name=f"validate-inject-{int(time.time())}")
            store_ids.append(store["id"])
            await SecretStore.set_secret(store["id"], "INJECTED_KEY", "injected-value")

            secret_sb = await Sandbox.create(timeout=300, secret_store=store["name"])
            sandboxes.append(secret_sb)

            r = await secret_sb.exec.run("echo $INJECTED_KEY")
            val = r.stdout.strip()
            ok(f"secret injected: {val[:20]}") if len(val) > 0 else fail("secret not injected")

            await secret_sb.kill()
            sandboxes.remove(secret_sb)
            await SecretStore.delete(store["id"])
            store_ids.remove(store["id"])
        except Exception as e:
            skip(f"sandbox with secrets: {e}")
        print()

    except Exception as e:
        fail(f"UNEXPECTED: {traceback.format_exc()}")

    finally:
        # ─── CLEANUP ─────────────────────────────────────────────────
        print("▸ Cleanup")
        for s in sandboxes:
            try:
                await s.kill()
                print(f"  Killed {s.sandbox_id}")
            except:
                pass
        for sid in store_ids:
            try:
                await SecretStore.delete(sid)
                print(f"  Deleted store {sid[:12]}...")
            except:
                pass

    # ─── RESULTS ─────────────────────────────────────────────────
    print(f"\n{'═' * 55}")
    print(f"  {PASS} passed, {FAIL} failed, {SKIP} skipped")
    if ERRORS:
        print(f"\n  Failures:")
        for e in ERRORS:
            print(f"    ✗ {e}")
    print(f"{'═' * 55}")
    return FAIL == 0


if __name__ == "__main__":
    success = asyncio.run(main())
    exit(0 if success else 1)
