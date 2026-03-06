# OpenComputer

Long-running cloud infrastructure for AI agents. Real computers, not sandboxes.

Every OpenComputer is a real VM - a real computer with a real filesystem, full OS access, and persistent state. Not a microVM, not a container. A full Linux machine with root access.

Think of it as the compute equivalent of a laptop that sleeps when you close the lid and is right where you left off when you open it. Except it's in the cloud, it scales to thousands, and you're not paying for it while it's asleep.

## Features

- **Persistent VMs** - Hibernate/wake instead of timeouts. Your VM sleeps when idle and wakes in seconds, right where you left off.
- **Checkpoints** - Instant snapshots. Fork or restore to any point. Break something, roll back in a second.
- **Preview URLs** - Expose ports externally with auth (Clerk) and custom domains. Give every environment a live URL.
- **Per-tenant package control** - Manage and hot-swap software versions inside running VMs. Every tenant gets exactly the stack they need.

## Quick start

### CLI

Download the latest `oc` binary from [GitHub Releases](https://github.com/diggerhq/opencomputer/releases):

```bash
# macOS (Apple Silicon)
curl -fsSL https://github.com/diggerhq/opencomputer/releases/latest/download/oc-darwin-arm64 -o /usr/local/bin/oc
chmod +x /usr/local/bin/oc

# Configure
oc config set api-key YOUR_API_KEY
```

### SDK

Install the SDK:

```bash
npm install @opencomputer/sdk
# or
pip install opencomputer-sdk
```

```typescript
import { Sandbox } from '@opencomputer/sdk';

// Create a sandbox
const sandbox = await Sandbox.create({ template: 'default' });

// Run a command
const result = await sandbox.commands.run('node --version');
console.log(result.stdout);

// Work with files
await sandbox.files.write('/app/index.js', 'console.log("hello")');
const output = await sandbox.commands.run('node /app/index.js');
console.log(output.stdout); // hello

// Clean up
await sandbox.kill();
```

## Documentation

https://docs.opencomputer.dev/

## Website

https://opencomputer.dev/
