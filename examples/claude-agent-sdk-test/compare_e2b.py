#!/usr/bin/env python3
"""
Compare OpenSandbox vs E2B for Claude Agent SDK integration.

This script runs the same operations on both sandbox providers
to compare their APIs and performance.

Requirements:
- E2B_API_KEY environment variable for E2B
- OPENSANDBOX_URL environment variable for OpenSandbox (or localhost:8080)
"""

import asyncio
import os
import time
from dataclasses import dataclass
from typing import Optional, Protocol


@dataclass
class CommandResult:
    """Result of a command execution."""
    stdout: str
    stderr: str
    exit_code: int
    duration_ms: float = 0.0


class SandboxProvider(Protocol):
    """Protocol for sandbox providers."""

    @property
    def name(self) -> str: ...
    async def create(self) -> None: ...
    async def run_command(self, command: str) -> CommandResult: ...
    async def write_file(self, path: str, content: str) -> float: ...
    async def read_file(self, path: str) -> str: ...
    async def destroy(self) -> None: ...


class OpenSandboxProvider:
    """OpenSandbox implementation."""

    def __init__(self):
        self._client = None
        self._sandbox = None

    @property
    def name(self) -> str:
        return "OpenSandbox"

    async def create(self) -> None:
        from opensandbox import OpenSandbox

        url = os.environ.get("OPENSANDBOX_URL", "http://localhost:8080")
        grpc_port = int(os.environ.get("OPENSANDBOX_GRPC_PORT", "50051"))
        # Use insecure gRPC for Fly.io (raw TCP, no TLS on gRPC port)
        grpc_insecure = os.environ.get("OPENSANDBOX_GRPC_INSECURE", "").lower() in ("1", "true", "yes")

        self._client = OpenSandbox(url, grpc_port=grpc_port, grpc_insecure=grpc_insecure)
        await self._client._ensure_connected()
        self._sandbox = await self._client.create()

    async def run_command(self, command: str) -> CommandResult:
        start = time.perf_counter()
        result = await self._sandbox.run(command)
        duration = (time.perf_counter() - start) * 1000

        return CommandResult(
            stdout=result.stdout,
            stderr=result.stderr,
            exit_code=result.exit_code,
            duration_ms=duration,
        )

    async def write_file(self, path: str, content: str) -> float:
        start = time.perf_counter()
        await self._sandbox.write_file(path, content)
        return (time.perf_counter() - start) * 1000

    async def read_file(self, path: str) -> str:
        content = await self._sandbox.read_file_text(path)
        return content

    async def destroy(self) -> None:
        if self._sandbox:
            await self._sandbox.destroy()
        if self._client:
            await self._client.close()


class E2BProvider:
    """E2B implementation."""

    def __init__(self):
        self._sandbox = None

    @property
    def name(self) -> str:
        return "E2B"

    async def create(self) -> None:
        from e2b import Sandbox
        # E2B SDK is synchronous, run in executor
        loop = asyncio.get_event_loop()
        self._sandbox = await loop.run_in_executor(
            None, lambda: Sandbox.create(timeout=300)
        )

    async def run_command(self, command: str) -> CommandResult:
        loop = asyncio.get_event_loop()

        start = time.perf_counter()
        result = await loop.run_in_executor(
            None, lambda: self._sandbox.commands.run(command, timeout=60)
        )
        duration = (time.perf_counter() - start) * 1000

        return CommandResult(
            stdout=result.stdout,
            stderr=result.stderr,
            exit_code=result.exit_code,
            duration_ms=duration,
        )

    async def write_file(self, path: str, content: str) -> float:
        loop = asyncio.get_event_loop()
        start = time.perf_counter()
        await loop.run_in_executor(
            None, lambda: self._sandbox.files.write(path, content)
        )
        return (time.perf_counter() - start) * 1000

    async def read_file(self, path: str) -> str:
        loop = asyncio.get_event_loop()
        content = await loop.run_in_executor(
            None, lambda: self._sandbox.files.read(path)
        )
        return content

    async def destroy(self) -> None:
        if self._sandbox:
            loop = asyncio.get_event_loop()
            await loop.run_in_executor(None, self._sandbox.kill)


async def run_benchmark(provider: SandboxProvider) -> dict:
    """Run a set of benchmark operations on a provider."""
    results = {
        "name": provider.name,
        "create_ms": 0,
        "command_ms": [],
        "write_ms": [],
        "read_ms": [],
        "errors": [],
    }

    try:
        # Create sandbox
        start = time.perf_counter()
        await provider.create()
        results["create_ms"] = (time.perf_counter() - start) * 1000
        print(f"  Created sandbox: {results['create_ms']:.1f}ms")

        # Run commands
        for cmd in ["echo hello", "uname -a", "ls -la /", "cat /etc/os-release"]:
            result = await provider.run_command(cmd)
            results["command_ms"].append(result.duration_ms)
            print(f"  Command '{cmd[:20]}...': {result.duration_ms:.1f}ms")

        # Write files
        for size in [100, 1000, 10000]:
            content = "x" * size
            duration = await provider.write_file(f"/tmp/test_{size}.txt", content)
            results["write_ms"].append(duration)
            print(f"  Write {size} bytes: {duration:.1f}ms")

        # Read files
        for size in [100, 1000, 10000]:
            start = time.perf_counter()
            content = await provider.read_file(f"/tmp/test_{size}.txt")
            duration = (time.perf_counter() - start) * 1000
            results["read_ms"].append(duration)
            print(f"  Read {size} bytes: {duration:.1f}ms")

    except Exception as e:
        results["errors"].append(str(e))
        print(f"  Error: {e}")

    finally:
        try:
            await provider.destroy()
            print("  Destroyed sandbox")
        except Exception as e:
            results["errors"].append(f"Destroy error: {e}")

    return results


def print_comparison(results: list[dict]):
    """Print a comparison table of results."""
    print("\n" + "=" * 70)
    print("COMPARISON SUMMARY")
    print("=" * 70)

    headers = ["Metric", *[r["name"] for r in results]]
    print(f"\n{headers[0]:<25} {headers[1]:<20} {headers[2] if len(headers) > 2 else '':<20}")
    print("-" * 65)

    metrics = [
        ("Create sandbox", "create_ms", None),
        ("Avg command exec", "command_ms", lambda x: sum(x) / len(x) if x else 0),
        ("Avg file write", "write_ms", lambda x: sum(x) / len(x) if x else 0),
        ("Avg file read", "read_ms", lambda x: sum(x) / len(x) if x else 0),
    ]

    for label, key, transform in metrics:
        row = [label]
        for r in results:
            val = r.get(key, 0)
            if transform and isinstance(val, list):
                val = transform(val)
            if isinstance(val, (int, float)):
                row.append(f"{val:.1f}ms")
            else:
                row.append("N/A")

        print(f"{row[0]:<25} {row[1]:<20} {row[2] if len(row) > 2 else '':<20}")

    print("\n" + "=" * 70)


async def main():
    """Run comparison between E2B and OpenSandbox."""
    print("\n" + "=" * 70)
    print("E2B vs OpenSandbox Comparison")
    print("=" * 70)

    results = []

    # Test OpenSandbox
    print("\n[OpenSandbox]")
    try:
        provider = OpenSandboxProvider()
        result = await run_benchmark(provider)
        results.append(result)
    except ImportError:
        print("  OpenSandbox SDK not available")
    except Exception as e:
        print(f"  OpenSandbox failed: {e}")

    # Test E2B
    print("\n[E2B]")
    e2b_key = os.environ.get("E2B_API_KEY")
    if not e2b_key:
        print("  Skipped: E2B_API_KEY not set")
    else:
        try:
            provider = E2BProvider()
            result = await run_benchmark(provider)
            results.append(result)
        except ImportError:
            print("  E2B SDK not available (pip install e2b)")
        except Exception as e:
            print(f"  E2B failed: {e}")

    if len(results) >= 1:
        print_comparison(results)

    # Print feature comparison
    print("\n" + "=" * 70)
    print("FEATURE COMPARISON")
    print("=" * 70)
    print("""
| Feature                    | OpenSandbox           | E2B                    |
|----------------------------|----------------------|------------------------|
| Hosting                    | Self-hosted / Fly.io | Cloud only             |
| Protocol                   | gRPC + HTTP          | HTTP (WebSocket)       |
| Pricing                    | Free (self-hosted)   | Pay per use            |
| Session timeout            | 5 min default        | Configurable           |
| Network access             | Full                 | Full                   |
| Custom images              | Docker-based         | Custom templates       |
| File operations            | Native gRPC          | HTTP-based             |
| Interactive terminal       | No                   | Yes (WebSocket)        |
| Claude Code integration    | Via SDK              | Via SDK                |
""")


if __name__ == "__main__":
    asyncio.run(main())
