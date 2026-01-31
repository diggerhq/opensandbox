#!/usr/bin/env python3
"""Benchmark: Concurrent sandbox operations."""

import time
import statistics
import concurrent.futures
from typing import Optional
from dataclasses import dataclass, field

from sandbox_interface import get_sandbox, BaseSandbox


@dataclass
class ConcurrencyResult:
    """Results from concurrency benchmark."""
    provider: str
    concurrency_levels: list[int]
    results: dict[int, dict]  # level -> {total_time, avg_per_sandbox, errors}
    errors: list[str]

    def to_dict(self) -> dict:
        return {
            "provider": self.provider,
            "concurrency_levels": self.concurrency_levels,
            "results": self.results,
            "errors": self.errors,
        }


def run_single_sandbox_workflow(provider: str, index: int, **kwargs) -> dict:
    """
    Run a single sandbox workflow.

    Returns timing info dict.
    """
    result = {
        "index": index,
        "success": False,
        "create_ms": 0,
        "command_ms": 0,
        "destroy_ms": 0,
        "total_ms": 0,
        "error": None,
    }

    sandbox: Optional[BaseSandbox] = None
    total_start = time.perf_counter()

    try:
        sandbox = get_sandbox(provider, **kwargs)

        # Create
        start = time.perf_counter()
        sandbox.create(timeout=300)
        result["create_ms"] = (time.perf_counter() - start) * 1000

        # Run commands
        start = time.perf_counter()
        sandbox.run_command("echo 'hello from sandbox'")
        sandbox.run_command("ls -la /")
        sandbox.run_command("uname -a")
        result["command_ms"] = (time.perf_counter() - start) * 1000

        # Destroy
        start = time.perf_counter()
        sandbox.destroy()
        result["destroy_ms"] = (time.perf_counter() - start) * 1000
        sandbox = None

        result["success"] = True

    except Exception as e:
        result["error"] = str(e)
        if sandbox:
            try:
                sandbox.destroy()
            except Exception:
                pass

    result["total_ms"] = (time.perf_counter() - total_start) * 1000
    return result


def run_benchmark(provider: str, iterations: int = 1, **kwargs) -> ConcurrencyResult:
    """
    Benchmark concurrent sandbox operations.

    Tests creating multiple sandboxes in parallel.

    Args:
        provider: "opensandbox" or "e2b"
        iterations: Not used for concurrency (uses predefined levels)

    Returns:
        ConcurrencyResult with timing data
    """
    print(f"\n{'='*60}")
    print(f"Concurrency Benchmark: {provider}")
    print(f"{'='*60}")

    # Test different concurrency levels
    concurrency_levels = [1, 2, 4, 8]
    errors = []
    results = {}

    for level in concurrency_levels:
        print(f"\n--- Testing {level} concurrent sandbox(es) ---")

        level_results = []
        total_start = time.perf_counter()

        with concurrent.futures.ThreadPoolExecutor(max_workers=level) as executor:
            futures = [
                executor.submit(run_single_sandbox_workflow, provider, i, **kwargs)
                for i in range(level)
            ]

            for future in concurrent.futures.as_completed(futures):
                try:
                    res = future.result()
                    level_results.append(res)
                    status = "OK" if res["success"] else f"FAIL: {res['error']}"
                    print(f"  Sandbox {res['index']}: {res['total_ms']:.2f}ms - {status}")
                except Exception as e:
                    errors.append(f"Level {level}: {e}")
                    print(f"  ERROR: {e}")

        total_time = (time.perf_counter() - total_start) * 1000

        # Calculate stats
        successful = [r for r in level_results if r["success"]]
        if successful:
            avg_create = statistics.mean([r["create_ms"] for r in successful])
            avg_command = statistics.mean([r["command_ms"] for r in successful])
            avg_destroy = statistics.mean([r["destroy_ms"] for r in successful])
            avg_total = statistics.mean([r["total_ms"] for r in successful])
        else:
            avg_create = avg_command = avg_destroy = avg_total = 0

        results[level] = {
            "concurrency": level,
            "total_wall_time_ms": round(total_time, 2),
            "successful": len(successful),
            "failed": level - len(successful),
            "avg_create_ms": round(avg_create, 2),
            "avg_command_ms": round(avg_command, 2),
            "avg_destroy_ms": round(avg_destroy, 2),
            "avg_total_ms": round(avg_total, 2),
            "throughput_per_sec": round(len(successful) / (total_time / 1000), 2) if total_time > 0 else 0,
        }

        print(f"\n  Total wall time: {total_time:.2f} ms")
        print(f"  Successful: {len(successful)}/{level}")
        print(f"  Avg per sandbox: {avg_total:.2f} ms")
        print(f"  Throughput: {results[level]['throughput_per_sec']:.2f} sandboxes/sec")

    result = ConcurrencyResult(
        provider=provider,
        concurrency_levels=concurrency_levels,
        results=results,
        errors=errors,
    )

    # Summary
    print(f"\n--- Summary for {provider} ---")
    print(f"{'Concurrency':<15} {'Wall Time':<15} {'Avg/Sandbox':<15} {'Throughput':<15}")
    print("-" * 60)
    for level in concurrency_levels:
        r = results[level]
        print(f"{level:<15} {r['total_wall_time_ms']:<15.2f} "
              f"{r['avg_total_ms']:<15.2f} {r['throughput_per_sec']:<15.2f}")

    return result


if __name__ == "__main__":
    import argparse
    import json

    parser = argparse.ArgumentParser(description="Benchmark concurrent sandbox operations")
    parser.add_argument("--provider", choices=["opensandbox", "e2b"], default="opensandbox")
    parser.add_argument("--opensandbox-url", default="https://opensandbox-test.fly.dev")

    args = parser.parse_args()

    result = run_benchmark(
        args.provider,
        base_url=args.opensandbox_url
    )

    print("\n\nFinal Results (JSON):")
    print(json.dumps(result.to_dict(), indent=2))
