# opensandbox-sdk

Python SDK for [OpenSandbox](https://github.com/diggerhq/opensandbox) — an open-source, E2B-compatible sandbox platform.

## Install

```bash
pip install opensandbox-sdk
```

## Quick Start

```python
import asyncio
from opensandbox import Sandbox

async def main():
    sandbox = await Sandbox.create(template="base")

    # Execute commands
    result = await sandbox.commands.run("echo hello")
    print(result.stdout)  # "hello\n"

    # Read and write files
    await sandbox.files.write("/tmp/test.txt", "Hello, world!")
    content = await sandbox.files.read("/tmp/test.txt")

    # Clean up
    await sandbox.kill()
    await sandbox.close()

asyncio.run(main())
```

## Configuration

| Parameter  | Env Variable            | Default                 |
|------------|------------------------|-------------------------|
| `api_url`  | `OPENSANDBOX_API_URL`  | `https://app.opencomputer.dev` |
| `api_key`  | `OPENSANDBOX_API_KEY`  | (none)                  |

## License

MIT
