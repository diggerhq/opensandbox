# OpenSandbox Python SDK

A high-performance Python client for [OpenSandbox](https://github.com/yourusername/opensandbox), using gRPC for fast command execution and file operations.

## Installation

```bash
pip install opensandbox
```

Or install from source:

```bash
cd sdk/python
pip install -e .
```

## Quick Start

```python
import asyncio
from opensandbox import OpenSandbox

async def main():
    async with OpenSandbox("https://opensandbox.fly.dev") as client:
        # Create a sandbox
        sandbox = await client.create()

        # Run commands
        result = await sandbox.run("echo hello")
        print(result.stdout)  # "hello\n"

        # Write and read files
        await sandbox.write_file("/tmp/test.py", "print('hi')")
        content = await sandbox.read_file_text("/tmp/test.py")

        # Set environment variables
        await sandbox.set_env({"MY_VAR": "value"})

        # Set working directory
        await sandbox.set_cwd("/tmp")

        # Clean up
        await sandbox.destroy()

asyncio.run(main())
```

## API Reference

### OpenSandbox

The main client class for connecting to an OpenSandbox server.

```python
client = OpenSandbox(
    base_url="https://opensandbox.fly.dev",  # Server URL
    grpc_port=50051,                          # gRPC port (default: 50051)
    timeout=30.0,                              # HTTP timeout in seconds
)
```

### Sandbox

A sandbox session for executing commands and managing files.

#### run(command, *, timeout_ms=300000, mem_kb=2097152, env=None, cwd=None)

Execute a shell command in the sandbox.

```python
result = await sandbox.run("ls -la")
print(result.stdout)
print(result.stderr)
print(result.exit_code)
print(result.success)  # True if exit_code == 0
```

#### write_file(path, content)

Write content to a file in the sandbox.

```python
await sandbox.write_file("/tmp/script.py", "print('hello')")
await sandbox.write_file("/tmp/data.bin", b"\x00\x01\x02")  # bytes
```

#### read_file(path) / read_file_text(path)

Read a file from the sandbox.

```python
content = await sandbox.read_file("/tmp/data.bin")  # bytes
text = await sandbox.read_file_text("/tmp/script.py")  # str
```

#### set_env(env)

Set environment variables for subsequent commands.

```python
await sandbox.set_env({"PATH": "/usr/bin:/bin", "MY_VAR": "value"})
```

#### set_cwd(cwd)

Set the working directory for subsequent commands.

```python
await sandbox.set_cwd("/home/user")
```

#### destroy()

Destroy the sandbox and clean up resources.

```python
await sandbox.destroy()
```

## Context Manager Support

Both `OpenSandbox` and `Sandbox` support async context managers:

```python
async with OpenSandbox("https://opensandbox.fly.dev") as client:
    async with await client.create() as sandbox:
        result = await sandbox.run("echo hello")
        # sandbox.destroy() called automatically
    # client.close() called automatically
```

## Error Handling

```python
from opensandbox import (
    OpenSandboxError,
    SandboxNotFoundError,
    SandboxConnectionError,
    CommandExecutionError,
    FileOperationError,
)

try:
    result = await sandbox.run("invalid-command")
except CommandExecutionError as e:
    print(f"Command failed: {e}")
    print(f"Exit code: {e.exit_code}")
    print(f"Stderr: {e.stderr}")
```

## Architecture

The SDK uses a hybrid approach:
- **HTTP** for sandbox lifecycle (create/destroy) - reliable and works through all proxies
- **gRPC** for fast operations (commands, file I/O) - low latency, binary protocol

This gives you the best of both worlds: reliable session management and fast command execution.

## License

MIT
