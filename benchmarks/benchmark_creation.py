#!/usr/bin/env python3
"""Benchmark: Sandbox creation and destruction time."""

import time
import statistics
from typing import Optional
from dataclasses import dataclass

from sandbox_interface import get_sandbox, BaseSandbox


@dataclass
class CreationResult:
    """Results from creation benchmark."""
    provider: str
    create_times_ms: list[float]
    destroy_times_ms: list[float]
    errors: list[str]

    @property
    def avg_create_ms(self) -> float:
        return statistics.mean(self.create_times_ms) if self.create_times_ms else 0

    @property
    def avg_destroy_ms(self) -> float:
        return statistics.mean(self.destroy_times_ms) if self.destroy_times_ms else 0

    @property
    def std_create_ms(self) -> float:
        return statistics.stdev(self.create_times_ms) if len(self.create_times_ms) > 1 else 0

    @property
    def std_destroy_ms(self) -> float:
        return statistics.stdev(self.destroy_times_ms) if len(self.destroy_times_ms) > 1 else 0

    def to_dict(self) -> dict:
        return {
            "provider": self.provider,
            "iterations": len(self.create_times_ms),
            "create": {
                "avg_ms": round(self.avg_create_ms, 2),
                "std_ms": round(self.std_create_ms, 2),
                "min_ms": round(min(self.create_times_ms), 2) if self.create_times_ms else 0,
                "max_ms": round(max(self.create_times_ms), 2) if self.create_times_ms else 0,
                "all_ms": [round(t, 2) for t in self.create_times_ms],
            },
            "destroy": {
                "avg_ms": round(self.avg_destroy_ms, 2),
                "std_ms": round(self.std_destroy_ms, 2),
                "min_ms": round(min(self.destroy_times_ms), 2) if self.destroy_times_ms else 0,
                "max_ms": round(max(self.destroy_times_ms), 2) if self.destroy_times_ms else 0,
                "all_ms": [round(t, 2) for t in self.destroy_times_ms],
            },
            "errors": self.errors,
        }


def run_benchmark(provider: str, iterations: int = 3, **kwargs) -> CreationResult:
    """
    Benchmark sandbox creation and destruction time.

    Args:
        provider: "opensandbox" or "e2b"
        iterations: Number of create/destroy cycles

    Returns:
        CreationResult with timing data
    """
    create_times = []
    destroy_times = []
    errors = []

    print(f"\n{'='*60}")
    print(f"Creation Benchmark: {provider}")
    print(f"{'='*60}")

    for i in range(iterations):
        print(f"\nIteration {i+1}/{iterations}...")

        sandbox: Optional[BaseSandbox] = None
        try:
            # Measure creation time
            sandbox = get_sandbox(provider, **kwargs)
            start = time.perf_counter()
            sandbox.create(timeout=300)
            create_time_ms = (time.perf_counter() - start) * 1000
            create_times.append(create_time_ms)
            print(f"  Create: {create_time_ms:.2f} ms")

            # Measure destruction time
            start = time.perf_counter()
            sandbox.destroy()
            destroy_time_ms = (time.perf_counter() - start) * 1000
            destroy_times.append(destroy_time_ms)
            print(f"  Destroy: {destroy_time_ms:.2f} ms")
            sandbox = None

        except Exception as e:
            error_msg = f"Iteration {i+1}: {str(e)}"
            errors.append(error_msg)
            print(f"  ERROR: {e}")
            if sandbox:
                try:
                    sandbox.destroy()
                except Exception:
                    pass

    result = CreationResult(
        provider=provider,
        create_times_ms=create_times,
        destroy_times_ms=destroy_times,
        errors=errors,
    )

    print(f"\n--- Summary for {provider} ---")
    print(f"Create:  avg={result.avg_create_ms:.2f}ms, std={result.std_create_ms:.2f}ms")
    print(f"Destroy: avg={result.avg_destroy_ms:.2f}ms, std={result.std_destroy_ms:.2f}ms")

    return result


if __name__ == "__main__":
    import argparse

    parser = argparse.ArgumentParser(description="Benchmark sandbox creation time")
    parser.add_argument("--provider", choices=["opensandbox", "opensandbox-http", "opensandbox-grpc", "e2b"], default="opensandbox-grpc")
    parser.add_argument("--iterations", type=int, default=3)
    parser.add_argument("--opensandbox-url", default="https://opensandbox-test.fly.dev")

    args = parser.parse_args()

    result = run_benchmark(
        args.provider,
        iterations=args.iterations,
        base_url=args.opensandbox_url
    )

    print("\n\nFinal Results:")
    import json
    print(json.dumps(result.to_dict(), indent=2))
