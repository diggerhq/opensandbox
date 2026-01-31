#!/usr/bin/env python3
"""Benchmark: Command execution latency."""

import time
import statistics
from typing import Optional
from dataclasses import dataclass, field

from sandbox_interface import get_sandbox, BaseSandbox


# Test commands to benchmark (name, command, description)
TEST_COMMANDS = [
    ("echo", "echo 'hello world'", "Simple echo command"),
    ("pwd", "pwd", "Print working directory"),
    ("ls", "ls -la /", "List root directory"),
    ("env", "env | head -20", "Show environment variables"),
    ("python_version", "python3 --version 2>&1 || python --version 2>&1 || echo 'no python'", "Check Python version"),
    ("git_version", "git --version", "Check Git version"),
    ("uname", "uname -a", "System information"),
    ("cat_etc_os", "cat /etc/os-release 2>/dev/null || cat /etc/debian_version 2>/dev/null || echo 'unknown'", "OS release info"),
    ("loop_100", "for i in $(seq 1 100); do echo $i; done | tail -1", "Loop 100 iterations"),
    ("calculate", "echo $((12345 * 67890))", "Simple calculation"),
]


@dataclass
class CommandBenchmark:
    """Results for a single command."""
    name: str
    command: str
    times_ms: list[float] = field(default_factory=list)
    success_count: int = 0
    fail_count: int = 0

    @property
    def avg_ms(self) -> float:
        return statistics.mean(self.times_ms) if self.times_ms else 0

    @property
    def std_ms(self) -> float:
        return statistics.stdev(self.times_ms) if len(self.times_ms) > 1 else 0

    def to_dict(self) -> dict:
        return {
            "name": self.name,
            "command": self.command,
            "avg_ms": round(self.avg_ms, 2),
            "std_ms": round(self.std_ms, 2),
            "min_ms": round(min(self.times_ms), 2) if self.times_ms else 0,
            "max_ms": round(max(self.times_ms), 2) if self.times_ms else 0,
            "success": self.success_count,
            "fail": self.fail_count,
        }


@dataclass
class CommandsResult:
    """Results from command benchmark."""
    provider: str
    iterations: int
    commands: list[CommandBenchmark]
    setup_time_ms: float
    teardown_time_ms: float
    errors: list[str]

    @property
    def total_avg_ms(self) -> float:
        """Average execution time across all commands."""
        all_times = []
        for cmd in self.commands:
            all_times.extend(cmd.times_ms)
        return statistics.mean(all_times) if all_times else 0

    def to_dict(self) -> dict:
        return {
            "provider": self.provider,
            "iterations": self.iterations,
            "setup_time_ms": round(self.setup_time_ms, 2),
            "teardown_time_ms": round(self.teardown_time_ms, 2),
            "total_avg_ms": round(self.total_avg_ms, 2),
            "commands": [cmd.to_dict() for cmd in self.commands],
            "errors": self.errors,
        }


def run_benchmark(provider: str, iterations: int = 3, **kwargs) -> CommandsResult:
    """
    Benchmark command execution latency.

    Args:
        provider: "opensandbox" or "e2b"
        iterations: Number of times to run each command

    Returns:
        CommandsResult with timing data
    """
    print(f"\n{'='*60}")
    print(f"Command Execution Benchmark: {provider}")
    print(f"{'='*60}")

    errors = []
    commands = [CommandBenchmark(name=name, command=cmd) for name, cmd, _ in TEST_COMMANDS]

    # Create sandbox
    sandbox: Optional[BaseSandbox] = None
    try:
        sandbox = get_sandbox(provider, **kwargs)
        print("\nCreating sandbox...")
        start = time.perf_counter()
        sandbox.create(timeout=300)
        setup_time_ms = (time.perf_counter() - start) * 1000
        print(f"  Setup time: {setup_time_ms:.2f} ms")
    except Exception as e:
        errors.append(f"Failed to create sandbox: {e}")
        print(f"  ERROR: {e}")
        return CommandsResult(
            provider=provider,
            iterations=iterations,
            commands=commands,
            setup_time_ms=0,
            teardown_time_ms=0,
            errors=errors,
        )

    # Run benchmarks
    print(f"\nRunning {len(TEST_COMMANDS)} commands x {iterations} iterations...")

    for iteration in range(iterations):
        print(f"\n--- Iteration {iteration + 1}/{iterations} ---")

        for i, (name, cmd, desc) in enumerate(TEST_COMMANDS):
            try:
                result = sandbox.run_command(cmd)
                commands[i].times_ms.append(result.duration_ms)
                if result.exit_code == 0:
                    commands[i].success_count += 1
                else:
                    commands[i].fail_count += 1
                print(f"  {name}: {result.duration_ms:.2f} ms (exit={result.exit_code})")
            except Exception as e:
                commands[i].fail_count += 1
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

    result = CommandsResult(
        provider=provider,
        iterations=iterations,
        commands=commands,
        setup_time_ms=setup_time_ms,
        teardown_time_ms=teardown_time_ms,
        errors=errors,
    )

    # Summary
    print(f"\n--- Summary for {provider} ---")
    print(f"{'Command':<20} {'Avg (ms)':<12} {'Std (ms)':<12} {'Min':<10} {'Max':<10}")
    print("-" * 64)
    for cmd in commands:
        print(f"{cmd.name:<20} {cmd.avg_ms:<12.2f} {cmd.std_ms:<12.2f} "
              f"{min(cmd.times_ms) if cmd.times_ms else 0:<10.2f} "
              f"{max(cmd.times_ms) if cmd.times_ms else 0:<10.2f}")
    print("-" * 64)
    print(f"{'OVERALL':<20} {result.total_avg_ms:<12.2f}")

    return result


if __name__ == "__main__":
    import argparse
    import json

    parser = argparse.ArgumentParser(description="Benchmark command execution latency")
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
