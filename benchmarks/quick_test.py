#!/usr/bin/env python3
"""
Quick test to verify sandbox connectivity before running full benchmarks.

Usage:
    python quick_test.py                    # Test opensandbox on localhost
    python quick_test.py --provider e2b     # Test e2b (requires E2B_API_KEY)
"""

import argparse
import os
import sys
import time

from sandbox_interface import get_sandbox


def test_provider(provider: str, **kwargs) -> bool:
    """Test a single provider with basic operations."""
    print(f"\n{'='*50}")
    print(f"Testing {provider}")
    print(f"{'='*50}")

    sandbox = None
    success = True

    try:
        # Create
        print("\n1. Creating sandbox...")
        sandbox = get_sandbox(provider, **kwargs)
        start = time.perf_counter()
        session_id = sandbox.create(timeout=60)
        create_time = (time.perf_counter() - start) * 1000
        print(f"   OK - Session: {session_id[:20]}... ({create_time:.0f}ms)")

        # Run command
        print("\n2. Running 'echo hello'...")
        start = time.perf_counter()
        result = sandbox.run_command("echo 'Hello from sandbox!'")
        cmd_time = (time.perf_counter() - start) * 1000
        if result.exit_code == 0:
            print(f"   OK - Output: {result.stdout.strip()} ({cmd_time:.0f}ms)")
        else:
            print(f"   FAIL - exit_code={result.exit_code}, stderr={result.stderr}")
            success = False

        # Write file
        print("\n3. Writing file...")
        start = time.perf_counter()
        sandbox.write_file("/tmp/test.txt", "Hello, World!")
        write_time = (time.perf_counter() - start) * 1000
        print(f"   OK - Wrote /tmp/test.txt ({write_time:.0f}ms)")

        # Read file
        print("\n4. Reading file...")
        start = time.perf_counter()
        content, _ = sandbox.read_file("/tmp/test.txt")
        read_time = (time.perf_counter() - start) * 1000
        if "Hello" in content:
            print(f"   OK - Content: {content.strip()} ({read_time:.0f}ms)")
        else:
            print(f"   FAIL - Unexpected content: {content}")
            success = False

        # System info
        print("\n5. Getting system info...")
        result = sandbox.run_command("uname -a")
        print(f"   {result.stdout.strip()}")

        # Destroy
        print("\n6. Destroying sandbox...")
        start = time.perf_counter()
        sandbox.destroy()
        destroy_time = (time.perf_counter() - start) * 1000
        sandbox = None
        print(f"   OK ({destroy_time:.0f}ms)")

    except Exception as e:
        print(f"\n   ERROR: {e}")
        success = False

    finally:
        if sandbox:
            try:
                sandbox.destroy()
            except Exception:
                pass

    print(f"\n{'='*50}")
    print(f"Result: {'PASS' if success else 'FAIL'}")
    print(f"{'='*50}")

    return success


def main():
    parser = argparse.ArgumentParser(description="Quick sandbox connectivity test")
    parser.add_argument(
        "--provider",
        choices=["opensandbox", "e2b", "all"],
        default="opensandbox",
        help="Provider to test"
    )
    parser.add_argument(
        "--opensandbox-url",
        default=os.environ.get("OPENSANDBOX_URL", "https://opensandbox-test.fly.dev"),
        help="OpenSandbox URL"
    )

    args = parser.parse_args()

    providers = []
    if args.provider in ["opensandbox", "all"]:
        providers.append(("opensandbox", {"base_url": args.opensandbox_url}))
    if args.provider in ["e2b", "all"]:
        if not os.environ.get("E2B_API_KEY"):
            print("WARNING: E2B_API_KEY not set, skipping e2b test")
        else:
            providers.append(("e2b", {}))

    if not providers:
        print("No providers to test!")
        sys.exit(1)

    all_passed = True
    for provider, kwargs in providers:
        if not test_provider(provider, **kwargs):
            all_passed = False

    sys.exit(0 if all_passed else 1)


if __name__ == "__main__":
    main()
