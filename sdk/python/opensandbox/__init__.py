"""OpenSandbox Python SDK.

A high-performance Python client for OpenSandbox, using gRPC for
fast command execution and file operations.

Usage:
    from opensandbox import OpenSandbox

    async with OpenSandbox("https://opensandbox.fly.dev") as client:
        sandbox = await client.create()

        result = await sandbox.run("echo hello")
        print(result.stdout)

        await sandbox.write_file("/tmp/test.py", "print('hello')")
        content = await sandbox.read_file("/tmp/test.py")

        await sandbox.destroy()
"""

from .client import OpenSandbox
from .sandbox import Sandbox, CommandResult
from .exceptions import (
    OpenSandboxError,
    SandboxNotFoundError,
    SandboxConnectionError,
    CommandExecutionError,
    FileOperationError,
)

__version__ = "0.1.0"

__all__ = [
    "OpenSandbox",
    "Sandbox",
    "CommandResult",
    "OpenSandboxError",
    "SandboxNotFoundError",
    "SandboxConnectionError",
    "CommandExecutionError",
    "FileOperationError",
]
