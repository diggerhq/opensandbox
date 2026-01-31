#!/usr/bin/env python3
"""Benchmark: File read/write operations."""

import time
import statistics
import string
import random
from typing import Optional
from dataclasses import dataclass, field

from sandbox_interface import get_sandbox, BaseSandbox


def generate_content(size_bytes: int) -> str:
    """Generate random content of specified size."""
    chars = string.ascii_letters + string.digits + "\n "
    return ''.join(random.choice(chars) for _ in range(size_bytes))


# File sizes to test (name, size in bytes)
FILE_SIZES = [
    ("tiny", 100),           # 100 bytes
    ("small", 1024),         # 1 KB
    # ("medium", 10240),     # 10 KB - skipped
    # ("large", 51200),      # 50 KB - skipped
    # ("xlarge", 1048576),   # 1 MB - skipped
]


@dataclass
class FileBenchmark:
    """Results for a single file size."""
    name: str
    size_bytes: int
    write_times_ms: list[float] = field(default_factory=list)
    read_times_ms: list[float] = field(default_factory=list)
    success_count: int = 0
    fail_count: int = 0

    @property
    def avg_write_ms(self) -> float:
        return statistics.mean(self.write_times_ms) if self.write_times_ms else 0

    @property
    def avg_read_ms(self) -> float:
        return statistics.mean(self.read_times_ms) if self.read_times_ms else 0

    @property
    def write_throughput_kbps(self) -> float:
        """Write throughput in KB/s."""
        if not self.write_times_ms:
            return 0
        avg_sec = self.avg_write_ms / 1000
        if avg_sec == 0:
            return 0
        return (self.size_bytes / 1024) / avg_sec

    @property
    def read_throughput_kbps(self) -> float:
        """Read throughput in KB/s."""
        if not self.read_times_ms:
            return 0
        avg_sec = self.avg_read_ms / 1000
        if avg_sec == 0:
            return 0
        return (self.size_bytes / 1024) / avg_sec

    def to_dict(self) -> dict:
        return {
            "name": self.name,
            "size_bytes": self.size_bytes,
            "write": {
                "avg_ms": round(self.avg_write_ms, 2),
                "min_ms": round(min(self.write_times_ms), 2) if self.write_times_ms else 0,
                "max_ms": round(max(self.write_times_ms), 2) if self.write_times_ms else 0,
                "throughput_kbps": round(self.write_throughput_kbps, 2),
            },
            "read": {
                "avg_ms": round(self.avg_read_ms, 2),
                "min_ms": round(min(self.read_times_ms), 2) if self.read_times_ms else 0,
                "max_ms": round(max(self.read_times_ms), 2) if self.read_times_ms else 0,
                "throughput_kbps": round(self.read_throughput_kbps, 2),
            },
            "success": self.success_count,
            "fail": self.fail_count,
        }


@dataclass
class FilesResult:
    """Results from file benchmark."""
    provider: str
    iterations: int
    files: list[FileBenchmark]
    setup_time_ms: float
    teardown_time_ms: float
    errors: list[str]

    def to_dict(self) -> dict:
        return {
            "provider": self.provider,
            "iterations": self.iterations,
            "setup_time_ms": round(self.setup_time_ms, 2),
            "teardown_time_ms": round(self.teardown_time_ms, 2),
            "files": [f.to_dict() for f in self.files],
            "errors": self.errors,
        }


def run_benchmark(provider: str, iterations: int = 3, **kwargs) -> FilesResult:
    """
    Benchmark file read/write operations.

    Args:
        provider: "opensandbox" or "e2b"
        iterations: Number of times to run each test

    Returns:
        FilesResult with timing data
    """
    print(f"\n{'='*60}")
    print(f"File Operations Benchmark: {provider}")
    print(f"{'='*60}")

    errors = []
    files = [FileBenchmark(name=name, size_bytes=size) for name, size in FILE_SIZES]

    # Pre-generate content for each size
    print("\nGenerating test content...")
    contents = {}
    for name, size in FILE_SIZES:
        contents[name] = generate_content(size)
        print(f"  {name}: {size:,} bytes")

    # Create sandbox
    sandbox: Optional[BaseSandbox] = None
    try:
        sandbox = get_sandbox(provider, **kwargs)
        print("\nCreating sandbox...")
        start = time.perf_counter()
        sandbox.create(timeout=300)
        setup_time_ms = (time.perf_counter() - start) * 1000
        print(f"  Setup time: {setup_time_ms:.2f} ms")

        # Create test directory
        sandbox.run_command("mkdir -p /tmp/benchmark")

    except Exception as e:
        errors.append(f"Failed to create sandbox: {e}")
        print(f"  ERROR: {e}")
        return FilesResult(
            provider=provider,
            iterations=iterations,
            files=files,
            setup_time_ms=0,
            teardown_time_ms=0,
            errors=errors,
        )

    # Run benchmarks
    print(f"\nRunning {len(FILE_SIZES)} file sizes x {iterations} iterations...")

    for iteration in range(iterations):
        print(f"\n--- Iteration {iteration + 1}/{iterations} ---")

        for i, (name, size) in enumerate(FILE_SIZES):
            content = contents[name]
            path = f"/tmp/benchmark/test_{name}_{iteration}.txt"

            try:
                # Write benchmark
                write_time = sandbox.write_file(path, content)
                files[i].write_times_ms.append(write_time)

                # Read benchmark
                read_content, read_time = sandbox.read_file(path)
                files[i].read_times_ms.append(read_time)

                # Verify content
                if read_content.strip() == content.strip():
                    files[i].success_count += 1
                else:
                    files[i].fail_count += 1
                    errors.append(f"{name}: Content mismatch")

                print(f"  {name} ({size:,}B): write={write_time:.2f}ms, read={read_time:.2f}ms")

            except Exception as e:
                files[i].fail_count += 1
                errors.append(f"{name}: {e}")
                print(f"  {name}: ERROR - {e}")

    # Destroy sandbox
    print("\nDestroying sandbox...")
    start = time.perf_counter()
    try:
        sandbox.destroy()
    except Exception as e:
        errors.append(f"Destroy error: {e}")
    teardown_time_ms = (time.perf_counter() - start) * 1000
    print(f"  Teardown time: {teardown_time_ms:.2f} ms")

    result = FilesResult(
        provider=provider,
        iterations=iterations,
        files=files,
        setup_time_ms=setup_time_ms,
        teardown_time_ms=teardown_time_ms,
        errors=errors,
    )

    # Summary
    print(f"\n--- Summary for {provider} ---")
    print(f"{'Size':<10} {'Write Avg':<12} {'Write KB/s':<12} {'Read Avg':<12} {'Read KB/s':<12}")
    print("-" * 60)
    for f in files:
        print(f"{f.name:<10} {f.avg_write_ms:<12.2f} {f.write_throughput_kbps:<12.2f} "
              f"{f.avg_read_ms:<12.2f} {f.read_throughput_kbps:<12.2f}")

    return result


if __name__ == "__main__":
    import argparse
    import json

    parser = argparse.ArgumentParser(description="Benchmark file operations")
    parser.add_argument("--provider", choices=["opensandbox", "opensandbox-http", "opensandbox-grpc", "e2b"], default="opensandbox-grpc")
    parser.add_argument("--iterations", type=int, default=3)
    parser.add_argument("--opensandbox-url", default="https://opensandbox-test.fly.dev")

    args = parser.parse_args()

    result = run_benchmark(
        args.provider,
        iterations=args.iterations,
        base_url=args.opensandbox_url
    )

    print("\n\nFinal Results (JSON):")
    print(json.dumps(result.to_dict(), indent=2))
