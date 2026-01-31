#!/usr/bin/env python3
"""
Main benchmark runner for OpenSandbox vs E2B comparison.

Usage:
    python run_benchmarks.py                    # Run all benchmarks for both providers
    python run_benchmarks.py --provider opensandbox  # Only test opensandbox
    python run_benchmarks.py --only creation commands  # Only run specific benchmarks
    python run_benchmarks.py --iterations 5     # Run 5 iterations per test
"""

import argparse
import json
import os
import sys
from datetime import datetime
from pathlib import Path

# Import benchmarks
import benchmark_creation
import benchmark_commands
import benchmark_files
import benchmark_workflow
import benchmark_concurrency


BENCHMARKS = {
    "creation": benchmark_creation,
    "commands": benchmark_commands,
    "files": benchmark_files,
    "workflow": benchmark_workflow,
    "concurrency": benchmark_concurrency,
}


def check_opensandbox_available(url: str) -> bool:
    """Check if OpenSandbox server is running."""
    import httpx
    try:
        response = httpx.get(f"{url}/health", timeout=5)
        return response.status_code == 200
    except Exception:
        return False


def check_e2b_available() -> bool:
    """Check if E2B API key is configured."""
    api_key = os.environ.get("E2B_API_KEY", "")
    return len(api_key) > 10


def generate_markdown_report(results: dict, output_path: Path) -> str:
    """Generate a markdown report from benchmark results."""
    lines = [
        "# Sandbox Benchmark Results",
        "",
        f"**Date:** {results['timestamp']}",
        f"**Iterations:** {results['iterations']}",
        "",
    ]

    providers = list(results.get("providers", {}).keys())

    # Creation benchmark
    if "creation" in results.get("benchmarks", {}):
        lines.extend([
            "## 1. Sandbox Creation Time",
            "",
            "| Provider | Create Avg (ms) | Create Std | Destroy Avg (ms) |",
            "|----------|-----------------|------------|------------------|",
        ])
        for provider in providers:
            data = results["benchmarks"]["creation"].get(provider, {})
            if data:
                create = data.get("create", {})
                destroy = data.get("destroy", {})
                lines.append(
                    f"| {provider} | {create.get('avg_ms', 'N/A')} | "
                    f"{create.get('std_ms', 'N/A')} | {destroy.get('avg_ms', 'N/A')} |"
                )
        lines.append("")

    # Commands benchmark
    if "commands" in results.get("benchmarks", {}):
        lines.extend([
            "## 2. Command Execution Latency",
            "",
            "| Command | " + " | ".join([f"{p} (ms)" for p in providers]) + " |",
            "|---------|" + "|".join(["----------" for _ in providers]) + "|",
        ])

        # Get command names from first available provider
        cmd_names = []
        for provider in providers:
            data = results["benchmarks"]["commands"].get(provider, {})
            if data and "commands" in data:
                cmd_names = [c["name"] for c in data["commands"]]
                break

        for cmd_name in cmd_names:
            row = [cmd_name]
            for provider in providers:
                data = results["benchmarks"]["commands"].get(provider, {})
                cmd_data = next(
                    (c for c in data.get("commands", []) if c["name"] == cmd_name),
                    {}
                )
                row.append(str(cmd_data.get("avg_ms", "N/A")))
            lines.append("| " + " | ".join(row) + " |")

        lines.extend(["", "### Overall Command Execution Average", ""])
        for provider in providers:
            data = results["benchmarks"]["commands"].get(provider, {})
            lines.append(f"- **{provider}:** {data.get('total_avg_ms', 'N/A')} ms")
        lines.append("")

    # Files benchmark
    if "files" in results.get("benchmarks", {}):
        lines.extend([
            "## 3. File Operations",
            "",
            "### Write Performance",
            "",
            "| Size | " + " | ".join([f"{p} (ms)" for p in providers]) + " |",
            "|------|" + "|".join(["----------" for _ in providers]) + "|",
        ])

        # Get file sizes from first available provider
        file_names = []
        for provider in providers:
            data = results["benchmarks"]["files"].get(provider, {})
            if data and "files" in data:
                file_names = [(f["name"], f["size_bytes"]) for f in data["files"]]
                break

        for file_name, size_bytes in file_names:
            row = [f"{file_name} ({size_bytes:,}B)"]
            for provider in providers:
                data = results["benchmarks"]["files"].get(provider, {})
                file_data = next(
                    (f for f in data.get("files", []) if f["name"] == file_name),
                    {}
                )
                row.append(str(file_data.get("write", {}).get("avg_ms", "N/A")))
            lines.append("| " + " | ".join(row) + " |")

        lines.extend([
            "",
            "### Read Performance",
            "",
            "| Size | " + " | ".join([f"{p} (ms)" for p in providers]) + " |",
            "|------|" + "|".join(["----------" for _ in providers]) + "|",
        ])

        for file_name, size_bytes in file_names:
            row = [f"{file_name} ({size_bytes:,}B)"]
            for provider in providers:
                data = results["benchmarks"]["files"].get(provider, {})
                file_data = next(
                    (f for f in data.get("files", []) if f["name"] == file_name),
                    {}
                )
                row.append(str(file_data.get("read", {}).get("avg_ms", "N/A")))
            lines.append("| " + " | ".join(row) + " |")
        lines.append("")

    # Workflow benchmark
    if "workflow" in results.get("benchmarks", {}):
        lines.extend([
            "## 4. Realistic Workflow (Git Clone + Edit)",
            "",
            "| Step | " + " | ".join([f"{p} (ms)" for p in providers]) + " |",
            "|------|" + "|".join(["----------" for _ in providers]) + "|",
        ])

        # Get step names from first available provider
        step_names = []
        for provider in providers:
            data = results["benchmarks"]["workflow"].get(provider, {})
            if data and "steps" in data:
                step_names = list(data["steps"].keys())
                break

        for step_name in step_names:
            row = [step_name]
            for provider in providers:
                data = results["benchmarks"]["workflow"].get(provider, {})
                step_data = data.get("steps", {}).get(step_name, {})
                row.append(str(step_data.get("avg_ms", "N/A")))
            lines.append("| " + " | ".join(row) + " |")

        lines.extend(["", "### Total Workflow Time", ""])
        for provider in providers:
            data = results["benchmarks"]["workflow"].get(provider, {})
            lines.append(f"- **{provider}:** {data.get('total_avg_ms', 'N/A')} ms")
        lines.append("")

    # Concurrency benchmark
    if "concurrency" in results.get("benchmarks", {}):
        lines.extend([
            "## 5. Concurrency (Parallel Sandboxes)",
            "",
            "| Concurrency | " + " | ".join([f"{p} Wall Time (ms)" for p in providers]) + " | " + " | ".join([f"{p} Throughput/s" for p in providers]) + " |",
            "|-------------|" + "|".join(["------------------" for _ in providers]) + "|" + "|".join(["------------------" for _ in providers]) + "|",
        ])

        # Get concurrency levels from first available provider
        levels = []
        for provider in providers:
            data = results["benchmarks"]["concurrency"].get(provider, {})
            if data and "concurrency_levels" in data:
                levels = data["concurrency_levels"]
                break

        for level in levels:
            row = [str(level)]
            for provider in providers:
                data = results["benchmarks"]["concurrency"].get(provider, {})
                level_data = data.get("results", {}).get(str(level), data.get("results", {}).get(level, {}))
                row.append(str(level_data.get("total_wall_time_ms", "N/A")))
            for provider in providers:
                data = results["benchmarks"]["concurrency"].get(provider, {})
                level_data = data.get("results", {}).get(str(level), data.get("results", {}).get(level, {}))
                row.append(str(level_data.get("throughput_per_sec", "N/A")))
            lines.append("| " + " | ".join(row) + " |")
        lines.append("")

    # Errors
    all_errors = []
    for bench_name, bench_data in results.get("benchmarks", {}).items():
        for provider, data in bench_data.items():
            errors = data.get("errors", [])
            for err in errors:
                all_errors.append(f"[{bench_name}/{provider}] {err}")

    if all_errors:
        lines.extend([
            "## Errors",
            "",
        ])
        for err in all_errors:
            lines.append(f"- {err}")
        lines.append("")

    report = "\n".join(lines)

    with open(output_path, "w") as f:
        f.write(report)

    return report


def main():
    parser = argparse.ArgumentParser(
        description="Run OpenSandbox vs E2B benchmarks",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  python run_benchmarks.py                         # Run all benchmarks
  python run_benchmarks.py --provider opensandbox  # Only test opensandbox
  python run_benchmarks.py --only creation         # Only creation benchmark
  python run_benchmarks.py --iterations 5          # 5 iterations per test
        """
    )

    parser.add_argument(
        "--provider",
        choices=["opensandbox", "e2b", "all"],
        default="all",
        help="Which provider to benchmark (default: all)"
    )
    parser.add_argument(
        "--only",
        nargs="+",
        choices=list(BENCHMARKS.keys()),
        default=list(BENCHMARKS.keys()),
        help="Which benchmarks to run (default: all)"
    )
    parser.add_argument(
        "--iterations",
        type=int,
        default=3,
        help="Number of iterations per benchmark (default: 3)"
    )
    parser.add_argument(
        "--opensandbox-url",
        default=os.environ.get("OPENSANDBOX_URL", "https://opensandbox-test.fly.dev"),
        help="OpenSandbox server URL"
    )
    parser.add_argument(
        "--output-dir",
        default="results",
        help="Output directory for results (default: results)"
    )

    args = parser.parse_args()

    # Determine which providers to test
    providers = []
    if args.provider in ["opensandbox", "all"]:
        if check_opensandbox_available(args.opensandbox_url):
            providers.append("opensandbox")
            print(f"✓ OpenSandbox available at {args.opensandbox_url}")
        else:
            print(f"✗ OpenSandbox not available at {args.opensandbox_url}")
            if args.provider == "opensandbox":
                print("  Start it with: docker compose up --build")
                sys.exit(1)

    if args.provider in ["e2b", "all"]:
        if check_e2b_available():
            providers.append("e2b")
            print("✓ E2B API key configured")
        else:
            print("✗ E2B API key not configured")
            if args.provider == "e2b":
                print("  Set it with: export E2B_API_KEY=your_key")
                sys.exit(1)

    if not providers:
        print("\nNo providers available. Please ensure at least one is configured.")
        sys.exit(1)

    print(f"\nRunning benchmarks for: {', '.join(providers)}")
    print(f"Benchmarks: {', '.join(args.only)}")
    print(f"Iterations: {args.iterations}")

    # Create output directory
    output_dir = Path(args.output_dir)
    output_dir.mkdir(exist_ok=True)

    # Initialize results
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    results = {
        "timestamp": datetime.now().isoformat(),
        "iterations": args.iterations,
        "providers": {p: {} for p in providers},
        "benchmarks": {},
    }

    # Run benchmarks
    for bench_name in args.only:
        bench_module = BENCHMARKS[bench_name]
        results["benchmarks"][bench_name] = {}

        print(f"\n{'#'*60}")
        print(f"# Running {bench_name} benchmark")
        print(f"{'#'*60}")

        for provider in providers:
            try:
                result = bench_module.run_benchmark(
                    provider,
                    iterations=args.iterations,
                    base_url=args.opensandbox_url
                )
                results["benchmarks"][bench_name][provider] = result.to_dict()
            except Exception as e:
                print(f"ERROR running {bench_name} for {provider}: {e}")
                results["benchmarks"][bench_name][provider] = {"error": str(e)}

    # Save results
    json_path = output_dir / f"benchmark_{timestamp}.json"
    with open(json_path, "w") as f:
        json.dump(results, f, indent=2)
    print(f"\n\nResults saved to: {json_path}")

    # Generate markdown report
    md_path = output_dir / f"benchmark_{timestamp}.md"
    report = generate_markdown_report(results, md_path)
    print(f"Report saved to: {md_path}")

    # Print summary
    print("\n" + "="*60)
    print("SUMMARY")
    print("="*60)
    print(report)


if __name__ == "__main__":
    main()
