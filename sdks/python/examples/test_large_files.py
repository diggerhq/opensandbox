#!/usr/bin/env python3
"""
Large File Streaming Test

Tests:
  1. Write/read 100MB file via SDK
  2. Download 100MB file via signed URL
  3. Upload 100MB file via signed URL
  4. read_stream / write_stream 50MB

Usage:
  python examples/test_large_files.py
"""

import asyncio
import hashlib
import sys
import time

import httpx

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


def generate_data(size_mb: int) -> bytes:
    """Generate deterministic data: repeating 256-byte pattern."""
    pattern = bytes(range(256))
    repeats = (size_mb * 1024 * 1024) // 256
    return pattern * repeats


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


async def collect_stream(stream) -> bytes:
    """Collect an async byte iterator into a single bytes object."""
    chunks = []
    async for chunk in stream:
        chunks.append(chunk)
    return b"".join(chunks)


async def byte_chunks(data: bytes, chunk_size: int = 256 * 1024):
    """Yield data in chunks as an async iterator."""
    for i in range(0, len(data), chunk_size):
        yield data[i : i + chunk_size]


async def main() -> None:
    global passed, failed

    bold("\n╔══════════════════════════════════════════════════╗")
    bold("║       Large File Streaming Test (Python)         ║")
    bold("╚══════════════════════════════════════════════════╝\n")

    sandbox = None

    try:
        sandbox = await Sandbox.create(template="base", timeout=300)
        green(f"Created sandbox: {sandbox.sandbox_id}")
        print()

        # ── Test 1: Write/read 100MB ─────────────────────────────
        bold("━━━ Test 1: Write/read 100MB file via SDK ━━━\n")

        data100 = generate_data(100)
        hash100 = sha256(data100)
        dim(f"Generated 100MB data, SHA-256: {hash100[:16]}...")

        t0 = time.time()
        await sandbox.files.write("/root/large100.bin", data100)
        write_s = time.time() - t0
        dim(f"Write took {write_s:.1f}s ({100 / write_s:.1f} MB/s)")
        green("100MB write completed")

        t1 = time.time()
        read_back = await sandbox.files.read_bytes("/root/large100.bin")
        read_s = time.time() - t1
        dim(f"Read took {read_s:.1f}s ({100 / read_s:.1f} MB/s)")

        check(
            "Read returns correct size",
            len(read_back) == len(data100),
            f"got {len(read_back)}, expected {len(data100)}",
        )
        read_hash = sha256(read_back)
        check("SHA-256 matches after read", read_hash == hash100)
        print()

        # ── Test 2: Download 100MB via signed URL ────────────────
        bold("━━━ Test 2: Download 100MB via signed URL ━━━\n")

        dl_url = await sandbox.download_url("/root/large100.bin", expires_in=300)
        dim("Download URL generated")

        t2 = time.time()
        async with httpx.AsyncClient(timeout=120.0) as http:
            dl_resp = await http.get(dl_url)
        check("Signed URL returns 200", dl_resp.status_code == 200)

        content_length = dl_resp.headers.get("content-length")
        dim(f"Content-Length: {content_length}")
        check("Content-Length is 100MB", content_length == str(len(data100)))

        dl_data = dl_resp.content
        dl_s = time.time() - t2
        dim(f"Download took {dl_s:.1f}s ({100 / dl_s:.1f} MB/s)")

        check("Downloaded size correct", len(dl_data) == len(data100))
        dl_hash = sha256(dl_data)
        check("SHA-256 matches via signed URL download", dl_hash == hash100)
        print()

        # ── Test 3: Upload 100MB via signed URL ──────────────────
        bold("━━━ Test 3: Upload 100MB via signed URL ━━━\n")

        up_url = await sandbox.upload_url("/root/uploaded100.bin")
        dim("Upload URL generated")

        t3 = time.time()
        async with httpx.AsyncClient(timeout=120.0) as http:
            up_resp = await http.put(up_url, content=data100)
        up_s = time.time() - t3
        dim(f"Upload took {up_s:.1f}s ({100 / up_s:.1f} MB/s)")
        check("Upload returns 204", up_resp.status_code == 204)

        up_read_back = await sandbox.files.read_bytes("/root/uploaded100.bin")
        check("Uploaded file size correct", len(up_read_back) == len(data100))
        up_hash = sha256(up_read_back)
        check("SHA-256 matches after signed URL upload", up_hash == hash100)
        print()

        # ── Test 4: read_stream / write_stream (50MB) ────────────
        bold("━━━ Test 4: read_stream / write_stream 50MB ━━━\n")

        data50 = generate_data(50)
        hash50 = sha256(data50)
        dim(f"Generated 50MB data, SHA-256: {hash50[:16]}...")

        # write_stream
        t4 = time.time()
        await sandbox.files.write_stream("/root/stream50.bin", byte_chunks(data50))
        ws_s = time.time() - t4
        dim(f"write_stream took {ws_s:.1f}s")
        green("50MB write_stream completed")

        # read_stream
        t5 = time.time()
        stream = await sandbox.files.read_stream("/root/stream50.bin")
        stream_data = await collect_stream(stream)
        rs_s = time.time() - t5
        dim(f"read_stream took {rs_s:.1f}s")

        check(
            "read_stream returns correct size",
            len(stream_data) == len(data50),
            f"got {len(stream_data)}, expected {len(data50)}",
        )
        stream_hash = sha256(stream_data)
        check("SHA-256 matches via read_stream", stream_hash == hash50)
        print()

    except Exception as e:
        red(f"Fatal error: {e}")
        import traceback

        traceback.print_exc()
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
