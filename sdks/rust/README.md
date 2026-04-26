# opencomputer

Rust SDK for [OpenComputer](https://github.com/diggerhq/opencomputer) — cloud sandbox platform.

## Install

```toml
[dependencies]
opencomputer = "0.1"
tokio = { version = "1", features = ["full"] }
```

## Quick Start

```rust
use opencomputer::{RunOpts, Sandbox, SandboxOpts};

#[tokio::main]
async fn main() -> opencomputer::Result<()> {
    let sandbox = Sandbox::create(SandboxOpts::new().template("base")).await?;

    // Execute commands
    let result = sandbox.commands().run("echo hello", RunOpts::new()).await?;
    println!("{}", result.stdout); // "hello\n"

    // Read and write files
    sandbox
        .files()
        .write("/tmp/test.txt", "Hello, world!")
        .await?;
    let _content = sandbox.files().read("/tmp/test.txt").await?;

    // Clean up
    sandbox.kill().await?;
    Ok(())
}
```

## Configuration

| Builder method | Env Variable           | Default                          |
|----------------|------------------------|----------------------------------|
| `.api_url(..)` | `OPENCOMPUTER_API_URL` | `https://app.opencomputer.dev`   |
| `.api_key(..)` | `OPENCOMPUTER_API_KEY` | (none)                           |

## Streaming output

```rust
use opencomputer::{ExecStartOpts, StreamEvent};

let (session, mut events) = sandbox
    .exec()
    .start("node", ExecStartOpts::new().args(vec!["server.js".into()]))
    .await?;

while let Some(ev) = events.recv().await {
    match ev {
        StreamEvent::Stdout(b) => print!("{}", String::from_utf8_lossy(&b)),
        StreamEvent::Stderr(b) => eprint!("{}", String::from_utf8_lossy(&b)),
        StreamEvent::Exit(code) => {
            println!("exited: {code}");
            break;
        }
        StreamEvent::ScrollbackEnd => {}
    }
}

let _ = session.done().await;
```

## Examples

The `examples/` directory mirrors the test scripts in the Python and TypeScript SDKs:

```bash
export OPENCOMPUTER_API_KEY=osb_...
export OPENCOMPUTER_API_URL=https://app.opencomputer.dev   # or your self-hosted URL

cargo run --example test_commands
cargo run --example test_file_ops
cargo run --example test_python_sdk
```

Each example creates a fresh sandbox, runs an end-to-end suite, and tears the
sandbox down. They exit non-zero if any check fails, so they double as
integration tests.

## Offline tests

```bash
cargo test
```

These tests verify the public API surface and JSON deserialization without
contacting the backend.

## License

MIT
