import asyncio
import sys

sys.path.insert(0, "../sdks/python")

from opencomputer import Sandbox


async def main():
    print("Creating sandbox...")
    async with await Sandbox.create(
        template="base",
        timeout=3600,
        api_url="https://app.opencomputer.dev",
    ) as sb:
        print(f"Sandbox created: {sb.sandbox_id}")

        # Run commands
        print("\n--- Commands ---")
        result = await sb.commands.run('echo "hello from python sdk"')
        print(f"stdout: {result.stdout.strip()}")
        print(f"exit code: {result.exit_code}")

        uname = await sb.commands.run("uname -a")
        print(f"uname: {uname.stdout.strip()}")

        # Filesystem
        print("\n--- Filesystem ---")
        await sb.files.write("/tmp/greeting.txt", "Hello from Python SDK!")
        content = await sb.files.read("/tmp/greeting.txt")
        print(f"file content: {content}")

        exists = await sb.files.exists("/tmp/greeting.txt")
        print(f"file exists: {exists}")

        await sb.files.make_dir("/tmp/mydir")
        await sb.files.write("/tmp/mydir/test.py", 'print("hello from python")')

        entries = await sb.files.list("/tmp")
        print("ls /tmp:")
        for entry in entries:
            kind = "d" if entry.is_dir else "-"
            print(f"  {kind} {entry.name}")

        # Run a multi-line script
        print("\n--- Script execution ---")
        await sb.files.write(
            "/tmp/script.sh",
            "\n".join(
                [
                    "#!/bin/bash",
                    'echo "Current directory: $(pwd)"',
                    'echo "User: $(whoami)"',
                    'echo "Date: $(date)"',
                    'echo "Files in /tmp:"',
                    "ls /tmp",
                ]
            ),
        )

        script = await sb.commands.run("bash /tmp/script.sh")
        print(script.stdout)

        # Run Python inside the sandbox
        print("--- Python in sandbox ---")
        await sb.files.write(
            "/tmp/hello.py",
            "\n".join(
                [
                    "import os",
                    "print(f'PID: {os.getpid()}')",
                    "print(f'CWD: {os.getcwd()}')",
                    "print('2 + 2 =', 2 + 2)",
                ]
            ),
        )
        py = await sb.commands.run("python /tmp/hello.py")
        if py.exit_code == 0:
            print(py.stdout)
        else:
            print(f"python3 not available in base image (exit {py.exit_code})")

        # Check sandbox status
        print("--- Status ---")
        running = await sb.is_running()
        print(f"running: {running}")

    # sandbox auto-killed by async with
    print("\nSandbox killed. Done!")


asyncio.run(main())
