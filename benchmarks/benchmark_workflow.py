#!/usr/bin/env python3
"""Benchmark: Realistic workflow (git clone, edit, test)."""

import time
import statistics
from typing import Optional
from dataclasses import dataclass, field

from sandbox_interface import get_sandbox, BaseSandbox


# Small public repos for testing git clone performance
TEST_REPOS = [
    ("tiny", "https://github.com/kelseyhightower/nocode", "~0 files"),
    ("small", "https://github.com/jlevy/the-art-of-command-line", "~10 files"),
    # You can add more repos here, but be mindful of rate limits
]


@dataclass
class WorkflowStep:
    """Timing for a workflow step."""
    name: str
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
            "avg_ms": round(self.avg_ms, 2),
            "std_ms": round(self.std_ms, 2),
            "min_ms": round(min(self.times_ms), 2) if self.times_ms else 0,
            "max_ms": round(max(self.times_ms), 2) if self.times_ms else 0,
            "success": self.success_count,
            "fail": self.fail_count,
        }


@dataclass
class WorkflowResult:
    """Results from workflow benchmark."""
    provider: str
    iterations: int
    steps: dict[str, WorkflowStep]
    total_times_ms: list[float]
    errors: list[str]

    @property
    def avg_total_ms(self) -> float:
        return statistics.mean(self.total_times_ms) if self.total_times_ms else 0

    def to_dict(self) -> dict:
        return {
            "provider": self.provider,
            "iterations": self.iterations,
            "total_avg_ms": round(self.avg_total_ms, 2),
            "total_times_ms": [round(t, 2) for t in self.total_times_ms],
            "steps": {name: step.to_dict() for name, step in self.steps.items()},
            "errors": self.errors,
        }


def run_workflow(sandbox: BaseSandbox, repo_url: str) -> dict[str, float]:
    """
    Run a single workflow iteration.

    Returns dict of step_name -> duration_ms
    """
    timings = {}

    # Step 1: Create directory
    start = time.perf_counter()
    result = sandbox.run_command("mkdir -p /tmp/workspace && cd /tmp/workspace && rm -rf repo")
    timings["mkdir"] = (time.perf_counter() - start) * 1000
    if result.exit_code != 0:
        raise RuntimeError(f"mkdir failed: {result.stderr}")

    # Step 2: Git clone (shallow)
    start = time.perf_counter()
    result = sandbox.run_command(f"cd /tmp/workspace && git clone --depth 1 {repo_url} repo 2>&1")
    timings["git_clone"] = (time.perf_counter() - start) * 1000
    if result.exit_code != 0:
        raise RuntimeError(f"git clone failed: {result.stderr}")

    # Step 3: List files
    start = time.perf_counter()
    result = sandbox.run_command("cd /tmp/workspace/repo && find . -type f | head -50")
    timings["list_files"] = (time.perf_counter() - start) * 1000
    if result.exit_code != 0:
        raise RuntimeError(f"list files failed: {result.stderr}")

    # Step 4: Create a new file
    start = time.perf_counter()
    sandbox.write_file("/tmp/workspace/repo/benchmark_test.txt", "This is a benchmark test file.\n" * 100)
    timings["write_file"] = (time.perf_counter() - start) * 1000

    # Step 5: Read README (if exists)
    start = time.perf_counter()
    result = sandbox.run_command("cd /tmp/workspace/repo && cat README.md 2>/dev/null || cat README 2>/dev/null || echo 'no readme'")
    timings["read_readme"] = (time.perf_counter() - start) * 1000

    # Step 6: Git status
    start = time.perf_counter()
    result = sandbox.run_command("cd /tmp/workspace/repo && git status")
    timings["git_status"] = (time.perf_counter() - start) * 1000

    # Step 7: Git diff
    start = time.perf_counter()
    result = sandbox.run_command("cd /tmp/workspace/repo && git diff --stat")
    timings["git_diff"] = (time.perf_counter() - start) * 1000

    # Step 8: Cleanup
    start = time.perf_counter()
    result = sandbox.run_command("rm -rf /tmp/workspace/repo")
    timings["cleanup"] = (time.perf_counter() - start) * 1000

    return timings


def run_benchmark(provider: str, iterations: int = 3, **kwargs) -> WorkflowResult:
    """
    Benchmark a realistic git workflow.

    Args:
        provider: "opensandbox" or "e2b"
        iterations: Number of complete workflow runs

    Returns:
        WorkflowResult with timing data
    """
    print(f"\n{'='*60}")
    print(f"Workflow Benchmark: {provider}")
    print(f"{'='*60}")

    errors = []
    step_names = ["sandbox_create", "mkdir", "git_clone", "list_files",
                  "write_file", "read_readme", "git_status", "git_diff",
                  "cleanup", "sandbox_destroy"]
    steps = {name: WorkflowStep(name=name) for name in step_names}
    total_times = []

    # Use the first (smallest) test repo
    repo_name, repo_url, repo_desc = TEST_REPOS[0]
    print(f"\nUsing repo: {repo_name} ({repo_url})")
    print(f"Description: {repo_desc}")

    for iteration in range(iterations):
        print(f"\n--- Iteration {iteration + 1}/{iterations} ---")

        sandbox: Optional[BaseSandbox] = None
        iteration_start = time.perf_counter()

        try:
            # Create sandbox
            sandbox = get_sandbox(provider, **kwargs)
            start = time.perf_counter()
            sandbox.create(timeout=300)
            create_time = (time.perf_counter() - start) * 1000
            steps["sandbox_create"].times_ms.append(create_time)
            steps["sandbox_create"].success_count += 1
            print(f"  sandbox_create: {create_time:.2f} ms")

            # Run workflow
            timings = run_workflow(sandbox, repo_url)

            for step_name, duration_ms in timings.items():
                steps[step_name].times_ms.append(duration_ms)
                steps[step_name].success_count += 1
                print(f"  {step_name}: {duration_ms:.2f} ms")

            # Destroy sandbox
            start = time.perf_counter()
            sandbox.destroy()
            destroy_time = (time.perf_counter() - start) * 1000
            steps["sandbox_destroy"].times_ms.append(destroy_time)
            steps["sandbox_destroy"].success_count += 1
            print(f"  sandbox_destroy: {destroy_time:.2f} ms")
            sandbox = None

            total_time = (time.perf_counter() - iteration_start) * 1000
            total_times.append(total_time)
            print(f"  TOTAL: {total_time:.2f} ms")

        except Exception as e:
            error_msg = f"Iteration {iteration + 1}: {str(e)}"
            errors.append(error_msg)
            print(f"  ERROR: {e}")
            if sandbox:
                try:
                    sandbox.destroy()
                except Exception:
                    pass

    result = WorkflowResult(
        provider=provider,
        iterations=iterations,
        steps=steps,
        total_times_ms=total_times,
        errors=errors,
    )

    # Summary
    print(f"\n--- Summary for {provider} ---")
    print(f"{'Step':<20} {'Avg (ms)':<12} {'Std (ms)':<12} {'Min':<10} {'Max':<10}")
    print("-" * 64)
    for step_name in step_names:
        step = steps[step_name]
        if step.times_ms:
            print(f"{step.name:<20} {step.avg_ms:<12.2f} {step.std_ms:<12.2f} "
                  f"{min(step.times_ms):<10.2f} {max(step.times_ms):<10.2f}")
    print("-" * 64)
    print(f"{'TOTAL':<20} {result.avg_total_ms:<12.2f}")

    return result


if __name__ == "__main__":
    import argparse
    import json

    parser = argparse.ArgumentParser(description="Benchmark realistic workflow")
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
