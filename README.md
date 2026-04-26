[![npm](https://img.shields.io/npm/v/@opencomputer/sdk)](https://www.npmjs.com/package/@opencomputer/sdk)
[![PyPI](https://img.shields.io/pypi/v/opencomputer-sdk)](https://pypi.org/project/opencomputer-sdk/)
[![GitHub stars](https://img.shields.io/github/stars/diggerhq/opencomputer)](https://github.com/diggerhq/opencomputer)

# OpenComputer

Long-running cloud infrastructure for AI agents. Real computers, not sandboxes.

Every OpenComputer is a real VM - a real computer with a real filesystem, full OS access, and persistent state. Not a container. A full Linux machine with root access.

Think of it as the compute equivalent of a laptop that sleeps when you close the lid and is right where you left off when you open it. Except it's in the cloud, it scales to thousands, and you're not paying for it while it's asleep.

## Features

- **Persistent VMs** - Hibernate/wake instead of timeouts. Your VM sleeps when idle and wakes in seconds, right where you left off.
- **Checkpoints** - Instant snapshots. Fork or restore to any point. Break something, roll back in a second.
- **Preview URLs** - Expose ports externally with auth (Clerk) and custom domains. Give every environment a live URL.
- **Per-tenant package control** - Manage and hot-swap software versions inside running VMs. Every tenant gets exactly the stack they need.
- **Claude Agent SDK** - Optimised for Claude Agent SDK workloads, with higher-level primitives for streaming.

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
npm install @opencomputer/sdk      # TypeScript
pip install opencomputer-sdk        # Python
cargo add opencomputer tokio --features tokio/full   # Rust
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

```rust
use opencomputer::{RunOpts, Sandbox, SandboxOpts};

#[tokio::main]
async fn main() -> opencomputer::Result<()> {
    let sandbox = Sandbox::create(SandboxOpts::new().template("default")).await?;

    let result = sandbox
        .commands()
        .run("node --version", RunOpts::new())
        .await?;
    println!("{}", result.stdout);

    sandbox
        .files()
        .write("/app/index.js", "console.log(\"hello\")")
        .await?;
    let output = sandbox
        .commands()
        .run("node /app/index.js", RunOpts::new())
        .await?;
    println!("{}", output.stdout); // hello

    sandbox.kill().await?;
    Ok(())
}
```

### Agent SDK

Run a full Claude agent session inside the VM with real-time event streaming:

```typescript
import { Sandbox } from '@opencomputer/sdk';

const sandbox = await Sandbox.create({
  template: 'default',
  apiKey: 'YOUR_API_KEY',
  envs: { ANTHROPIC_API_KEY: 'YOUR_ANTHROPIC_KEY' },
});

// Start a Claude agent session inside the sandbox
const session = await sandbox.agent.start({
  prompt: 'Create a todo app with React',
  systemPrompt: 'You are a senior fullstack developer...',
  maxTurns: 30,
  cwd: '/workspace',
  onEvent: (event) => {
    switch (event.type) {
      case 'assistant':
        console.log('Agent:', event.message?.content);
        break;
      case 'turn_complete':
        console.log('Done!');
        break;
      case 'error':
        console.error(event.message);
        break;
    }
  },
});

// Get a live preview URL
const preview = await sandbox.createPreviewURL({ port: 80 });
console.log('Preview:', preview);
```

## How it works

OpenComputer gives each agent a full Linux VM (not a container). The agent loop runs *inside* the VM alongside the filesystem and preview server — no network hops between your agent and the code it's writing.

```
┌─────────────────────────────────┐
│        OpenComputer VM          │
│                                 │
│  ┌───────────────────────────┐  │
│  │   Claude Agent SDK        │  │
│  │   (agent loop + tools)    │  │
│  └─────────┬─────────────────┘  │
│            │                    │
│  ┌─────────▼──┐  ┌───────────┐  │
│  │ Filesystem │  │  Preview   │  │
│  │ /workspace │  │  Server    │  │
│  └────────────┘  └───────────┘  │
│                                 │
│  Hibernates when idle           │
│  Wakes in seconds               │
└─────────────────────────────────┘
```

VMs hibernate instead of dying. State survives across sessions without manual snapshot/restore. No more re-installing node_modules because the container timed out.

## Why not containers?

|                | Ephemeral sandboxes              | OpenComputer                        |
| -------------- | -------------------------------- | ----------------------------------- |
| Persistence    | Starts from scratch every time   | Hibernates and resumes              |
| Runtime        | Containers with time limits      | Full VMs, no timeouts               |
| Agent loop     | Runs externally, talks over network | Runs inside the VM               |
| File I/O       | Network round-trips              | Local, instant                      |
| State          | Lost on timeout                  | Survives across sessions            |

## Guides

- [Building an Open Lovable - part 1](https://opencomputer.dev/guides/building-open-lovable-part-1) — Build a Lovable clone using Claude Agent SDK and OpenComputer

## Links

- [Documentation](https://docs.opencomputer.dev/)
- [Website](https://opencomputer.dev/)
- [Guides](https://opencomputer.dev/guides)
- [Talk to founders](https://cal.com/team/digger/opencomputer-founder-chat)

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.
