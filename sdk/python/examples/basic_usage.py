#!/usr/bin/env python3
"""Basic usage example for the OpenSandbox SDK."""

import asyncio
from opensandbox import OpenSandbox


async def main():
    # Connect to OpenSandbox server
    async with OpenSandbox("https://opensandbox-test.fly.dev") as client:
        # Create a new sandbox
        sandbox = await client.create()
        print(f"Created sandbox: {sandbox.session_id}")

        try:
            # Run a simple command
            result = await sandbox.run("echo 'Hello, OpenSandbox!'")
            print(f"Command output: {result.stdout}")
            print(f"Exit code: {result.exit_code}")

            # Write a file
            await sandbox.write_file("/tmp/test.py", "print('Hello from Python!')")
            print("Wrote file: /tmp/test.py")

            # Read the file back
            content = await sandbox.read_file_text("/tmp/test.py")
            print(f"File content: {content}")

            # Execute the Python script
            result = await sandbox.run("python3 /tmp/test.py")
            print(f"Script output: {result.stdout}")

            # Set working directory
            await sandbox.set_cwd("/tmp")

            # Run command in the new directory
            result = await sandbox.run("pwd")
            print(f"Current directory: {result.stdout.strip()}")

        finally:
            # Clean up
            await sandbox.destroy()
            print("Sandbox destroyed")


if __name__ == "__main__":
    asyncio.run(main())
