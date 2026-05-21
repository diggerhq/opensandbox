//! Mirrors `examples/test.py` and `examples/test.ts`.
//!
//! Run with:
//!   OPENCOMPUTER_API_KEY=... cargo run --bin test

use opencomputer::{RunOpts, Sandbox, SandboxOpts};

#[tokio::main]
async fn main() -> opencomputer::Result<()> {
    println!("Creating sandbox...");
    let sb = Sandbox::create(
        SandboxOpts::new()
            .template("base")
            .timeout(3600)
            .api_url("https://app.opencomputer.dev"),
    )
    .await?;
    println!("Sandbox created: {}", sb.sandbox_id());

    // Run commands
    println!("\n--- Commands ---");
    let result = sb
        .commands()
        .run("echo hello from rust sdk", RunOpts::new())
        .await?;
    println!("stdout: {}", result.stdout.trim());
    println!("exit code: {}", result.exit_code);

    let uname = sb.commands().run("uname -a", RunOpts::new()).await?;
    println!("uname: {}", uname.stdout.trim());

    // Filesystem
    println!("\n--- Filesystem ---");
    sb.files()
        .write("/tmp/greeting.txt", "Hello from Rust SDK!")
        .await?;
    let content = sb.files().read("/tmp/greeting.txt").await?;
    println!("file content: {}", content);

    let exists = sb.files().exists("/tmp/greeting.txt").await;
    println!("file exists: {}", exists);

    sb.files().make_dir("/tmp/mydir").await?;
    sb.files()
        .write("/tmp/mydir/test.py", "print(\"hello from python\")")
        .await?;

    let entries = sb.files().list("/tmp").await?;
    println!("ls /tmp:");
    for entry in entries {
        let kind = if entry.is_dir { "d" } else { "-" };
        println!("  {} {}", kind, entry.name);
    }

    // Run a multi-line script
    println!("\n--- Script execution ---");
    let script_body = [
        "#!/bin/bash",
        "echo \"Current directory: $(pwd)\"",
        "echo \"User: $(whoami)\"",
        "echo \"Date: $(date)\"",
        "echo \"Files in /tmp:\"",
        "ls /tmp",
    ]
    .join("\n");
    sb.files().write("/tmp/script.sh", script_body).await?;

    let script = sb
        .commands()
        .run("bash /tmp/script.sh", RunOpts::new())
        .await?;
    println!("{}", script.stdout);

    // Check sandbox status
    println!("--- Status ---");
    let running = sb.is_running().await;
    println!("running: {}", running);

    // Clean up
    sb.kill().await?;
    println!("\nSandbox killed. Done!");
    Ok(())
}
