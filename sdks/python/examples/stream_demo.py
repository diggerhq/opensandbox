"""stream_demo.py — simulates apt-install-style output over ~6 seconds
via shell.run(), printing each chunk with its arrival timestamp so you
can see exactly how streaming feels in practice.

Usage:
    python sdks/python/examples/stream_demo.py
"""

from __future__ import annotations

import asyncio
import os
import time

from opencomputer import Sandbox

API_URL = os.environ.get("OPENCOMPUTER_API_URL", "https://app.opencomputer.dev")
API_KEY = os.environ.get("OPENCOMPUTER_API_KEY", "opensandbox-dev")

# Pseudo-apt output. Uses /bin/echo (external, flushes on exit) rather
# than bash builtins so glibc block-buffering doesn't hide the streaming.
APT_SIM = r"""
/bin/echo "Reading package lists..."
sleep 0.3
/bin/echo "Building dependency tree..."
sleep 0.2
/bin/echo "Reading state information..."
sleep 0.4
/bin/echo "The following NEW packages will be installed:"
/bin/echo "  libfoo1 libfoo-dev pkg-a pkg-b pkg-c"
sleep 0.3
/bin/echo "0 upgraded, 5 newly installed, 0 to remove and 0 not upgraded."
/bin/echo "Need to get 4,321 kB of archives."
/bin/echo "After this operation, 12.3 MB of additional disk space will be used."
sleep 0.4
for i in 1 2 3 4 5; do
  /bin/echo "Get:$i http://deb.example.com/debian bookworm/main amd64 pkg-$i"
  sleep 0.2
done
/bin/echo "Fetched 4,321 kB in 1s (4,321 kB/s)"
sleep 0.3
for i in 1 2 3 4 5; do
  /bin/echo "Selecting previously unselected package pkg-$i."
  /bin/echo "Unpacking pkg-$i (1.0-$i) ..."
  sleep 0.15
  /bin/echo "Setting up pkg-$i (1.0-$i) ..."
  sleep 0.15
done
/bin/echo "W: Could not fetch http://example.com/deprecated — ignoring" >&2
/bin/echo "Processing triggers for libc-bin (2.36-9+deb12u3) ..."
sleep 0.4
/bin/echo "Processing triggers for man-db (2.11.2-2) ..."
sleep 0.5
/bin/echo "done."
"""


async def main() -> None:
    print(f"API: {API_URL}")
    sb = await Sandbox.create(api_url=API_URL, api_key=API_KEY, template="base")
    print(f"sandbox: {sb.sandbox_id}")

    t0 = time.monotonic()
    out_count = 0
    err_count = 0
    last_t = t0

    try:
        sh = await sb.exec.shell()

        def on_out(b: bytes) -> None:
            nonlocal out_count, last_t
            out_count += 1
            now = time.monotonic()
            gap = now - last_t
            last_t = now
            # Show dt-since-start and gap-since-last-chunk.
            print(f"[+{now - t0:5.2f}s Δ{gap:4.2f}s OUT] {b.decode().rstrip()}")

        def on_err(b: bytes) -> None:
            nonlocal err_count, last_t
            err_count += 1
            now = time.monotonic()
            gap = now - last_t
            last_t = now
            print(f"[+{now - t0:5.2f}s Δ{gap:4.2f}s ERR] {b.decode().rstrip()}")

        print("--- starting simulated apt-install ---")
        r = await sh.run(APT_SIM, on_stdout=on_out, on_stderr=on_err)
        total = time.monotonic() - t0
        print("--- done ---")
        print(f"exit={r.exit_code}  total_wall={total:.2f}s  "
              f"stdout_chunks={out_count} stderr_chunks={err_count}")
        print(f"stdout_bytes={len(r.stdout)} stderr_bytes={len(r.stderr)}")
        await sh.close()
    finally:
        await sb.kill()


if __name__ == "__main__":
    asyncio.run(main())
