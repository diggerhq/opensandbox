#!/usr/bin/env python3
"""
Test script for OpenSandbox + Claude Agent SDK integration.

This script demonstrates two approaches:
1. Running Claude Agent SDK directly (tools execute locally)
2. Using OpenSandbox to isolate code execution
"""

import asyncio
import os
from typing import Optional


async def test_claude_sdk_direct():
    """Test Claude Agent SDK running directly (no sandbox)."""
    try:
        from claude_code_sdk import query, ClaudeCodeOptions

        print("=" * 60)
        print("Test 1: Claude Agent SDK (Direct Execution)")
        print("=" * 60)

        api_key = os.environ.get("ANTHROPIC_API_KEY")
        if not api_key:
            print("Skipped: ANTHROPIC_API_KEY not set\n")
            return

        prompt = "What is 2 + 2? Reply with just the number."

        print(f"Prompt: {prompt}\n")
        print("Response:")

        async for message in query(
            prompt=prompt,
            options=ClaudeCodeOptions(
                max_turns=3,
            )
        ):
            if hasattr(message, 'content'):
                print(message.content)
            elif hasattr(message, 'result'):
                print(f"Result: {message.result}")

        print("\n[Direct execution test completed]\n")

    except ImportError as e:
        print("=" * 60)
        print("Test 1: Claude Agent SDK (Direct Execution)")
        print("=" * 60)
        print(f"Claude Agent SDK not available: {e}")
        print("Make sure claude-code-sdk is installed.\n")
    except Exception as e:
        print(f"Error during Claude SDK test: {e}\n")


async def test_opensandbox_connection():
    """Test basic OpenSandbox connectivity."""
    try:
        from opensandbox import OpenSandbox

        print("=" * 60)
        print("Test 2: OpenSandbox Connection")
        print("=" * 60)

        sandbox_url = os.environ.get("OPENSANDBOX_URL", "http://localhost:8080")
        grpc_port = int(os.environ.get("OPENSANDBOX_GRPC_PORT", "50051"))

        print(f"Connecting to: {sandbox_url} (gRPC port: {grpc_port})\n")

        async with OpenSandbox(sandbox_url, grpc_port=grpc_port) as client:
            # Create a sandbox session
            sandbox = await client.create(
                env={"TEST_VAR": "hello_from_sandbox"}
            )

            print(f"Created sandbox session: {sandbox.session_id}")

            # Run a simple command
            result = await sandbox.run("echo $TEST_VAR && uname -a")
            print(f"Command output:\n{result.stdout}")

            if result.stderr:
                print(f"Stderr: {result.stderr}")

            print(f"Exit code: {result.exit_code}")

            # Test file operations
            print("\nTesting file operations...")
            await sandbox.write_file("/tmp/test.txt", "Hello from OpenSandbox!")
            content = await sandbox.read_file_text("/tmp/test.txt")
            print(f"File content: {content}")

            # Clean up
            await sandbox.destroy()
            print("\n[Sandbox session destroyed]\n")

    except ImportError as e:
        print("=" * 60)
        print("Test 2: OpenSandbox Connection")
        print("=" * 60)
        print(f"OpenSandbox SDK not available: {e}\n")
    except Exception as e:
        print("=" * 60)
        print("Test 2: OpenSandbox Connection")
        print("=" * 60)
        print(f"OpenSandbox test failed: {e}\n")
        import traceback
        traceback.print_exc()


async def test_claude_in_sandbox():
    """
    Test running Claude Agent SDK with tools executing inside OpenSandbox.

    This is the advanced use case where:
    - Claude Agent SDK runs in the host/orchestrator
    - Tool execution (Bash, file ops) happens inside the sandbox
    """
    try:
        from opensandbox import OpenSandbox

        print("=" * 60)
        print("Test 3: Check Claude Code in Sandbox")
        print("=" * 60)
        print("Checking if Claude Code CLI is available in the sandbox...\n")

        sandbox_url = os.environ.get("OPENSANDBOX_URL", "http://localhost:8080")
        grpc_port = int(os.environ.get("OPENSANDBOX_GRPC_PORT", "50051"))

        async with OpenSandbox(sandbox_url, grpc_port=grpc_port) as client:
            sandbox = await client.create()

            print(f"Created sandbox session: {sandbox.session_id}")

            # Check available tools
            result = await sandbox.run("which python3 git curl")
            print(f"Available tools:\n{result.stdout}")

            # Check if Claude Code could be installed
            result = await sandbox.run("which node npm 2>/dev/null || echo 'Node.js not installed'")
            print(f"Node.js status: {result.stdout.strip()}")

            await sandbox.destroy()
            print("\n[Integration test completed]\n")

    except ImportError as e:
        print("=" * 60)
        print("Test 3: Check Claude Code in Sandbox")
        print("=" * 60)
        print(f"OpenSandbox SDK not available: {e}\n")
    except Exception as e:
        print("=" * 60)
        print("Test 3: Check Claude Code in Sandbox")
        print("=" * 60)
        print(f"Integration test failed: {e}\n")


async def main():
    """Run all tests."""
    print("\n" + "=" * 60)
    print("OpenSandbox + Claude Agent SDK Test Suite")
    print("=" * 60 + "\n")

    # Check for required environment variables
    api_key = os.environ.get("ANTHROPIC_API_KEY")
    if not api_key:
        print("Note: ANTHROPIC_API_KEY not set. Claude SDK tests will be skipped.\n")

    # Run tests
    await test_opensandbox_connection()
    await test_claude_sdk_direct()
    await test_claude_in_sandbox()

    print("=" * 60)
    print("All tests completed!")
    print("=" * 60)


if __name__ == "__main__":
    asyncio.run(main())
